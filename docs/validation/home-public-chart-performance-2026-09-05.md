# Home public chart lifecycle and performance validation — 2026-09-05

## Scope

Commit `77672799d4066e0f55d5ce5171022dc4b7d148ae`에서 공개 Helm repository의 chart
`0.1.2`를 Home MicroK8s에 임시 설치해 CSI lifecycle과 반복 운영 비용을 관찰했다.

- chart package SHA-256:
  `acc34e186d228a4e79d2c56da737f540a76c11c8970f3abaeb2718ca46e73444`
- Controller:
  `0.1.3@sha256:77cc63a6aa3d61417af5c23443723f347e277fa062fecde9665fb52ecf5478de`
- Node Plugin:
  `0.1.1@sha256:7b2f5d3066375d88a7062cf09efb5006b47f6c7688f19366867db33fd7b121f9`
- Kubernetes: MicroK8s `v1.35.6`, `server-01`/`server-02`, node당 4 CPU와 약 16 GiB
- filesystem: 두 node의 약 477 GiB ext4 root NVMe 임시 경로
- network link: node당 1 Gbps

Home node에는 별도 data filesystem mount가 없었다. `/var/tmp` 아래 임시 Pool은 ADR
0002의 production Pool 계약인 별도 사전 mount를 충족하지 않는다. 따라서 이 결과는
공개 artifact의 설치와 CSI control path baseline이며 production filesystem 성능 인증이
아니다.

## Shared-cluster stop condition

첫 실행은 workload를 `server-02`에 고정하기 위해 `server-01`을 잠깐 cordon했다. drain과
기존 Pod eviction은 수행하지 않았지만 CloudNativePG가 cordon을 cluster maintenance
신호로 관찰해 `pg-home-1`에서 `pg-home-2`로 primary switchover를 시작했다.

- `pg-home-1` restart count가 2에서 3으로 증가했다.
- 전환 중 두 FileGate Pod에서 readiness HTTP 503, Gitea에서 readiness timeout이
  관찰됐다.
- 두 PostgreSQL instance와 FileGate/Gitea는 다시 Ready가 됐고 cluster는
  `pg-home-2` primary의 healthy state로 수렴했다.

비테스트 workload 변화가 즉시 중단 조건이므로 mobility 측정 전에 실행을 중단하고 모든
테스트 리소스를 제거했다. 이 결과는 cordon 자체가 shared cluster에서 비영향 작업이
아니며 namespace opt-in만으로 다른 operator의 반응을 격리할 수 없음을 확인한다.

## Non-cordon cycle

두 번째 실행은 공개 chart와 같은 image digest를 사용하되 Node의 cordon, taint, drain을
전혀 변경하지 않았다. test Pod의 nodeSelector만 `server-02`로 설정했다.

| 검사 | 결과 |
|------|------|
| Helm install → Controller/Node Ready | 4.680 s |
| 순차 dynamic provisioning | 3/3 성공, 6.519/6.654/7.571 s |
| provisioning 평균/중앙값 | 6.915 s / 6.654 s |
| main PVC와 첫 Pod Ready | 7.315 s |
| 같은 PVC/PV/handle의 republish | 10/10 성공 |
| Pod create → Ready republish | 1.988–3.027 s, 평균 2.805 s, 중앙값 3.002 s |
| 데이터 보존 | 10회 뒤 manifest SHA-256 일치 |
| owner | 모든 반복에서 `server-02` |
| 사용 중 Helm uninstall | 거부, CSI workload와 데이터 유지 |
| blocker 제거 뒤 같은 uninstall 재시도 | 성공 |

republish 값은 기존 Pod 삭제가 끝난 뒤 새 Pod를 생성한 시점부터 잰 값이다. 기본 Pod
termination grace 약 30초는 포함하지 않는다. 애플리케이션 관점의 delete-to-ready RTO는
shutdown 정책과 republish 시간을 함께 측정해야 한다.

### Data profile

