# 0006. 의존 storage가 남은 제거는 기본 거부한다

- 상태: Accepted
- 날짜: 2026-09-03

## Context

ShiftPV chart를 제거하면 Controller, Node Plugin, RBAC, `CSIDriver`와 StorageClass가
사라지지만 Helm이 소유하지 않는 PVC/PV, ShiftPV CR과 host data는 남는다. 이 상태에서
기존 Pod가 재시작되면 volume을 다시 mount할 수 없으며 진행 중인 move의 닫힌 루프도
중단된다.

Kubernetes finalizer는 finalizer가 붙은 개별 object의 삭제만 지연한다. 따라서 PVC/PV
보호 finalizer만으로 ShiftPV driver 전체의 제거 순서를 보장할 수 없다.

## Decision

- chart는 빠른 진단을 위한 fail-closed `pre-delete` Job과 Kubernetes API 단계의
  lifecycle validation webhook을 항상 포함한다.
- Job은 다음 중 하나라도 존재하면 실패하여 제거를 거부한다.
  - CSI driver가 `csi.shiftpv.io`인 PV
  - chart에 설정된 ShiftPV StorageClass를 참조하는 PVC
  - 모든 `ShiftPVVolume`
  - phase가 `Succeeded` 또는 `Blocked`가 아닌 `ShiftPVMove`
- Kubernetes API 또는 ShiftPV CR 조회가 실패하거나 시간 제한을 넘으면 안전 여부를
  증명할 수 없으므로 제거를 거부한다.
- 같은 Job을 Helm `pre-delete`와 Argo CD 3.3+ `PreDelete` hook으로 선언한다. Argo CD의
  `PreDelete`는 전체 Application 삭제에만 적용하며 일반 sync prune을 가로채지 않는다.
- hook 실행과 별개로 lifecycle validation webhook은 보호 label이 붙은 ShiftPV
  Deployment, DaemonSet, Service, ServiceAccount, RBAC, StorageClass와 CSIDriver의 DELETE를
  같은 검사 규칙으로 승인하거나 거부한다. API 조회 오류도 거부한다.
- 이중 방어가 필요한 이유는 Argo CD 3.5.2의 동시 reconcile에서 새 PreDelete hook의
  `AlreadyExists`를 완료로 오판하여 resource finalizer 단계가 먼저 진행될 수 있기
  때문이다. webhook은 이 경쟁이 발생해도 driver resource가 실제로 삭제되지 않게 한다.
- guard는 검사를 시작하기 전에 현재 `CSIDriver` UID가 소유하는 5분 유효
  `quiescing` 상태를 기록한다. Controller는 이를 관찰하면 새 CSI `CreateVolume`을
  `Unavailable`로 거부하고, 이미 진입한 `CreateVolume`이 모두 끝난 뒤 같은 attempt에
  acknowledgement를 기록한다. 따라서 dependency snapshot과 새 PV/Volume 생성이
  겹치지 않는다.
- Controller의 certificate reconciler도 같은 quiesce gate를 사용한다. acknowledgement
  이후에는 lifecycle `ValidatingWebhookConfiguration`을 다시 만들지 않는다.
- acknowledgement를 받은 guard만 dependency를 검사한다. 안전하면 lifecycle
  `ValidatingWebhookConfiguration`을 제거하고 상태를 `granted`로 바꾼다. 이후 Helm이나
  Argo CD가 Service와 RBAC를 어떤 순서로 지워도 같은 teardown은 webhook endpoint
  가용성에 의존하지 않는다.
- lifecycle admission handler는 read-only다. 직접 DELETE나 dry-run 요청은 permit을
  만들거나 바꾸지 않으며, 현재 `CSIDriver` UID에 연결된 유효한 `granted` 상태만
  승인한다. 재설치로 UID가 바뀌면 이전 상태는 새 설치에 적용되지 않는다.
- guard가 검사, webhook 제거 또는 grant 중 실패하면 자기 attempt를 취소한다.
  Controller는 이를 관찰하여 provisioning과 validation reconcile을 재개한다.
- 운영자가 보존 metadata와 data의 복구 책임을 명시적으로 인수할 때는 lifecycle
  `ValidatingWebhookConfiguration`을 먼저 명시적으로 제거한 뒤 Helm `--no-hooks`를
  사용할 수 있다. 이 절차는 삭제 차단을 절대적인 보안 경계로 만들지 않는다.

## Consequences

- 활성 workload뿐 아니라 사용하지 않는 retained PV/Volume도 정리 또는 명시적 우회
  없이는 chart 제거를 막는다.
- 거부된 hook Job은 진단 log를 위해 남고 다음 제거 시도 전에 교체된다.
- guard 성공 후 hook Job은 삭제된다.
- `quiescing`과 `granted` 상태는 모두 최대 5분만 유효하다. 실패한 guard는 즉시
  자기 attempt를 취소하고, 비정상 종료로 상태가 남아도 만료 후 Controller가
  provisioning과 lifecycle validation reconcile을 재개한다.
- Argo CD에서는 ShiftPV를 별도 Application으로 관리하고 Application 삭제를 제거
  경로로 사용해야 동일한 동적 검사가 실행된다.
- 직접 Kubernetes DELETE는 dependency가 없어도 guard가 만든 `granted` 상태가 없으면
  거부된다. webhook 자체의 강제 제거와 RBAC 우회는 cluster administrator 책임이다.
