# Architecture Decision Records

ADR은 구조적 결정과 그 결과만 기록한다. 동작 계약은 [`spec/`](../spec/README.md), 현재 검증 기준은
[`development/testing.md`](../development/testing.md)가 소유한다.

```mermaid
flowchart LR
    SCOPE[서비스 경계] --> FS[Pool filesystem 경계]
    FS --> CSI[CSI lifecycle]
    CSI --> POOL[Capacity + readiness]
    POOL --> MOVE[계획 이동]
    MOVE --> SAFE[복구 + 관측]
    SAFE --> OPS[제거 + 인증서]
```

| 계층 | ADR | 결정 | 상태 |
|---|---|---|---|
| 제품 | [0001](0001-service-boundary.md) | 기존 filesystem의 로컬 directory를 관리 | Accepted |
| 제품 | [0002](0002-mounted-filesystem-boundary.md) | Pool directory와 host filesystem 책임 분리 | Accepted |
| CSI | [0003](0003-csi-product-foundation.md) | Kubernetes CSI를 제품 인터페이스로 사용 | Accepted |
| CSI | [0004](0004-minimal-csi-bootstrap.md) | 최소 CSI lifecycle 제공 | Accepted |
| Pool | [0005](0005-pool-filesystem-capacity-admission.md) | Pool filesystem 총량으로 신규 할당 제어 | Accepted |
| Pool | [0006](0006-node-reported-pool-readiness.md) | Node가 실제 Pool readiness 보고 | Accepted |
| 이동 | [0007](0007-automatic-cordon-volume-mobility.md) | 정상 cordon 이동과 Kubernetes 배치 협력 | Accepted |
| 이동 | [0008](0008-nondisruptive-mobility-preflight.md) | consumer를 보존하는 이동 사전 점검 | Accepted |
| 이동 | [0009](0009-explicit-owner-recovery.md) | 현재 owner를 명시적으로 복구 | Accepted |
| 이동 | [0010](0010-operator-visible-mobility-diagnostics.md) | Move journal을 운영 진단에 사용 | Accepted |
| 운영 | [0011](0011-fail-closed-uninstall-guard.md) | storage dependency 해소 후 제거 | Accepted |
| 운영 | [0012](0012-controller-managed-webhook-certificates.md) | Controller가 admission 인증서 관리 | Accepted |

모든 ADR은 `Context → Decision → Alternatives considered → Consequences` 순서를 사용한다.
파일명은 `NNNN-kebab-title.md` 형식이다. 정식 release 전에는 넓은 경계에서 세부 경계로 번호를
정돈하고, 정식 release 뒤에는 기존 번호를 고정한 채 새 결정과 대체 결정을 뒤에 추가한다.
