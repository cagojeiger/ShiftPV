# ShiftPV Helm Contract

> **Status:** 이 파일은 현재 checkout의 Chart 구현 후보를 설명한다. 운영 gate와 별도 release가
> 끝나기 전에는 배포 승인 또는 runtime 보증으로 사용하지 않는다.

Chart는 CSI Controller/Node, 세 CRD(`ShiftPVPool`, `ShiftPVVolume`, `ShiftPVMove`), StorageClass,
admission, metrics와 fail-closed removal guard를 설치해야 한다. Pool directory와 host filesystem은
storage operator가 준비해야 한다.

## Requirements

- Kubernetes 1.35+
- Linux kernel 5.6+ on participating nodes
- node마다 기존 writable absolute non-root Pool directory
- 일반 kubelet은 `/var/lib/kubelet`; MicroK8s는 보통
  `/var/snap/microk8s/common/var/lib/kubelet`
- RWO filesystem workload

0.4는 empty install을 기준으로 한다. 설치 전 같은 driver의 PVC/PV와 ShiftPV CRD instance가 없어야
하며, 다른 contract의 resource를 자동 변환하지 않는다.

## Install

0.4 artifact가 release gate를 통과하기 전에는 이 절차를 실행하지 않는다. 승인 뒤에는 고정한 chart
version과 image digest를 사용한다.

```bash
helm repo add shiftpv https://project-jelly.github.io/ShiftPV
helm repo update shiftpv
helm install shiftpv shiftpv/shiftpv \
  --namespace shiftpv-system \
  --create-namespace \
  --version <approved-chart-version> \
  --wait
```

Repository 검증에서는 `shiftpv/shiftpv` 대신 `./charts/shiftpv`를 사용한다. Controller replica는
단일 protocol writer 계약 때문에 1이어야 한다.

MicroK8s 예시:

```bash
helm install shiftpv ./charts/shiftpv \
  --namespace shiftpv-system \
  --create-namespace \
  --set node.kubeletRootDir=/var/snap/microk8s/common/var/lib/kubelet \
  --wait
```

## Register Pools

참여 node마다 운영자가 소유한 directory를 가리키는 Pool 하나를 선언한다.

```yaml
apiVersion: shiftpv.io/v1alpha1
kind: ShiftPVPool
metadata:
  name: worker-a
spec:
  nodeName: worker-a
  mountPath: /var/lib/shiftpv
  capacity:
    limit: 500Gi
```

```bash
kubectl wait --for=condition=Ready shiftpvpool/worker-a --timeout=2m
kubectl get shiftpvpool/worker-a -o yaml
```

Ready만 보지 말고 `status.observedGeneration == metadata.generation`, inventory valid/complete, scan 시각,
filesystem capacity를 함께 확인한다. Scan 시각은 readiness evidence이며 삭제 완료의 causal fence가 아니다.
Missing/root/read-only path, stale·invalid·truncated inventory는 신규 provision과 destination selection에서
제외된다. 기존 owner authority는 그대로 보존된다.

운영 acceptance는 source/destination filesystem 조합이 rsync의 hardlink, symlink, FIFO, numeric ownership,
mode, sparse file, ACL과 xattr semantics를 보존하는지 별도 검증해야 한다. 필수 metadata를 보존할 수 없는
조합은 owner commit 전에 이동을 실패시킨다. Device node와 nested filesystem traversal은 범위 밖이다.

Chart는 directory, filesystem, mount, RAID, encryption과 backup을 만들거나 복구하지 않는다.

## StorageClass

Chart는 같은 provisioner를 사용하는 두 StorageClass를 설치한다. `shiftpv`는 일반 PVC lifecycle에 맞춰
삭제되는 기본 class이고, `shiftpv-retain`은 PVC 삭제 뒤에도 PV와 data를 보존해야 하는 workload가
명시적으로 선택한다. 둘 다 `WaitForFirstConsumer`와 RWO filesystem을 사용한다.

