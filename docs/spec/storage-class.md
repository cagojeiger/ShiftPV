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

사용자가 설정하는 유일한 parameter는 `shiftpv.io/capacity-enforcement: none`이다.
external-provisioner가 내부적으로 추가하는 PVC/PV metadata parameter를 제외한 알 수 없는
parameter와 다른 값은 `InvalidArgument`로 거부한다. requested capacity는 PV capacity와
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
않는다. chart의 `pre-delete` guard는 먼저 CSI provisioning을 quiesce하고 이미 실행 중인
`CreateVolume`이 끝났다는 Controller acknowledgement를 기다린다. 그 뒤 ShiftPV PV,
ShiftPV StorageClass를 참조하는 PVC,
모든 `ShiftPVVolume`, 진행 중인 `ShiftPVMove`가 하나라도 남아 있으면 uninstall을
거부한다. Kubernetes API나 CR 조회 실패도 fail-closed로 거부한다. 거부된 경우
quiesce attempt가 취소되고 Controller가 provisioning과 lifecycle validation reconcile을
재개하며, Controller, Node Plugin, RBAC와 StorageClass는 그대로 유지된다.

정상 제거는 workload를 중지하고 move를 terminal phase로 수렴시킨 다음 PVC/PV와
ShiftPVVolume metadata를 명시적으로 정리한 뒤 수행한다. `Retain`이므로 PVC/PV
metadata 정리만으로 host data directory를 자동 삭제하지 않는다.

보존된 PVC/PV/CR과 data를 나중에 같은 설정으로 복구해야 하는 경우에는 운영자가
위험을 인수하고 lifecycle `ValidatingWebhookConfiguration`을 먼저 삭제한 뒤
`helm uninstall shiftpv --namespace shiftpv-system --no-hooks`로 guard를 우회할 수 있다.
이 경로에서는 PVC/PV, reservation ConfigMap, ShiftPV CR과 host data가 남지만 driver가
없는 동안 새 mount와 Pod 재시작은 실패한다. 같은 namespace와 동일한 Pool 등록으로
reinstall한 후 workload를 재시작해야 한다.

Argo CD가 release를 소유하면 Helm values에 `lifecycle.uninstallMode=argocd`를 명시한다.
이때 Job은 Argo CD 3.3+ `PreDelete` hook으로 실행되며 blocker가 있는 동안 5초 간격으로
독립된 quiesce attempt를 반복한다. blocker가 제거되면 실행 중인 Job이 안전 검사를
통과하여 같은 Application 삭제를 완료한다. 기본 `helm` mode는 dependency가 있으면
즉시 실패한다. lifecycle validation webhook은 hook 경쟁이나 직접 Kubernetes
DELETE에서도 보호 label이 붙은 driver resource의 제거를 거부한다. admission handler는
상태를 변경하지 않으며 guard가 quiesce와 검사를 끝내 생성한 유효 permit만 승인한다.
일반 sync의 prune은 `PreDelete` 단계가 아니므로 ShiftPV는 별도 Application으로 관리하고
제거는 Application 삭제 절차로 제한해야 한다. 이 경로는 Argo CD 3.5.2를 사용한 별도
kind E2E에서 삭제 허용, 대기와 blocker 해소 후 자동 수렴까지 검증한다.

PVC를 삭제해도 `Retain` PV는 `Released`가 되고 data directory는 남는다. 현재
버전은 이를 자동 reclaim하지 않는다. `DeleteVolume`을 호출하는 별도 Delete-policy
StorageClass는 제공 chart와 지원 범위 밖이다.
