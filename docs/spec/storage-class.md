# StorageClass Contract

ShiftPV StorageClass는 참여 node의 고정 pool root에서 directory-backed volume을
provision한다.

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: shiftpv
provisioner: csi.shiftpv.io
reclaimPolicy: Retain
allowVolumeExpansion: false
volumeBindingMode: WaitForFirstConsumer
parameters:
  shiftpv.io/capacity-enforcement: none
```

| 항목 | 값 | 이유 |
|------|---------|------|
| `provisioner` | `csi.shiftpv.io` | ShiftPV CSI 식별자 |
| binding | `WaitForFirstConsumer` | workload node를 먼저 선택 |
| reclaim | `Retain` | 자동 데이터 삭제 방지 |
| expansion | `false` | resize를 지원하지 않음 |
| access/volume mode | RWO Filesystem만 | node-local directory volume |

유일한 parameter는 `shiftpv.io/capacity-enforcement: none`이다. 누락, 알 수 없는
parameter, 다른 값은 `InvalidArgument`로 거부한다. requested capacity는 PV capacity와
reservation의 idempotency 비교에 쓰지만 directory write를 제한하지 않는다.

Helm의 `storageClass.defaultClass`를 `true`로 설정하면 chart가
`storageclass.kubernetes.io/is-default-class: "true"` annotation을 추가한다. 그러면
`storageClassName`을 생략한 새 PVC도 Kubernetes admission에 의해 `shiftpv`를 선택하며,
kind E2E는 이 PVC를 사용하는 Pod가 실제로 mount하고 데이터를 쓰는 과정까지 검증한다.
기존 기본 StorageClass가 있는 cluster에서는 동시에 둘을 기본값으로 두지 않아야 한다.
기본값인 `false`로 설치하면 chart는 기존 StorageClass의 annotation을 변경하지 않으며,
workload는 `storageClassName: shiftpv`로 ShiftPV를 명시적으로 선택할 수 있다.

## Helm lifecycle

Helm은 StorageClass를 소유하지만 이 StorageClass로 만들어진 PVC/PV는 소유하지
않는다. uninstall 전에 사용 Pod를 중지해야 하며 uninstall 중에는 새 mount나 Pod
재시작이 불가능하다. PVC/PV, reservation ConfigMap, host data directory는 남는다.
같은 namespace와 pool root로 reinstall하면 retained PV를 다시 publish할 수 있다.

PVC를 삭제해도 `Retain` PV는 `Released`가 되고 data directory는 남는다. 현재
버전은 이를 자동 reclaim하지 않는다. `DeleteVolume`을 호출하는 별도 Delete-policy
StorageClass는 제공 chart와 지원 범위 밖이다.