```yaml
storageClass:
  create: true
  name: shiftpv
  defaultClass: true
  reclaimPolicy: Delete

retainStorageClass:
  create: true
  name: shiftpv-retain
  defaultClass: false
  reclaimPolicy: Retain
```

PVC 예시:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: shiftpv-retain
  resources:
    requests:
      storage: 20Gi
```

Capacity는 hard quota가 아니다. Admission은 Pool limit, filesystem available bytes, 모든 Volume owner
hold와 미완료 Move hold를 보수적으로 계산한다. 이동 중 두 물리 copy가 있으면 두 Pool에 동시에
capacity가 잡히는 것이 정상이다.

중요한 data는 cluster default에 의존하지 않고 `shiftpv-retain`을 명시해야 한다. `Retain`은 PVC 삭제를
막지 않으며, 삭제 뒤 PV와 실제 data를 보존해 운영자의 별도 폐기 결정을 요구한다.

ShiftPV는 CSI `File` fsGroup 정책을 선언한다. Pod가 `securityContext.fsGroup`을 지정하면 kubelet이
볼륨의 그룹 소유권과 쓰기 권한을 적용하므로 비-root workload도 별도 init container 없이 사용할 수 있다.

### StorageClass lifecycle 변경

Kubernetes StorageClass의 `reclaimPolicy`는 immutable이다. 설치된 class와 Chart의 정책이 다르면 일반적인
Helm upgrade만으로 변경할 수 없다. 이 경우에는 먼저 ShiftPV PV/PVC와 진행 중인 provisioning이 없는지
확인하고, 기존 StorageClass만 삭제한 직후 Chart를 sync해 다시 생성한다. 기존 PV의 reclaim policy는
StorageClass를 다시 만들어도 소급 변경되지 않는다. Lifecycle webhook은 uninstall checker가 의존하는 ShiftPV
storage를 찾지 못했을 때에 한해 StorageClass 삭제만 허용하며, 의존 리소스가 남아 있으면 blocker 목록과 함께
삭제를 거부한다.

```bash
kubectl get pv,pvc -A
kubectl get shiftpvvolumes.shiftpv.io -A
kubectl delete storageclass shiftpv
helm upgrade shiftpv shiftpv/shiftpv --namespace shiftpv-system --version <approved-chart-version> --wait
kubectl get storageclass shiftpv shiftpv-retain
```

이 절차가 실제로 동작하는지는 `make kind-upgrade-e2e`가 확인한다. 마지막 공개 릴리스를 설치한 뒤 현재
checkout으로 in-place upgrade하고, 의존하는 storage가 남아 있을 때의 삭제 거부와 정리 후의 삭제·재생성을
그대로 재현한다.

#### Argo CD에서의 교체

Argo CD는 immutable field가 다른 StorageClass를 server-side diff dry-run으로 비교하다 실패하고, 그
Application 전체를 `ComparisonError`/`Sync Unknown`으로 두어 auto-sync와 selfHeal이 멈춘다. 이 상태는 Chart
값이나 리소스 annotation으로는 풀 수 없다. `Replace=true,Force=true` sync-option은 매 sync마다 두 class를
delete/create하므로 ShiftPV storage가 하나라도 생기면 webhook이 삭제를 거부해 이후 sync가 모두 실패한다 —
그래서 Chart는 그 annotation을 제공하지 않는다. 절차는 위와 같다: 의존 storage가 없음을 확인하고 StorageClass를
직접 삭제한 뒤 Application을 refresh/sync하면 Chart가 다시 만든다. 삭제부터 재생성까지 cluster에 기본
StorageClass가 없으므로 `storageClassName`을 생략한 PVC는 그 사이 Pending에 머문다. Argo CD는 같은 revision의
실패한 sync를 자동으로 재시도하지 않으니 삭제 뒤에는 sync를 명시적으로 트리거한다.

### Retain volume 회수

`shiftpv-retain`의 PVC를 삭제하면 PV는 `Released`로 남고 `ShiftPVVolume`과 node의 data directory도
보존된다. 그 data를 폐기하기로 결정했으면 PV의 reclaim policy를 `Delete`로 바꾸는 것으로 끝난다. PV
controller가 PV를 삭제하고 CSI `DeleteVolume`이 ShiftPV의 일반 cleanup 경로(cleanup Job, copy marker와
placement marker 제거, receipt 기록, `ShiftPVVolume` 삭제)를 그대로 태운다.

```bash
kubectl get pv <pv-name> -o jsonpath='{.status.phase} {.spec.csi.volumeHandle}{"\n"}'   # Released 확인
kubectl patch pv <pv-name> -p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}'
kubectl get pv <pv-name>; kubectl get shiftpvvolume <volume-id>                        # 둘 다 NotFound가 되면 완료
```

`ShiftPVVolume`의 finalizer를 직접 떼거나 node에서 directory를 `rm`하지 않는다. data directory만 지우면
`.shiftpv/`의 copy marker가 남아 Pool inventory가 그 copy를 `Missing`으로 보고하고
`ShiftPVCopyNeedsReview`가 발생한다.

## Planned mobility

Settled terminal `ShiftPVMove` metadata is retained for seven days by default
and then removed with a UID precondition. Configure whole-hour retention with
`mobility.journalRetention`. This policy never deletes copy data and never
removes unresolved, active, or finalizer-protected journals.

Mobility admission을 사용할 workload namespace만 opt in한다.

```bash
kubectl label namespace my-workload shiftpv.io/admission=enabled
```

계획 이동 전 다음을 확인한다.

- source와 destination node/Pool이 Ready이고 inventory가 fresh·complete다.
- workload selector, required affinity, taint/toleration이 destination을 허용한다.
- PDB가 consumer 중단과 replacement scheduling을 허용한다.
- Volume이 Ready이고 active Move나 deletionTimestamp가 없다.
- source data backup 또는 workload 수준 복구 절차가 준비돼 있다.

Source cordon 뒤 Controller가 cold move를 시작한다. 진행은 parent resource만으로 판정한다.

```bash
kubectl get shiftpvvolumes
kubectl get shiftpvmoves
kubectl get shiftpvmove <move-name> -o yaml
kubectl get shiftpvpools -o yaml
```

| 판정 | 의미 |
|---|---|
| commit 전 대기/실패 | source owner와 data를 보존하고 resume 또는 abort |
| owner CAS 확인 | commit 완료; destination만 authoritative |
| destination 실제 publish 확인 | `publishedNodes`와 destination Pool inventory의 actual mount proof가 모두 맞으면 source cleanup을 시작할 수 있음 |
| purge API receipt + fresh absence | source hold와 Move finalizer를 해제할 수 있음 |
| `Blocked` / cleanup `NeedsReview` | Move 또는 cleanup evidence 모순; 자동 destructive action 없음 |

Node outage 중에는 관련 journal/finalizer를 제거하지 않는다. Node가 같은 identity로 돌아오면 동일
transaction을 재개한다. 영구 authoritative disk/node 손실은 제품 복구 범위 밖이다.

## Delete a retained volume

`Retain` PVC/PV의 폐기는 workload I/O를 멈추고 exact identity를 확인한 뒤 CSI `DeleteVolume`에
위임한다.

1. PV driver, claim UID, volume handle을 확인한다.
2. Volume에 active Move가 없는지 확인한다.
3. Pod 종료와 owner node의 실제 unpublish를 확인하고 필요한 data를 backup한다.
4. 확인한 PV의 reclaim policy를 `Delete`로 변경한다.
5. Volume deletion journal, exact purge receipt, fresh absence와 Volume/PV 삭제를 확인한다.

```bash
kubectl get pv <pv-name> -o yaml
kubectl get shiftpvvolume <volume-handle> -o yaml
kubectl patch pv <pv-name> --type=merge \
  -p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}'