- 512 MiB 단일 파일
- 4 KiB 파일 5,000개
- 정렬한 file checksum manifest:
  `b516ba8fad4e87cf3534b9865580cb0693da84537c730c2da70e4c3566419063`

| 관찰 | wall time | 환산값 |
|------|-----------|--------|
| 512 MiB write + 마지막 fsync | 613 ms | 약 835 MiB/s |
| 첫 sequential read | 203 ms | 약 2,522 MiB/s |
| 두 번째 sequential read | 191 ms | 약 2,681 MiB/s |
| 4 KiB 파일 5,000개 생성 + 마지막 sync | 6.344 s | 약 788 files/s |

read는 page cache를 제거하지 않은 warm-cache 관찰이고, 작은 파일은 각 파일마다 fsync하지
않고 마지막에 한 번 sync했다. 따라서 disk의 cold-read나 per-operation durable IOPS로
해석하지 않는다. 같은 구간의 `server-02` iostat에는 최대 약 524,592 kB/s write와
33.7% device utilization이 보였지만 shared-node background I/O가 포함된다.

### ShiftPV component samples

1초 간격 `kubectl top pod --containers` 표본의 관찰 피크다.

| container | CPU | memory |
|-----------|-----|--------|
| `shiftpv-controller` | 17m | 10 MiB |
| `csi-provisioner` | 15m | 13 MiB |
| `shiftpv-node` | 9m | 9 MiB |
| `node-driver-registrar` | 3m | 4 MiB |
| `liveness-probe` | 4m | 6 MiB |

이 cycle은 provisioning과 same-node republish만 포함하므로 rsync helper의 CPU/memory와
cross-node network 비용을 포함하지 않는다. chart 기본 resources가 비어 있어 이 값이
보장된 request/limit도 아니다.

## Mounted-filesystem one-way mobility cycle

같은 공개 chart로 두 node에 각각 8 GiB sparse image를 만들고 별도 ext4 loop filesystem을
실제 mount했다. `fstab`은 변경하지 않았고 다음 임시 mount만 Pool로 등록했다.

```text
/var/tmp/shiftpv-home-mobility-20260905-140446.img
  -> /mnt/shiftpv-home-mobility-20260905-140446
```

loop filesystem은 실제 mount point 경계를 검증하지만 backing file은 여전히 각 node의 root
NVMe에 있다. 물리적으로 분리된 production data disk의 성능 표본은 아니다.

CloudNativePG primary가 `pg-home-2@server-02`인 상태에서 test volume을 `server-01`에 먼저
배치했다. 사용자 nodeSelector를 Deployment template에서 제거하고 ShiftPV owner pin으로
재생성한 뒤, primary가 없는 source `server-01`만 cordon해 `server-02`로 한 번 이동했다.

### Dataset and result

- 1 GiB 단일 파일과 4 KiB 파일 10,000개
- 전체 크기: 1,114,701,836 bytes, 약 1,063.06 MiB
- manifest SHA-256:
  `bf2ad2404c1d1c4d101f3578ab537cf034fd9af41c48cbe4c0cfed2f8b7a70b8`
- source/destination filesystem: ext4 loop mount
- source/destination physical link: 1 Gbps

| 관찰 | 결과 |
|------|------|
| cordon → Move 생성 | 4.232 s |
| cordon → `Succeeded` | 94.939 s |
| 기존 Pod Ready 상실 → destination Pod Ready | 72.019 s |
| `Copying` 관찰 구간 | 22.067 s |
| copy/checksum 구간 유효 처리량 | 약 48.17 MiB/s |
| 전체 이동 기준 유효 처리량 | 약 11.20 MiB/s |
| source `enp1s0` TX 증가 | 1,258,338,250 bytes |
| destination `enp1s0` RX 증가 | 1,216,614,338 bytes |

네트워크 delta에는 같은 node의 background traffic도 포함된다. source rsync Pod의 관찰 피크는
261m CPU와 3 MiB memory였다. 1초 metrics-server 표본이 짧게 실행된 destination copy Job을
잡지 못했으므로 copy 전체의 CPU peak로 해석하지 않는다.

