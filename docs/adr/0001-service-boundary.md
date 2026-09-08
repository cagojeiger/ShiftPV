# 0001. 기존 filesystem의 로컬 directory를 관리한다

- 상태: Accepted
- 날짜: 2026-09-01
- 상세 계약: [CSI driver](../spec/csi-driver.md), [volume mobility](../spec/volume-mobility.md)

## Context

소규모 self-managed cluster에는 기존 hostPath StorageClass를 대체하면서 표준 PVC lifecycle과 계획
이동을 제공하는 로컬 스토리지가 필요하다. 분산 스토리지는 복제와 네트워크 데이터 경로를 더하고,
HostPath는 동적 provisioning과 CSI lifecycle을 제공하지 않는다.

## Decision

| 서비스 경계 | 결정 |
|---|---|
| 대상 | 운영자가 준비한 기존 Linux filesystem 안의 local directory |
| Kubernetes 경험 | StorageClass와 PVC를 통한 동적 provisioning |
| 데이터 authority | 한 시점에 owner node 하나 |
| 정상 I/O | node-local bind mount로 직접 처리 |
| 이동 | 정상 source를 사용하는 계획된 cold migration |
| 내구성 | filesystem, replication, backup은 cluster 운영 체계가 담당 |

## Alternatives considered

| 대안 | 절충점 |
|---|---|
| 분산 스토리지 | 내구성과 failover를 얻는 대신 운영 비용과 네트워크 데이터 경로가 늘어난다. |
| HostPath 직접 사용 | 구성이 단순하지만 동적 provisioning과 CSI lifecycle이 없다. |

## Consequences

ShiftPV는 소중규모 cluster의 local directory storage lifecycle에 집중한다. Owner node와 host
filesystem의 가용성이 volume 가용성을 결정한다.
