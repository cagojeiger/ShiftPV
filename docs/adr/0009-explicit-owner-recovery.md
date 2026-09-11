# 0009. 현재 owner를 명시적으로 복구한다

- 상태: Accepted
- 날짜: 2026-09-03
- 상세 계약: [volume mobility](../spec/volume-mobility.md#blocked-recovery)

## Context

Blocked 이동에는 이전 copy, promotion 또는 cleanup 작업이 남을 수 있다. Commit 뒤 destination write가
가능하므로 실패 시점을 기준으로 source를 선택하면 stale data를 owner로 만들 수 있다.

## Decision

운영자가 기록된 current owner의 복구를 명시적으로 요청한다. 같은 reconciler가 실행 중인 helper를
멈추고 exact current copy를 검증한 뒤 같은 owner의 mount를 다시 연다. Non-owner copy는 recovery가
삭제하지 않으며 bounded inventory와 cleanup lifecycle이 보존·분류한다. 전 과정은 영속 상태로 기록한다.

## Alternatives considered

| 대안 | 절충점 |
|---|---|
| 자동 source rollback | destination write 뒤 stale source가 owner가 될 수 있다. |
| 수동 status patch | 실행 중 helper와 filesystem artifact를 함께 조정하지 못한다. |
| recovery에서 non-owner 자동 삭제 | 불완전한 journal이 stale copy에 삭제 권한을 부여할 수 있다. |
| 별도 recovery controller | 한 transaction의 상태 소유자가 둘이 된다. |

## Consequences

명확한 current owner와 접근 가능한 filesystem이 복구의 전제다. 불명확한 authority와 검증되지 않은
artifact는 운영자가 확인할 수 있도록 보존된다.
