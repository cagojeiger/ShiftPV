# 0008. 이동 전 consumer를 보존하는 사전 점검을 한다

- 상태: Accepted
- 날짜: 2026-09-03
- 상세 계약: [volume mobility](../spec/volume-mobility.md#preflight-and-placement)

## Context

관측 가능한 배치 제약을 eviction 뒤 발견하면 서비스가 불필요하게 중단된다. ShiftPV가 scheduler의
모든 의미를 복제해도 실제 배치 결과를 보장할 수 없다.

## Decision

ShiftPV는 consumer 중단 전에 지원 범위, binding identity, 강제 배치 제약과 disruption 허용 여부를
읽기 전용으로 점검한다. 호환 가능한 destination이 확인되면 Kubernetes가 eviction과 실제 배치를
최종 판정한다. 그전에는 기존 consumer를 유지하고 재평가한다.

## Alternatives considered

| 대안 | 절충점 |
|---|---|
| eviction 뒤 검사 | 피할 수 있는 서비스 중단이 발생한다. |
| 자체 scheduler 판정 | Kubernetes scheduling 의미를 불완전하게 복제한다. |
| workload 제약 자동 완화 | workload owner와 GitOps 정책의 소유권이 겹친다. |

## Consequences

보수적 판정은 일부 가능한 이동을 대기시킬 수 있다. Preflight는 중단 전 안전 조건이며 실제 예약과
이동 성공은 Kubernetes 배치 결과와 이후 transaction이 결정한다.
