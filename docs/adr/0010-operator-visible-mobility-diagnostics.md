# 0010. 이동 진단은 기존 Move journal에 기록한다

- 상태: Accepted
- 날짜: 2026-09-04
- 상세 계약: [volume mobility](../spec/volume-mobility.md#operator-diagnostics)

## Context

Controller log만으로는 이동이 자동 재평가 중인지, 안전을 위해 멈췄는지, 운영자가 무엇을
해야 하는지 빠르게 구분하기 어렵다. 그러나 진단만을 위한 별도 resource나 process는 현재
제품 규모에 비해 복잡하다.

## Decision

기존 `ShiftPVMove` transaction journal을 운영 진단의 source of truth로 사용한다. 현재 진행,
실패 이유와 다음 안전 행동을 machine-readable 상태와 사람용 설명으로 함께 제공하고, 의미 있는
변화만 Kubernetes Event로 보조한다. 진단 시간은 자동 실패나 rollback 근거로 사용하지 않는다.

## Alternatives considered

- Log-only 진단은 transaction 상태와 운영 행동을 구조적으로 조회하기 어렵다.
- 별도 진단 CRD나 controller는 동일 이동의 상태 소유권을 분산시킨다.
- 전용 CLI는 Kubernetes 표준 조회 경로보다 추가 배포와 호환성 비용이 크다.

## Consequences

운영자는 Move resource만으로 현재 상태와 다음 행동을 찾을 수 있다. Event와 message는 관측
가능성을 높이지만 data authority나 무결성 증거를 대신하지 않는다.
