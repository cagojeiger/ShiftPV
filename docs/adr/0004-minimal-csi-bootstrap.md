# 0004. 최소 CSI lifecycle로 시작한다

- 상태: Accepted
- 날짜: 2026-09-01
- 후속 결정: Pool 총량 admission은 [ADR 0005](0005-pool-filesystem-capacity-admission.md)가 확장한다.
- 후속 결정: owner와 topology는 [ADR 0007](0007-automatic-cordon-volume-mobility.md)이 확장한다.
- 상세 계약: [CSI driver](../spec/csi-driver.md), [StorageClass](../spec/storage-class.md)

## Context

첫 제품은 StorageClass, PVC, provisioning, publish와 재설치 흐름을 cluster에서 검증할 수 있는
가장 작은 lifecycle이 필요했다.

## Decision

| 제공 | 후속 범위 |
|---|---|
| single-owner RWO Filesystem volume | volume expansion |
| 동적 provisioning과 mount lifecycle | snapshot |
| Retain 기반 데이터 보존 | per-volume hard quota |

Requested capacity는 Pool 총량 회계의 단위로 사용한다.

## Alternatives considered

| 대안 | 절충점 |
|---|---|
| CSI 기능 일괄 구현 | 기반 검증 전에 상태와 장애 경계가 커진다. |
| HostPath 전용 provisioner | 작지만 CSI 제품 기반을 충족하지 못한다. |

## Consequences

지원 capability와 오류 경계가 작고 명확하다. 개별 volume의 실제 write 제한은 filesystem 운영
계층이 담당한다.
