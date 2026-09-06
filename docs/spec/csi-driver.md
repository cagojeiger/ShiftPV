# CSI Driver Contract

ShiftPV의 Kubernetes storage interface는 `csi.shiftpv.io` CSI driver다. 제품
기반은 [ADR 0003](../adr/0003-csi-product-foundation.md), 지원 범위는
[ADR 0004](../adr/0004-minimal-csi-bootstrap.md)를 따른다.

## Initial deployment

```text
Controller Deployment
├── shiftpv-controller       Identity + Controller service + mobility reconcile/admission
├── csi-provisioner
└── liveness-probe

Node Plugin DaemonSet (participating worker마다 1개)
├── shiftpv-node             Identity + Node service + bind mount authorization
├── node-driver-registrar
└── liveness-probe
```

`external-attacher`, `external-resizer`, `external-snapshotter`는 배포하지 않는다.

| 항목 | 값 |
|------|----|
| driver name | `csi.shiftpv.io` |
| controller socket | `/run/csi/csi.sock` |
| node socket | `/var/lib/kubelet/plugins/csi.shiftpv.io/csi.sock` |
| topology key | `topology.csi.shiftpv.io/node` |
| attach required | `false` |
| lifecycle mode | `Persistent` |

`NodeGetInfo`의 node ID와 topology value는 Kubernetes node name과 같다.

## RPC surface

- Identity: `GetPluginInfo`, `GetPluginCapabilities`, `Probe`
- Controller: `CreateVolume`, `DeleteVolume`, `ControllerGetCapabilities`,
  `ValidateVolumeCapabilities`
- Node: `NodeGetInfo`, `NodeGetCapabilities`, `NodePublishVolume`,
  `NodeUnpublishVolume`

광고하지 않은 RPC는 구현하지 않는다. `STAGE_UNSTAGE_VOLUME`, expansion,
stats, snapshot, attach capability는 광고하지 않는다.

## Provisioning

1. StorageClass의 `WaitForFirstConsumer`가 workload node를 고른다.
2. external-provisioner가 selected topology와 PVC/PV metadata를 포함해 `CreateVolume`을
   호출한다.
3. Controller는 request name의 SHA-256으로 안정적인 volume ID를 만든다.
4. Controller는 selected node의 `ShiftPVPool.spec.capacity.limit`와 현재 owner 기준
   reservation 합계를 확인한다. 예약 ConfigMap의 capacity가 크기이고, 대응하는
   `ShiftPVVolume.status.ownerNode`가 있으면 최초 node 대신 현재 owner에 합산한다. Volume이
   아직 없는 create 중간 상태만 reservation의 최초 node에 합산한다.
5. Controller는 node-bound helper Pod로 Pool mount의 `statfs`를 읽는다. requested bytes가
   `limit - reserved` 또는 `Bavail * Frsize`보다 크면 directory를 만들기 전에
   `ResourceExhausted`로 거부한다. Pool 설정이나 측정 결과가 불명확해도 fail-closed다.
6. Controller namespace의 `<volume-id>` ConfigMap을 idempotent reservation으로
   생성한다. request name, node, capacity가 기존 값과 다르면 `AlreadyExists`다. 동일한 기존
   reservation은 용량을 다시 차감하지 않고 중단된 create를 계속한다.
7. Controller는 selected node의 `ShiftPVPool.spec.mountPath`를 조회하고 helper Pod를 띄워
   `<mountPath>/volumes/<volume-id>`를 `mkdir -p`한다. hostPath type은 `Directory`라
   등록 path가 없으면 자동 생성하지 않고 실패한다.
8. Controller는 `ShiftPVVolume`을 만들고 selected node를 최초 authoritative owner로
   기록한다. selected node에 해당하는 `ShiftPVPool` 등록이 없으면 provisioning을
   거부한다.
9. mobility opt-in namespace면 성공 응답에 생성 시점의 registered Pool node 전체를,
   아니면 selected owner node 하나만 accessible topology로 담는다. PVC namespace metadata가
   없을 때도 안전하게 owner-only를 선택한다. volume context의 node는 최초 배치 기록일 뿐
   이동 후 권한의 source of truth가 아니다.
10. external-provisioner가 이 topology를 PV node affinity로 변환한다.

Controller는 node-local path에 직접 접근하지 않는다. statfs와 directory 생성은 등록 Pool을
mount한 일회성 helper Pod가 수행한다. reservation ConfigMap은 Helm resource가 아니며 같은
namespace 재설치 후에도 남는다.

## Publish authorization

`NodePublishVolume`은 RWO Filesystem, writable publish, 안전한 volume ID, kubelet
pods 아래 target path인지 확인한다. 이어 `ShiftPVVolume.status`를 조회해 phase가
`Ready`이고 owner가 현재 node이며 canonical source directory가 실제로 있을 때만 bind
mount한다. 상태 조회 실패, `Moving`, `Blocked`, owner 불일치는 즉시 fail-closed다.
`Moving`에서 CSI 호출을 장시간 유지하지 않는다. Controller가 workload를 scheduling gate
아래 두고 destination data promotion과 owner commit 뒤에 release하므로 정상 이동의 publish는
`Ready` owner에서 재시도된다. 이 경계의 근거는
[node publish 검증](../validation/node-publish-wait-rejection-2026-09-05.md)에 기록한다.

동시에 들어오는 `NodePublishVolume`도 요청마다 현재 `ShiftPVVolume`을 한 번 읽고 독립적으로
승인 또는 즉시 거부한다. CSI 요청별 polling goroutine이나 Kubernetes watch는 만들지 않는다.

