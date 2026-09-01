# 0001. 복제 없는 로컬 디렉터리 CSI 드라이버

- 상태: Accepted
- 날짜: 2026-09-01

## Context

소규모 self-managed Kubernetes cluster에는 운영자가 이미 준비한 로컬
filesystem을 PVC로 사용하는 단순한 동적 provisioning 경로가 필요하다. 분산
스토리지는 복제와 네트워크 데이터 경로를 추가하고, 일반 HostPath는 표준 CSI
lifecycle과 동적 provisioning을 제공하지 않는다.

## Decision

ShiftPV는 운영자가 노드에 준비한 로컬 디렉터리를 RWO Filesystem volume으로
제공하는 CSI driver다.

- volume 데이터는 선택된 owner node 한 곳에만 존재한다.
- Pod의 데이터 경로는 bind mount를 통해 host filesystem으로 직접 연결된다.
- replication, HA, failover, backup과 node 간 데이터 이동을 제공하지 않는다.
- RWX, raw block, snapshot과 volume expansion을 지원하지 않는다.
- Kubernetes scheduler를 대체하거나 별도 DB/MQ를 운영하지 않는다.

## Consequences

- owner node 또는 그 filesystem을 사용할 수 없으면 volume도 사용할 수 없다.
- 데이터 내구성과 host filesystem 운영은 cluster 운영자의 책임이다.
- 실행 중인 Pod의 정상 I/O 경로에는 ShiftPV controller가 개입하지 않는다.
- 지원 범위를 넓히는 변경은 별도 ADR과 구현 검증을 요구한다.
