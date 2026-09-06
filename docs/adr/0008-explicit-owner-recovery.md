# 0008. Blocked 이동은 현재 owner를 명시적으로 재개한다

- 상태: Accepted
- 날짜: 2026-09-03
- 상세 계약: [volume mobility](../spec/volume-mobility.md#explicit-owner-recovery)

## Context

Blocked 이동에서 cordon만 제거하거나 status를 직접 수정하면 이전 copy, promotion 또는
cleanup 작업과 충돌할 수 있다. Commit 뒤 destination에서 쓰기가 시작됐을 가능성이 있으므로
실패했다는 이유만으로 source를 복구하는 것도 안전하지 않다.

## Decision

ShiftPV는 자동 rollback이나 failover 대신 운영자가 요청하는 명시적 owner recovery를
제공한다. Recovery는 기록된 current owner를 바꾸지 않고, 남은 작업과 비권위 artifact를
안전하게 정리한 뒤 같은 owner의 mount를 다시 연다. 과정은 영속 상태로 기록하고 재시작해도
멱등적으로 수렴한다.

## Alternatives considered

- 자동 source rollback은 destination write 이후 stale data를 authoritative하게 만들 수 있다.
- 수동 status patch는 실행 중인 helper와 filesystem artifact를 조정하지 못한다.
- 별도 recovery controller는 같은 transaction에 두 조정자를 만들어 책임이 겹친다.

## Consequences

불명확한 authority와 unavailable owner는 자동 복구되지 않는다. 운영자 개입이 필요하며,
안전성을 증명하지 못한 artifact는 자동 삭제하지 않는다.