kubectl wait --for=delete pv/<pv-name> --timeout=10m
kubectl wait --for=delete shiftpvvolume/<volume-handle> --timeout=10m
```

Deleting Volume의 finalizer는 durable intent, exact executor, local/API purge receipt와 이후 generation의
absence proof가 모두 확인될 때만 해제된다. Intent 없이 path가 사라졌거나 identity가 다르면 cleanup
subjournal이 `NeedsReview`로 남는다.

## GC and review

자동 GC는 Volume/Move journal로 정확히 설명되는 target에만 적용한다. Inventory에서 발견한 unknown,
unmarked, parent 없는 copy는 report-only다. Timestamp, 나이, metric, Job 성공만으로 삭제를 승인하지
않는다.

```bash
kubectl get shiftpvvolumes -o yaml
kubectl get shiftpvmoves -o yaml
kubectl get shiftpvpools -o yaml
kubectl get events --all-namespaces \
  --field-selector reportingComponent=csi.shiftpv.io
```

`Blocked` 또는 cleanup `NeedsReview`에서는 current owner, Pool/Volume/Move UID, copy/operation ID, 실제
mount와 marker를 먼저 대조한다. 모순 상태에서 finalizer를 강제 제거하거나 host path를 직접 지우는 절차는
지원하지 않는다.

## Metrics

Metrics는 선택 기능이며 authority가 아니다.

```yaml
metrics:
  enabled: true
  snapshotInterval: 30s
  serviceMonitor:
    enabled: true
  prometheusRule:
    enabled: true
