# CSI Driver Contract

ShiftPV의 Kubernetes storage interface는 `csi.shiftpv.io` CSI driver다. 제품
기반은 [ADR 0003](../adr/0003-csi-product-foundation.md), 지원 범위는
[ADR 0004](../adr/0004-minimal-csi-bootstrap.md)를 따른다.

## Initial deployment

```text
Controller Deployment
├── shiftpv-controller       Identity + Controller service
├── csi-provisioner
└── liveness-probe

Node Plugin DaemonSet (participating worker마다 1개)
├── shiftpv-node             Identity + Node service + bind mount gate
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
2. external-provisioner가 selected topology를 포함해 `CreateVolume`을 호출한다.
3. Controller는 request name의 SHA-256으로 안정적인 volume ID를 만든다.
4. Controller namespace의 `<volume-id>` ConfigMap을 idempotent reservation으로
   생성한다. request name, node, capacity가 기존 값과 다르면 `AlreadyExists`다.
5. Controller는 selected node에 helper Pod를 띄워
   `<poolRoot>/volumes/<volume-id>`를 `mkdir -p`한다. hostPath type은 `Directory`라
   pool root가 없으면 자동 생성하지 않고 실패한다.
6. 성공 응답은 owner node를 volume context와 accessible topology에 담는다.
7. external-provisioner가 그 topology를 PV node affinity로 변환한다.

Controller는 node-local path에 직접 접근하지 않는다. reservation ConfigMap은
Helm resource가 아니며 같은 namespace 재설치 후에도 남는다.

## Publish gate

`NodePublishVolume`은 RWO Filesystem, writable publish, 안전한 volume ID, kubelet
pods 아래 target path인지 확인한다. CSI volume context의 owner node가 현재
node와 같고 canonical source directory가 실제로 있을 때만 bind mount한다.

현재 publish gate의 authoritative placement 값은 CSI volume context의 owner
node다. 별도 PV annotation이나 외부 상태를 조회하지 않는다.

## Idempotency and deletion

- 같은 `CreateVolume` 재시도는 ConfigMap 값이 같을 때 같은 ID/topology를 반환한다.
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
- `DeleteVolume`은 reservation의 owner node에서 directory를 제거한 다음
  reservation을 제거한다. 존재하지 않는 volume은 성공한다.
- 제공 chart의 StorageClass는 `Retain` 고정이므로 PVC/PV 삭제 경로에서
  `DeleteVolume`은 자동 호출되지 않는다.

## Validation

- `make verify`의 race test, 80% coverage gate, vet, build와 Helm 검사 통과
- 서로 다른 host directory를 가진 2-worker kind에서 ShiftPV를 기본
  StorageClass로 지정
- `storageClassName` 없는 PVC가 `shiftpv`를 선택하고 CSI PV에 Bound
- Pod가 volume을 mount해 데이터를 쓰고 재생성 후 동일 checksum 확인
- workload 중지 후 Helm uninstall 시 PVC/PV/reservation/data 유지
- 같은 namespace/poolRoot에 재설치하고 Pod를 다시 만들면 checksum 유지
- worker pool의 실제 ENOSPC/read-only 장애에서 `Unavailable`, 상태 보존과 복구 후
  재시도 수렴 확인
