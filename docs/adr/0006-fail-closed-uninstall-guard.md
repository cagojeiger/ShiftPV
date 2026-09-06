# 0006. 의존 storage가 남은 제거를 기본 거부한다

- 상태: Accepted
- 날짜: 2026-09-03
- 운영 절차: [Helm chart](../../charts/shiftpv/README.md#uninstall-and-recovery)

## Context

Chart를 제거해 driver와 RBAC가 사라져도 Helm이 소유하지 않는 PVC, PV, ShiftPV resource와
host data는 남을 수 있다. 이 상태에서는 기존 Pod가 재시작해도 volume을 mount할 수 없고
진행 중인 이동도 멈춘다. 개별 object finalizer만으로 driver 전체의 제거 순서는 보장되지 않는다.

## Decision

ShiftPV는 제거 전에 새 provisioning을 멈추고 storage dependency를 검사한다. Helm
pre-delete guard와 Kubernetes API deletion validation을 함께 사용하며, dependency나 API
상태가 불명확하면 제거를 거부한다. 운영자가 보존 데이터의 책임을 명시적으로 인수하는
emergency bypass만 별도로 허용한다.

## Alternatives considered

- Helm hook만 사용하면 GitOps controller의 삭제 경쟁에서 보호 resource가 먼저 지워질 수 있다.
- PVC/PV finalizer만으로는 Deployment, DaemonSet, RBAC와 CSI resource 전체를 보호하지 못한다.
- 무조건 제거 허용은 retained data를 다시 mount할 수 없는 상태로 만들 수 있다.

## Consequences

Retained dependency가 있으면 정상 uninstall이 중단된다. Helm과 Argo CD는 각 lifecycle에 맞는
guard 경로를 사용해야 하며, 강제 우회 이후 복구는 cluster 운영자 책임이다.
