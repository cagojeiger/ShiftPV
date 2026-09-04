# Validation Evidence

이 디렉터리는 특정 commit과 환경에서 실제로 실행한 검증 결과를 기록한다. 설계
요구사항은 ADR/spec에, 실행 방법과 CI 합격 기준은
[`development/testing.md`](../development/testing.md)에 둔다.

| 기록 | 범위 |
|------|------|
| [kind-e2e-2026-09-01.md](kind-e2e-2026-09-01.md) | Kubernetes 1.35.8, 두 worker의 분리된 pool, 기본 StorageClass와 Helm 재설치 |
| [kind-mobility-e2e-2026-09-02.md](kind-mobility-e2e-2026-09-02.md) | cordon mobility의 Blocked/Succeeded terminal과 Controller restart recovery |
| [kind-ultraqa-2026-09-03.md](kind-ultraqa-2026-09-03.md) | Node Plugin 재시작, 병렬 E2E 격리, admission outage와 filesystem fault 회복 |
| [kind-argocd-uninstall-2026-09-03.md](kind-argocd-uninstall-2026-09-03.md) | Argo CD Application guard와 lifecycle admission 허용/거부, blocker 해소 후 삭제 수렴 |
| [blocked-owner-recovery-2026-09-03.md](blocked-owner-recovery-2026-09-03.md) | 명시적 Blocked owner 복구, before/after commit 데이터 보존, 실패·재시작·CRD 검증 |
| [nondisruptive-preflight-2026-09-04.md](nondisruptive-preflight-2026-09-04.md) | eviction 전 제약/PDB 판정, Retain PV/PVC UID 격리, 일시적 조건 해소 후 재개 |
| [public-artifact-ci-2026-09-04.md](public-artifact-ci-2026-09-04.md) | 공개 chart package SHA-256과 image manifest digest 고정, 실제 Helm/PVC mount CI |
| [operator-diagnostics-2026-09-04.md](operator-diagnostics-2026-09-04.md) | Move reason/message/진행 시각/Event, API 오류 복구와 실제 Blocked/Recovered/Succeeded 진단 |

검증 문서는 당시의 환경과 결과를 보존하는 snapshot이다. 현재 branch의 통과 여부는
같은 테스트를 다시 실행하거나 해당 commit의 CI check로 판단한다.