### Phase timeline

| phase | 다음 phase까지 |
|-------|---------------:|
| `Pending` | 2.633 s |
| `Locking` | 3.448 s |
| `Evicting` | 2.711 s |
| `WaitingForUnpublish` | 2.613 s |
| `WaitingForReplacement` | 3.431 s |
| `WaitingForDestination` | 5.269 s |
| `Copying` | 22.067 s |
| `Promoting` | 6.218 s |
| `Committing` | 2.612 s |
| `WaitingForDestinationPublish` | 31.527 s |
| `CleaningSource` | 7.888 s |

가장 긴 단일 구간은 rsync/checksum copy가 아니라 destination publish 대기였다. 현재 FSM은
Kubernetes scheduler가 destination을 고르게 하기 위해 copy 전에 Placement Hold를 해제한다.
이때 destination에 배치된 Pod의 `NodePublishVolume`은 owner commit 전 `Moving` 상태를 보고
`FailedPrecondition`으로 닫힌다. 같은 시간대 `server-02` kubelet journal에서 mount 실패 뒤
재시도가 약 0.5, 1, 2, 4, 8, 16, 32초 간격으로 늘어난 것을 확인했다. 마지막 실패는
05:09:52.324 UTC, 다음 재시도는 05:10:24.324 UTC였고 Pod는 05:10:25.175 UTC에 Running이
됐다. 따라서 이 baseline의 31.527초 구간은 CSI 오류를 반복한 kubelet volume-operation
backoff가 지배했다.

### Correctness and cleanup

- Move는 `Succeeded`, Volume은 `Ready`, owner/published node는 `server-02` 하나였고
  `activeMove`는 비었다.
- PVC UID, PV 이름과 CSI volume handle은 유지됐다.
- destination checksum은 source와 같았다.
- destination final이 존재하고 source final은 사라졌으며 source의 move별 retired path가
  존재한 것을 cleanup 전에 확인했다.
- cordon 전후 non-test Pod의 UID/node/Ready/restartCount diff는 비었다.
- CNPG primary는 계속 `pg-home-2`, ready instances는 2였고 두 instance restartCount도
  각각 3/2로 유지됐다.
- 종료 뒤 두 ext4 mount를 unmount하고 loop device, sparse image와 mount directory를
  제거했다. Helm release, CRD, namespace, StorageClass, PV/PVC/Move/Volume/Pool 잔여물은
  0개였고 두 node는 Ready/schedulable이었다.

## Destination publish A/B validation

같은 commit을 기준으로 Node Plugin이 `Moving`을 즉시 오류로 반환하는 대신 CSI 호출
context 안에서 `Ready`를 기다리도록 변경한 로컬 `linux/amd64` combined image를 만들었다.
공개 chart `0.1.2`의 템플릿과 기본 2초 mobility interval은 그대로 두고 Controller, Node,
helper image만 `shiftpv-perf:20260905-145700`으로 교체했다. `Moving` 중에는 mount하지 않고,
500ms 간격으로 상태를 다시 읽었다. `Ready` 전환 뒤 owner를 다시 확인하므로 fail-closed 권한
조건은 유지했다. 이 방식과 후속 per-request watch 시도는 publish 대기 수치를 낮췄지만
node-container 재시작 중 진행 중인 CSI publish가 kubelet의 incomplete volume metadata를
남기는 문제가 재현돼 제품 구현으로 채택하지 않았다. 따라서 아래 표는 폐기된 실험의 A/B
결과이며 현재 제품 성능으로 간주하지 않는다.

baseline과 같은 1,114,701,836-byte dataset과 checksum으로 `server-01`에서 `server-02`로
한 번 더 이동했다.

