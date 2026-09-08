# 0003. Kubernetes CSI를 제품 인터페이스로 사용한다

- 상태: Accepted
- 날짜: 2026-09-01
- 상세 계약: [CSI driver](../spec/csi-driver.md)
- 참고: [Container Storage Interface specification](https://github.com/container-storage-interface/spec)

## Context

ShiftPV는 StorageClass 동적 provisioning과 kubelet의 표준 volume lifecycle에 참여한다.

## Decision

Kubernetes storage interface를 CSI로 고정한다. 표준 sidecar와 kubelet registration을 사용하고,
실제로 구현한 capability만 광고한다.

## Alternatives considered

| 대안 | 절충점 |
|---|---|
| PVC 감시 후 HostPath PV 생성 | 구현은 작지만 CSI Node lifecycle과 mount 권한 경계가 없다. |
| 별도 scheduler 또는 in-tree plugin | Kubernetes 표준 확장 경로와 결합되지 않는다. |

## Consequences

사용자는 일반 PVC와 StorageClass 흐름을 사용한다. 배포는 Controller, Node Plugin, CSI sidecar,
Unix socket과 RBAC를 포함한다.
