# Validation Evidence

이 디렉터리는 특정 commit과 환경에서 실제로 실행한 검증 결과를 기록한다. 설계
요구사항은 ADR/spec에, 실행 방법과 CI 합격 기준은
[`development/testing.md`](../development/testing.md)에 둔다.

| 기록 | 범위 |
|------|------|
| [kind-e2e-2026-09-01.md](kind-e2e-2026-09-01.md) | Kubernetes 1.35.8, 두 worker의 분리된 pool, 기본 StorageClass와 Helm 재설치 |

검증 문서는 당시의 환경과 결과를 보존하는 snapshot이다. 현재 branch의 통과 여부는
같은 테스트를 다시 실행하거나 해당 commit의 CI check로 판단한다.
