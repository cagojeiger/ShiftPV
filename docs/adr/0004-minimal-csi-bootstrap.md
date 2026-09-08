# 0004. 최소 CSI lifecycle을 제공한다

- 상태: Accepted
- 날짜: 2026-09-01
- 관련 결정: [Pool 총량 admission](0005-pool-filesystem-capacity-admission.md), [owner와 topology](0007-automatic-cordon-volume-mobility.md)
- 상세 계약: [CSI driver](../spec/csi-driver.md), [StorageClass](../spec/storage-class.md)

## Context

Directory 기반 CSI는 provisioning, publish와 데이터 보존을 명확한 장애 경계 안에서 제공해야 한다.
지원 capability가 늘어날수록 검증할 상태와 복구 경계도 늘어난다.

## Decision

| 선택 | 이유 |
|---|---|
| single-owner RWO Filesystem volume | directory의 authority를 단일화한다. |
| 동적 provisioning과 mount lifecycle | Kubernetes PVC의 핵심 lifecycle에 집중한다. |
| Retain 기반 데이터 보존 | workload 삭제와 데이터 폐기를 분리한다. |

Requested capacity는 Pool 총량 회계의 단위로 사용한다.

## Alternatives considered

| 대안 | 절충점 |
|---|---|
| CSI 기능 일괄 구현 | 기반 검증 전에 상태와 장애 경계가 커진다. |
| HostPath 전용 provisioner | 작지만 CSI 제품 기반을 충족하지 못한다. |

## Consequences

지원 capability와 오류 경계가 작고 명확하다. 개별 volume의 실제 write 제한은 filesystem 운영
계층이 담당한다.
