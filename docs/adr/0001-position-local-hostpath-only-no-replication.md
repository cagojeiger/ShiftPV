# 0001. 복제 없는 로컬 스토리지를 제공한다

- 상태: Accepted
- 날짜: 2026-09-01
- 상세 계약: [CSI driver](../spec/csi-driver.md), [volume mobility](../spec/volume-mobility.md)

## Context

소규모 self-managed Kubernetes cluster에는 운영자가 준비한 로컬 filesystem을 표준 PVC로
사용하는 단순한 경로가 필요하다. 분산 스토리지는 복제와 네트워크 데이터 경로를 추가하고,
일반 HostPath는 표준 CSI lifecycle과 동적 provisioning을 제공하지 않는다.

## Decision

ShiftPV는 한 owner node의 로컬 filesystem을 사용하는 directory-backed CSI storage를 제공한다.
정상 애플리케이션 I/O에는 Controller나 네트워크 데이터 경로를 넣지 않는다. Replication, HA,
automatic failover와 backup은 제공하지 않으며, 정상 source를 이용한 계획된 cold migration만
별도 결정 범위에서 지원한다.

## Alternatives considered

- 분산 스토리지는 내구성과 failover를 제공하지만 현재 소중규모 범위보다 운영 비용이 크다.
- HostPath 직접 사용은 단순하지만 동적 provisioning과 CSI lifecycle을 제공하지 않는다.

## Consequences

owner node나 filesystem을 사용할 수 없으면 volume도 사용할 수 없다. 데이터 내구성과 host
filesystem 운영은 cluster 운영자 책임이다.
