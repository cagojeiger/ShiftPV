# Architecture Decision Records

ADR은 현재 구현의 구조적 결정을 한 문서에 하나씩 기록한다. 모든 ADR은 `Context`,
`Decision`, `Alternatives considered`, `Consequences` 순서를 사용한다. 동작 세부사항은
[`spec/`](../spec/README.md)에 두고 실행 결과는 [`validation/`](../validation/README.md)에
둔다.

| ADR | 결정 | 상태 |
|-----|------|------|
| [0001](0001-position-local-hostpath-only-no-replication.md) | 복제 없는 로컬 스토리지 | Accepted |
| [0002](0002-mounted-filesystem-boundary.md) | 사전 마운트된 로컬 filesystem만 Pool로 관리 | Accepted |
| [0003](0003-csi-product-foundation.md) | Kubernetes CSI를 제품 기반으로 사용 | Accepted |
| [0004](0004-minimal-csi-bootstrap.md) | 첫 제품 범위를 최소 CSI lifecycle로 제한 | Accepted |
| [0005](0005-automatic-cordon-volume-mobility.md) | 정상 cordon 이동과 Kubernetes 배치 협력 | Accepted |
| [0006](0006-fail-closed-uninstall-guard.md) | 의존 storage가 남은 제거를 기본 거부 | Accepted |
| [0007](0007-controller-managed-webhook-certificates.md) | admission 인증서를 Controller가 관리 | Accepted |
| [0008](0008-explicit-owner-recovery.md) | Blocked 이동에서 현재 owner를 명시적으로 재개 | Accepted |
| [0009](0009-nondisruptive-mobility-preflight.md) | 이동 전 consumer를 보존하는 사전 점검 | Accepted |
| [0010](0010-operator-visible-mobility-diagnostics.md) | 기존 Move journal을 운영 진단에 사용 | Accepted |
| [0011](0011-pool-filesystem-capacity-admission.md) | Pool filesystem 총량으로 신규 할당 제어 | Accepted |
| [0012](0012-node-reported-pool-readiness.md) | Node가 실제 filesystem Pool readiness를 보고 | Accepted |

파일 이름은 `NNNN-kebab-title.md` 형식을 사용한다. 구체적인 필드, 상태 전이, 명령과
테스트 결과는 ADR에 복제하지 않는다. 새 구조적 결정이나 기존 결정의 대체는 새 ADR로
기록한다. 정식 버전 전 문서 정리와 사실 오류 수정은 결정의 의미를 바꾸지 않는 범위에서
기존 ADR에 반영할 수 있다.
