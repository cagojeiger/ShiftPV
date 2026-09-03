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
4. Controller namespace의 `<volume-id>` ConfigMap을 idempotent reservation으로
   생성한다. request name, node, capacity가 기존 값과 다르면 `AlreadyExists`다.
5. Controller는 selected node의 `ShiftPVPool.spec.mountPath`를 조회하고 helper Pod를 띄워
   `<mountPath>/volumes/<volume-id>`를 `mkdir -p`한다. hostPath type은 `Directory`라
   등록 path가 없으면 자동 생성하지 않고 실패한다.
6. Controller는 `ShiftPVVolume`을 만들고 selected node를 최초 authoritative owner로
   기록한다. selected node에 해당하는 `ShiftPVPool` 등록이 없으면 provisioning을
   거부한다.
7. mobility opt-in namespace면 성공 응답에 생성 시점의 registered Pool node 전체를,
   아니면 selected owner node 하나만 accessible topology로 담는다. PVC namespace metadata가
   없을 때도 안전하게 owner-only를 선택한다. volume context의 node는 최초 배치 기록일 뿐
   이동 후 권한의 source of truth가 아니다.
8. external-provisioner가 이 topology를 PV node affinity로 변환한다.

Controller는 node-local path에 직접 접근하지 않는다. reservation ConfigMap은
Helm resource가 아니며 같은 namespace 재설치 후에도 남는다.

## Publish authorization

`NodePublishVolume`은 RWO Filesystem, writable publish, 안전한 volume ID, kubelet
pods 아래 target path인지 확인한다. 이어 `ShiftPVVolume.status`를 조회해 phase가
`Ready`이고 owner가 현재 node이며 canonical source directory가 실제로 있을 때만 bind
mount한다. 상태 조회 실패, `Moving`, `Blocked`, owner 불일치는 fail-closed다.

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

## Idempotency and deletion

- 같은 `CreateVolume` 재시도는 ConfigMap과 `ShiftPVVolume`의 최초 owner가 같을 때 같은
  ID와 namespace opt-in 규칙에 따른 topology를 반환한다.
- Kubernetes API의 timeout, server unavailable, throttling은 `Unavailable`로
  반환한다. 호출 context의 deadline/cancellation은 해당 gRPC code를 유지한다.
- reservation 생성이나 삭제의 응답이 유실되어 실제 반영 여부가 모호해도 다음
  CSI 재시도는 현재 ConfigMap 상태를 읽어 동일 결과로 수렴한다.
- helper Pod가 directory 작업 중 실패하면 외부 파일시스템의 일시 장애로 취급해
  `Unavailable`을 반환한다. Create 실패는 reservation을, Delete 실패는
  reservation과 directory를 보존해 다음 CSI 재시도가 같은 상태에서 계속된다.
- 제공 chart는 Controller replica를 1개로 고정한다. Controller는 같은 volume
  ID의 Create/Delete lifecycle을 직렬화하고 서로 다른 volume ID는 병렬 처리한다.
- 이미 올바르게 mount된 target publish와 이미 unmount된 unpublish는 성공한다.
- `DeleteVolume`은 dynamic owner node에서 directory를 제거한 다음 reservation과
  `ShiftPVVolume`을 제거한다. phase가 `Ready`가 아니거나 `activeMove`가 남아 있으면
  data 삭제를 거부한다. 존재하지 않는 reservation은 성공한다.
- 제공 chart의 StorageClass는 `Retain` 고정이므로 PVC/PV 삭제 경로에서
  `DeleteVolume`은 자동 호출되지 않는다.

## Validation

- `make verify`의 race test, 80% coverage gate, vet, build와 Helm 검사 통과
- 서로 다른 host directory를 가진 2-worker kind에서 ShiftPV를 기본
  StorageClass로 지정
- `storageClassName` 없는 PVC가 `shiftpv`를 선택하고 CSI PV에 Bound
- Pod가 volume을 mount해 데이터를 쓰고 재생성 후 동일 checksum 확인
- workload 중지 후 Helm uninstall 시 PVC/PV/reservation/data 유지
- 같은 namespace와 Pool 등록으로 재설치하고 Pod를 다시 만들면 checksum 유지
- worker pool의 실제 ENOSPC/read-only 장애에서 `Unavailable`, 상태 보존과 복구 후
  재시도 수렴 확인
- source-only scheduling constraint는 source authority를 보존한 `Blocked`로 종료
- 정상 cordon 이동은 `Copying`/`Committing` 중 Controller 강제 재시작 후에도 동일
  transaction과 PVC/PV/checksum을 유지하며 `Succeeded`로 수렴
