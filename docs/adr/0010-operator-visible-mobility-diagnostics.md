# 0010. Move journal을 운영 진단에 사용한다

- 상태: Accepted
- 날짜: 2026-09-04
- 상세 계약: [volume mobility](../spec/volume-mobility.md#operator-diagnostics)

## Context

운영자는 이동이 진행 중인지, 안전을 위해 멈췄는지, 다음 행동이 무엇인지 Kubernetes API에서
구분할 수 있어야 한다. 별도 진단 resource는 같은 이동의 상태 소유권을 분산시킨다.

## Decision

`ShiftPVMove` transaction journal을 운영 진단의 source of truth로 사용한다. 현재 단계, 실패 이유와
다음 안전 행동을 machine-readable 상태와 사람용 설명으로 함께 제공한다. 의미 있는 변화는
Kubernetes Event로 보조한다.

## Alternatives considered

| 대안 | 절충점 |
|---|---|
| log-only 진단 | transaction 상태와 운영 행동을 구조적으로 조회하기 어렵다. |
| 별도 진단 CRD/controller | 동일 이동의 상태 소유권이 분산된다. |
| 전용 CLI | 추가 배포와 API 호환성 비용이 생긴다. |

## Consequences

운영자는 Move resource에서 현재 상태와 다음 행동을 찾는다. Event와 message는 관측 정보를 제공하며,
data authority와 무결성은 transaction 상태와 filesystem 검증이 결정한다.
