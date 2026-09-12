# ShiftPV Contracts

이 디렉터리는 현재 바이너리와 Helm chart가 구현하는 계약만 포함한다.

| 문서 | 단일 책임 |
|------|-----------|
| [csi-driver.md](csi-driver.md) | CSI RPC, topology, provision/publish와 mount 동작 |
| [storage-class.md](storage-class.md) | StorageClass, 기본 클래스 설정과 용량 의미 |
| [volume-mobility.md](volume-mobility.md) | 정상 cordon cold migration과 안전 경계 |
| [source-cleanup.md](source-cleanup.md) | exact copy cleanup·GC 관찰·receipt 수렴 |
| [metrics.md](metrics.md) | Pool·이동·CSI 관측 지표와 freshness |

## Current scope

| Capability | Current contract |
|---|---|
| Volume | RWO Filesystem |
| Pool | node별 기존 absolute non-root directory 하나 |
| Filesystem layout | root filesystem 하위 directory 또는 별도 mount |
| Capacity | Pool reservation과 filesystem available bytes 기반 신규 할당 |
| Mobility | 정상인 cordon owner의 계획된 cold migration |
| Authority | owner node 하나와 active Move 하나 |
| Recovery | 재시작 후 reconcile과 명시적 `ResumeOwner` |
| Cleanup / GC | 승인된 exact copy만 삭제; orphan과 불명확한 상태는 `NeedsReview` 보존 |
| Replication, HA, unavailable-node failover | 외부 storage architecture |
| Snapshot과 backup | 외부 data-protection system |
| RWX, raw block, volume expansion | 현재 제품 범위 밖 |
| Per-volume filesystem quota | filesystem 또는 외부 quota manager |
| 기존 PV migration | workload별 migration 절차 |

계약 변경이 구조적 결정을 바꾸면 ADR을 먼저 추가하거나 대체한다.
