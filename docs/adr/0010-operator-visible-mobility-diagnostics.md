# 0010. Move journal을 운영 진단에 사용한다

- 상태: Accepted
- 날짜: 2026-09-04
- 적용 범위: 0.4 target contract
- 상세 계약: [volume mobility](../spec/volume-mobility.md#reconcile-loop)

## Context

운영자는 이동이 진행 중인지, 안전을 위해 멈췄는지, 다음 행동이 무엇인지 Kubernetes API에서
구분할 수 있어야 한다. Ephemeral Job, Pod, log는 사라질 수 있으므로 transaction truth가 될 수 없다.

## Decision

Parent resource의 status journal과 finalizer를 운영 진단의 source of truth로 사용한다. Move와 Volume은
intent, current owner/copy, generation fence, helper receipt, publication proof, purge receipt, absence proof를
자기 journal에 기록한다. Job은 parent journal에 기록된 intent를 수행하는 실행 수단이며, 완료 여부만으로
promote, purge, release를 증명하지 않는다.

현재 단계, 실패 이유와 다음 안전 행동을 machine-readable 상태와 사람용 설명으로 함께 제공한다. 의미
있는 변화는 Kubernetes Event로 보조한다.

Prometheus 지표는 기존 관찰 결과의 읽기 전용 집계로 제공한다. 수집 요청은 메모리에 보존한
결과를 사용하고, storage action의 승인과 상태 전이는 기존 controller와 CSI가 소유한다.

## Alternatives considered

| 대안 | 절충점 |
|---|---|
| log-only 진단 | transaction 상태와 운영 행동을 구조적으로 조회하기 어렵다. |
| 별도 진단 CRD/controller | 동일 이동의 상태 소유권이 분산된다. |
| 전용 CLI | 추가 배포와 API 호환성 비용이 생긴다. |
| Job status를 journal로 사용 | TTL, retry, controller restart 뒤 권한 증거가 사라진다. |

## Consequences

운영자는 parent resource에서 현재 상태와 다음 행동을 찾는다. Event와 message는 관측 정보를 제공하며,
data authority와 무결성은 transaction 상태와 filesystem 검증이 결정한다. Move journal이 모순을 기록하면
controller는 자동 삭제나 promote 대신 `Blocked`를 유지하고, cleanup subjournal 모순은 `NeedsReview`에 머문다.
