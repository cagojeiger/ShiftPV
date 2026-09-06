# 0004. 첫 제품 범위를 최소 CSI lifecycle로 제한한다

- 상태: Accepted
- 날짜: 2026-09-01
- 후속 결정: owner와 topology는 [ADR 0005](0005-automatic-cordon-volume-mobility.md)가 확장한다.
- 후속 결정: Pool 총량 admission은 [ADR 0011](0011-pool-filesystem-capacity-admission.md)이 확장한다.
- 상세 계약: [CSI driver](../spec/csi-driver.md), [StorageClass](../spec/storage-class.md)

## Context

첫 제품 상태는 StorageClass, PVC, provisioning, publish와 재설치 흐름이 실제 cluster에서
성립하는지 검증할 수 있을 만큼 작아야 했다.

## Decision

초기 범위는 single-owner RWO Filesystem volume의 동적 provisioning과 mount lifecycle,
Retain 기반 보존으로 제한한다. Volume expansion, snapshot과 hard capacity quota는 포함하지
않는다.

## Alternatives considered

- CSI 기능을 한 번에 넓히면 제품 기반을 검증하기 전에 상태와 장애 경계가 복잡해진다.
- HostPath 전용 provisioner는 최소 구현은 쉽지만 ADR 0003의 CSI 제품 기반을 충족하지 못한다.

## Consequences

기본 lifecycle은 작고 검증 가능하지만 지원하지 않는 기능은 명시적으로 거부해야 한다.
Requested capacity는 hard write limit가 아니다.
