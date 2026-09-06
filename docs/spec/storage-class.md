# StorageClass Contract

ShiftPV StorageClass는 참여 node의 등록된 Pool mount path에서 directory-backed volume을
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

사용자가 설정하는 StorageClass parameter는 없다. `shiftpv.io/capacity-enforcement: none`은
기존 StorageClass의 불변 필드를 유지하는 호환성 marker이며 동작을 선택하는 설정이 아니다.
external-provisioner가 내부적으로 추가하는 PVC/PV metadata parameter와 이 marker를 제외한
알 수 없는 parameter 또는 다른 marker 값은 `InvalidArgument`로 거부한다.

requested capacity는 PV capacity와 reservation idempotency뿐 아니라 owner Pool의 총예약
입장 판단에 쓰인다. Controller는 `ShiftPVPool.spec.capacity.limit`에서 현재 총예약을 뺀 값과
Pool filesystem의 현재 available bytes를 각각 확인한다. 둘 중 하나보다 요청량이 크면 신규
provisioning을 거부한다. 이 검사는 개별 directory write를 제한하는 quota가 아니다.

Helm의 `storageClass.defaultClass`를 `true`로 설정하면 chart가
`storageclass.kubernetes.io/is-default-class: "true"` annotation을 추가한다. 그러면
`storageClassName`을 생략한 새 PVC도 Kubernetes admission에 의해 `shiftpv`를 선택한다.
기존 기본 StorageClass가 있는 cluster에서는 동시에 둘을 기본값으로 두지 않아야 한다.
기본값인 `false`로 설치하면 chart는 기존 StorageClass의 annotation을 변경하지 않으며,
workload는 `storageClassName: shiftpv`로 ShiftPV를 명시적으로 선택할 수 있다.

## Helm lifecycle

Helm은 StorageClass를 소유하지만 이 StorageClass로 생성한 PVC/PV와 host data는 소유하지
않는다. `Retain`이므로 PVC를 삭제해도 PV와 data directory가 자동 삭제되지 않는다.
ShiftPV dependency가 남은 정상 uninstall은 fail-closed로 거부한다.

Helm과 Argo CD의 제거, emergency bypass와 재설치 절차는
[chart 운영 문서](../../charts/shiftpv/README.md#uninstall-and-recovery)를 따른다.
