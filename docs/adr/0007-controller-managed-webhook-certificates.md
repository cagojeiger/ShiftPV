# 0007. Admission 인증서는 Controller가 관리한다

- 상태: Accepted
- 날짜: 2026-09-03
- 운영 절차: [Helm chart](../../charts/shiftpv/README.md)

## Context

Admission webhook은 service DNS에 맞는 serving certificate와 신뢰 가능한 CA bundle이
필요하다. Helm에서 설치 때마다 인증서를 생성하면 렌더가 비결정적이고 갱신과 hot reload가
닫힌 루프가 되지 않는다. 외부 certificate operator를 필수 dependency로 두는 것도 현재
제품 범위보다 크다.

## Decision

기존 Controller reconciler가 webhook 인증서와 trust configuration을 관리한다. 인증서는
실행 중 갱신하고 CA 전환 중에도 API server trust가 이어지도록 조정한다. Controller는
ShiftPV가 소유한다고 확인한 resource만 변경하며 Helm은 안정적인 service 선언만 맡는다.

## Alternatives considered

- Helm 생성 인증서는 결정적 렌더, 자동 갱신과 hot reload를 함께 만족하지 못한다.
- Cert-manager 같은 외부 operator는 검증된 대안이지만 필수 설치 dependency가 늘어난다.
- 수동 인증서 교체는 만료와 CA bundle 불일치 위험을 운영자에게 남긴다.

## Consequences

Controller에는 Secret과 webhook configuration을 조정할 권한이 필요하다. CA private key를
담은 namespace는 storage operator 보안 경계가 되며, Controller와 API가 장기간 모두
중단되면 복구 후에야 갱신이 다시 수렴한다.