```

기본 Alert는 metrics snapshot 실패·staleness, invalid Pool accounting, invalid/truncated inventory,
cleanup `NeedsReview`, orphan/missing/unsafe copy observation을 알린다. Dashboard는 이 신호와 Pool
capacity, Volume/Move phase, mobility deferral, CSI 오류·latency를 함께 보여 준다. 복구/삭제 판단은
반드시 CR journal과 현재 node evidence로 다시 확인한다.
[Metrics contract](../../docs/spec/metrics.md)에 bounded-cardinality 규칙이 있다.

## Pool removal

Pool 삭제는 신규 allocation과 destination selection을 먼저 막는다. Finalizer는 다음 조건이 모두
충족될 때만 exact Pool UID를 release한다.

- 삭제 요청 이후 generation의 inventory가 valid·complete이며 copy가 없다.
- Pool을 참조하는 Volume owner와 non-terminal Move/cleanup journal이 없다.
- 해당 Pool의 capacity hold가 없다.
- node-local lock 안에서 directory empty와 Pool identity를 재확인했다.

Node 또는 API가 unavailable이면 기다린다. Unknown orphan이 있으면 자동 삭제하지 않으므로 운영자가
데이터와 identity를 해결하기 전까지 Pool 삭제도 완료되지 않는다.

## Uninstall

정상 uninstall은 먼저 provisioning을 quiesce하고 fresh inventory를 요청한 뒤 dependency를 검사한다.
다음 중 하나라도 있으면 fail closed한다.

- ShiftPV class를 사용하는 PVC/PV
- `ShiftPVVolume`
- non-terminal `ShiftPVMove` 또는 cleanup journal/finalizer
- 어느 Pool의 physical copy 또는 capacity hold
- stale, invalid, incomplete Pool observation
- Kubernetes API read failure

```bash
helm uninstall shiftpv --namespace shiftpv-system
```

Emergency `--no-hooks`와 admission 제거는 보증을 우회한다. Metadata와 host data를 자동 정리하지 않으며,
동일 installation identity와 Pool 선언을 복원하기 전에는 workload를 재개하지 않는다.

## Verification

Chart release는 deterministic render/lint, CRD schema, RBAC, install/upgrade/uninstall, CSI lifecycle,
mobility node/process restart, cleanup fault, real-node durability와 soak gate를 통과해야 한다.
구체적인 명령과 증거 경계는 [Testing](../../docs/development/testing.md)이 소유한다.
