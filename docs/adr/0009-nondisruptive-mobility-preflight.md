# 0009. 이동 전 기존 consumer를 보존하는 사전 점검을 한다

- 상태: Accepted
- 날짜: 2026-09-03
- 상세 계약: [volume mobility](../spec/volume-mobility.md#non-disruptive-preflight)

## Context

이미 관측 가능한 scheduling 제약을 eviction 뒤에 발견하면 data authority가 안전해도 서비스가
불필요하게 중단된다. 반대로 ShiftPV가 scheduler의 모든 판단을 복제하면 서로 다른 결과를 낼
수 있다.

## Decision

ShiftPV는 consumer를 중단하기 전에 지원 범위, binding identity, 강제 배치 제약과 disruption
허용 여부를 읽기 전용으로 점검한다. 호환 가능한 destination을 증명하지 못하면 기존 consumer를
보존하고 재평가한다. 이 검사는 보수적인 선행 조건이며 실제 eviction과 배치는 Kubernetes가
최종 판정한다.

## Alternatives considered

- Eviction 뒤 검사하면 피할 수 있는 서비스 중단이 발생한다.
- 자체 scheduler 판정은 Kubernetes scheduling 의미를 불완전하게 복제한다.
- 제약을 자동 완화하면 workload owner와 GitOps 정책을 침범한다.

## Consequences

보수적인 판정 때문에 이동이 가능한 workload도 대기할 수 있다. Preflight 통과는 예약이나
이동 성공 보장이 아니므로 운영자는 cordon 뒤 실제 이동 완료를 확인해야 한다.
