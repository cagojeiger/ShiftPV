# 0012. Admission 인증서는 Controller가 관리한다

- 상태: Accepted
- 날짜: 2026-09-03
- 운영 절차: [Helm chart](../../charts/shiftpv/README.md)

## Context

Admission webhook은 service DNS에 맞는 serving certificate, CA bundle, 갱신과 hot reload가 필요하다.
외부 certificate operator는 현재 설치 범위에 새로운 필수 dependency를 더한다.

## Decision

기존 Controller reconciler가 webhook 인증서와 trust configuration을 관리한다. CA 전환 중에도 API
server trust를 유지하며 ShiftPV 소유 resource만 변경한다. Helm은 안정적인 service 선언을 맡는다.

## Alternatives considered

| 대안 | 절충점 |
|---|---|
| Helm 설치 시 생성 | 렌더가 비결정적이고 실행 중 갱신과 hot reload가 분리된다. |
| 외부 certificate operator | 검증된 수명주기를 얻는 대신 필수 dependency가 늘어난다. |
| 수동 교체 | 만료와 CA bundle 동기화를 운영 절차가 담당한다. |

## Consequences

Controller는 Secret과 webhook configuration 조정 권한을 갖는다. CA private key가 있는 namespace는
storage operator 보안 경계이며, 장기 중단 후 복구되면 reconciler가 다시 수렴한다.
