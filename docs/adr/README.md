# Architecture Decision Records

ADR은 현재 구현의 구조적 결정을 한 문서에 하나씩 기록한다. 동작 세부사항은
[`spec/`](../spec/README.md)에 두고 실행 결과는 [`validation/`](../validation/README.md)에
둔다.

| ADR | 결정 | 상태 |
|-----|------|------|
| [0001](0001-position-local-hostpath-only-no-replication.md) | 복제 없는 로컬 디렉터리 CSI 드라이버 | Accepted |
| [0002](0002-mounted-filesystem-boundary.md) | 이미 준비된 로컬 filesystem부터 관리 | Accepted |
| [0003](0003-csi-product-foundation.md) | Kubernetes CSI를 제품 기반으로 사용 | Accepted |
| [0004](0004-minimal-csi-bootstrap.md) | 최소 CSI bootstrap 범위 고정 | Accepted |

파일 이름은 `NNNN-kebab-title.md` 형식을 사용한다. 결정을 변경할 때는 기존 ADR을
조용히 확장하지 않고 새 ADR로 대체한다.