| 관찰 | 공개 chart baseline | 대기 방식 | 변화 |
|------|-------------------:|----------:|-----:|
| cordon → Move 생성 | 4.232 s | 4.434 s | +0.202 s |
| cordon → `Succeeded` | 94.939 s | 66.906 s | -28.033 s (-29.5%) |
| Pod Ready 상실 → destination Ready | 72.019 s | 41.014 s | -31.005 s (-43.1%) |
| `Copying` 관찰 구간 | 22.067 s | 22.556 s | +0.489 s |
| `WaitingForDestinationPublish` | 31.527 s | 2.631 s | -28.896 s (-91.7%) |

최적화 구간의 `server-02` kubelet journal에는 해당 volume의 `FailedMount` 또는
`NodePublishVolume` 실패가 없었고, image pull 없이 destination Pod가 Running으로
관찰됐다. copy 구간은 사실상 같고 publish 대기 감소량이 전체 이동 감소량과 거의 같으므로,
이번 변경은 측정 대상 병목을 직접 제거했다. 다만 각 방식이 1회 표본이므로 분산이나 수치 SLO를
주장하지 않는다. 이어진 node-restart 반증 때문에 이 최적화 자체도 채택하지 않는다.

### Rejected experiment correctness and cleanup

- checksum 전후 값은
  `bf2ad2404c1d1c4d101f3578ab537cf034fd9af41c48cbe4c0cfed2f8b7a70b8`로 같았다.
- Move는 `Succeeded`, Volume은 `Ready`, owner와 유일한 published node는 `server-02`였고
  `activeMove`는 비었다.
- source final은 없고 destination final은 있었으며, cordon 전후 non-test Pod diff는 비었다.
- Helm release, namespace, CRD, StorageClass, loop mount, sparse image와 임시 directory를
  제거한 뒤 두 node가 Ready/schedulable임을 확인했다.
- 실험 evaluator는 destination publish 5초 이하와 downtime 45초 이하를 요구하며 각각
  2.631초와 41.014초로 통과했다. 전체 `make verify`도 통과했고 statement coverage는
  84.6%였다.

## Helm lifecycle focus

별도 64 MiB PVC로 uninstall 흐름을 다시 확인했다.

1. 살아 있는 PV/PVC/`ShiftPVVolume` 때문에 pre-delete Job이 실패했고 정확한 blocker를
   로그로 출력했다.
2. Helm release status는 `uninstalling`이 됐지만 Controller 1/1, Node DaemonSet 2/2,
   lifecycle webhook과 mounted data는 유지됐다.
3. test PV만 `Delete`로 바꾸고 Pod/PVC/Volume 및 Pool을 제거했다.
4. 같은 `helm uninstall`을 재시도해 release를 정상 제거했다.

따라서 Helm mode의 운영 계약은 거부 뒤 status가 `deployed`인 것이 아니라, 보호 리소스와
CSI service가 유지되고 dependency 제거 뒤 같은 uninstall이 완료되는 것이다.

## Result and limits

- 공개 chart 설치, 3회 provisioning, 10회 same-node republish, checksum, fail-closed
  uninstall과 재시도는 통과했다.
- non-cordon cycle 시작/종료의 비테스트 active Pod snapshot은 UID, node, Ready,
  restartCount까지 동일했다.
- 기존 default StorageClass `microk8s-hostpath`의 UID와 default annotation은 유지됐다.
- 종료 뒤 ShiftPV Helm release, CRD, namespace, StorageClass, PV/PVC와 두 node의 정확한
  임시 경로는 모두 0개였다. 두 node는 Ready/schedulable이었다.
- cross-node mobility는 현재 primary가 없는 source에서 destination 방향으로 한 번
  측정했다. 반대 방향, 반복 표본, checksum second-pass 단독 비용과 concurrent Move는
  측정하지 않았다. primary node cordon은 다른 operator를 실제로 자극했으므로 전용
  node/cluster 또는 승인된 maintenance window에서만 측정한다.

첫 baseline의 표본 수와 cache/filesystem 조건으로는 수치 SLO를 정할 수 없다. 현재 spec은
정상 I/O가 direct bind mount라는 경계, lifecycle 지표의 정의와 환경 기록 의무만 규정한다.