successful publish/unpublish는 `publishedNodes`를 갱신한다. 이 값은 이동 전 실제
unpublish와 이동 후 publish를 확인하는 관찰값이며, owner 권한을 대신하지 않는다.

Node Plugin은 node마다 다른 Pool path를 지원하기 위해 privileged DaemonSet 안의 `/host`에
host root를 mount하고, 현재 node의 immutable `ShiftPVPool.spec.mountPath`를 그 아래에서
해석한다. `/`와 상대 path, 누락 또는 중복 node 등록은 fail-closed다. 따라서 Pool CR
쓰기 권한은 storage operator에게만 제한해야 한다.

`/host` mount는 `HostToContainer` propagation을 사용한다. Node Plugin이 재시작될 때
기존 kubelet volume mount가 `/host`의 private mount namespace에 남으면 실제 target을
unmount한 뒤에도 디렉터리 제거가 `EBUSY`로 실패할 수 있기 때문이다. kubelet target
mount는 별도의 `Bidirectional` mount로 host에 unpublish를 전달하고, `/host`는 그
host-side mount/unmount 변화를 다시 수신한다.

## Performance boundary

정상 publish 뒤 애플리케이션 I/O는 node-local bind mount를 통해 등록된 filesystem으로
직접 전달된다. ShiftPV Controller, CSI sidecar와 network copy 경로는 정상 read/write에
참여하지 않는다. 따라서 정상 I/O의 throughput, latency, durability는 Pool filesystem,
underlying device, mount option, encryption과 workload I/O pattern의 특성이다. ShiftPV는
이 값에 대한 수치 SLO를 제공하지 않는다.

CSI lifecycle 성능은 데이터 경로와 별도로 판단한다.

- provisioning latency는 PVC/consumer 생성부터 WFFC scheduling, external-provisioner의
  `CreateVolume`, statfs helper, directory helper, PV bind와 첫 Pod Ready까지를 포함한다.
- republish latency는 새 Pod 생성부터 `NodePublishVolume`과 Pod Ready까지다. 기존 Pod의
  termination grace와 workload shutdown 시간은 별도 측정한다.
- Controller, Node Plugin과 sidecar의 chart 기본 resources는 비어 있다. Kubernetes가
  보장하는 request/limit가 필요하면 운영자가 `controller.resources`, `node.resources`,
  `sidecars.*.resources`를 명시해야 한다.
- 성능 결과에는 Kubernetes 버전, node CPU/memory, 실제 Pool mount/device/filesystem,
  dataset 크기와 file count, cache 조건, 동시 workload와 표본 수를 함께 기록한다.

측정된 성능은 제품 보장값이 아니라 해당 환경의 증거이며
[dated validation](../validation/home-public-chart-performance-2026-09-05.md)에 기록한다.

## Idempotency and deletion

- 같은 `CreateVolume` 재시도는 ConfigMap과 `ShiftPVVolume`의 최초 owner가 같을 때 같은
  ID와 namespace opt-in 규칙에 따른 topology를 반환한다.
- Kubernetes API의 timeout, server unavailable, throttling은 `Unavailable`로
  반환한다. 호출 context의 deadline/cancellation은 해당 gRPC code를 유지한다.
- reservation이나 `ShiftPVVolume` 생성·삭제의 응답이 유실되어 실제 반영 여부가 모호해도
  다음 CSI 재시도는 남은 두 기록을 각각 확인해 동일 결과로 수렴한다. reservation은 이미
  없지만 `ShiftPVVolume`이 남은 부분 삭제도 현재 owner에서 directory delete를 반복한 뒤
  Volume 상태를 제거한다.
- helper Pod가 directory 작업 중 실패하면 외부 파일시스템의 일시 장애로 취급해
  `Unavailable`을 반환한다. Create 실패는 reservation을, Delete 실패는
  reservation과 directory를 보존해 다음 CSI 재시도가 같은 상태에서 계속된다.
- 제공 chart는 Controller replica를 1개로 고정한다. Controller는 같은 volume
  ID의 Create/Delete lifecycle을 직렬화한다. 신규 reservation admission은 같은 Pool에서
  직렬화하고 서로 다른 Pool은 병렬 처리한다.
- 이미 올바르게 mount된 target publish와 이미 unmount된 unpublish는 성공한다.
- `DeleteVolume`은 dynamic owner node에서 directory를 제거한 다음 reservation과
  `ShiftPVVolume`을 제거한다. phase가 `Ready`가 아니거나 `activeMove`가 남아 있으면
  data 삭제를 거부한다. reservation과 `ShiftPVVolume`이 모두 없을 때만 이미 완료된
  삭제로 성공한다.
- 제공 chart의 StorageClass는 `Retain` 고정이므로 PVC/PV 삭제 경로에서
  `DeleteVolume`은 자동 호출되지 않는다.

Mobility는 source unpublish 뒤 실제 volume directory bytes를 한 번 측정하고, destination Pool의
총예약과 `statfs` 여유를 복사 전에 승인한다. 승인된 진행 중 Move는 destination의 논리·물리
예약으로 계산되며 결과는 Move status에 남는다. 공간 부족은 copy Job과 owner commit 전에
Move를 `Blocked`로 전환하고 source recovery를 허용한다.

검증 방법은 [development testing](../development/testing.md), 실행 결과는
[validation evidence](../validation/README.md)를 따른다.
