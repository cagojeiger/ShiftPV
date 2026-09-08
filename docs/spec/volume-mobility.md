# Volume Mobility Contract

ShiftPV는 정상인 cordon owner의 authoritative directory를 다른 Pool로 옮긴다. PVC, PV와 CSI
volume handle은 유지하고 workload I/O를 멈춘 cold migration으로 실행한다.

## Responsibility

```mermaid
flowchart LR
    K[Kubernetes<br/>workload + scheduler] --> P[Placement Hold]
    P --> M[ShiftPV Move FSM]
    M --> R[authenticated rsync]
    R --> O[owner commit]
    O --> K
    O --> D[source purge]
```

| 관심사 | 소유자 |
|---|---|
| replacement Pod 생성 | Deployment/StatefulSet controller |
| node constraint와 resource fit | kube-scheduler |
| disruption 허용 | Eviction API와 PDB |
| Hold, copy, owner commit, source purge | ShiftPV |
| workload template과 replica 수 | workload owner |

## Trigger and supported input

| 필수 관찰 | 계약 |
|---|---|
| Volume | Bound RWO Filesystem, `Ready`, 빈 `activeMove` |
| Source | 현재 owner Node와 Pool이 healthy이고 Node가 cordon 상태 |
| Consumer | controller-owned Pod 하나와 ShiftPV PVC 하나 |
| Namespace | `shiftpv.io/admission=enabled` |
| Destination | schedulable하고 Ready인 Pool 하나 이상 |

Controller는 운영자가 설정한 `Node.spec.unschedulable`을 관찰한다. Pod의 Pending 또는
Unschedulable status는 배치 결과이며 이동 trigger가 아니다. Source Node/Pool을 확인할 수 없는
동안 기록된 owner를 유지한다.

지원 workload는 ReplicaSet/Deployment와 StatefulSet이다. Bare Pod, custom scheduler, 여러
consumer, 한 Pod의 여러 ShiftPV PVC, 추가 co-placement volume은 자동 입력 범위 밖이다. Namespace
opt-in은 ShiftPV workload를 선택하며 cordon에 반응하는 다른 controller의 동작은 그대로 유지한다.

## Non-disruptive preflight

Preflight는 discovery, lock 직전, 최초 eviction 직전에 같은 read-only 규칙으로 실행된다.

| 검사 | 통과 조건 | 보류 조건 |
|---|---|---|
| Binding | PVC UID ↔ PV claimRef UID ↔ volume handle 일치 | 삭제·재사용·변경 중인 binding |
| Controller | live owner UID와 template 일치 | 삭제 중이거나 교체된 owner |
| Node placement | selector, required affinity, PV affinity에 맞는 candidate | 일치하는 candidate 없음 |
| Taints | source와 candidate가 현재 taint를 tolerate | NoSchedule/NoExecute 불일치 |
| PDB | matching PDB 하나, 최신 generation, 양수 allowance | denied 또는 여러 PDB로 판단 불명확 |
| Scheduling 의미 | reservation Pod로 표현 가능 | inter-Pod affinity, topology spread, resource claim, inline/ephemeral CSI |

| 세부 규칙 | 판정 |
|---|---|
| Admission이 주입한 owner hostname pin | `shiftpv.io/placement=owner`이면 사용자 제약에서 제외 |
| Node affinity | Kubernetes `component-helpers` matcher로 평가 |
| Toleration | stable `Equal`/`Exists` 의미로 평가 |
| Soft node affinity와 CPU/memory fit | kube-scheduler가 최종 판단 |
| Eviction | Pod UID precondition을 붙이고 Eviction API/PDB에 위임 |

보류 판정은 기존 Pod와 Ready volume을 유지하고 Node, Pool, Pod, owner, PDB event에서 재평가한다.
Preflight는 destination 예약이나 성공 보장이 아니다.

Discovery와 Node 갱신은 서로 다른 API 관찰이다. 이전 cordon snapshot으로 늦게 생성된 Pending Move는
현재 source가 healthy, schedulable, Ready, unlocked이면 Move UID precondition으로 정리한다. Lock 뒤
조건 변화는 기존 mount를 유지한 채 같은 phase에서 기다리며 새 CSI publish는 authority guard가 닫는다.

## Resources and authority

```text
ShiftPVPool    참여 node + 기존 Pool directory
ShiftPVVolume authoritative owner + phase + published nodes + active Move
ShiftPVMove   source-to-destination transaction + 영속 journal
```

| Move 시점 | Authoritative node |
|---|---|
| Owner commit 전 | Source |
| Owner commit 후 | Destination |

한 volume은 active Move 하나를 갖는다. `activeMove`는 첫 lock부터 source cleanup 성공까지 유지한다.
CSI volume context의 node는 최초 배치 기록이며 현재 publication은
`ShiftPVVolume.status.ownerNode`가 결정한다.

## Reconcile loop

```mermaid
flowchart LR
    OBS[API + CR + helper 관찰] --> DECIDE[순수 FSM 결정]
    DECIDE --> ACTION[멱등 action 하나]
    ACTION --> SAVE[status 또는 owner CAS 저장]
    SAVE --> EVENT[event 또는 30s safety tick]
    EVENT --> OBS
```

Node, managed Pod, helper Pod/Job, Pool, Volume, Move 변경은 하나의 coalescing event stream을 깨운다.
30초 safety tick은 watch 누락과 재연결 복구 시간을 제한한다. CSI 요청은 watch와 polling goroutine을
소유하지 않는다. Controller 재시작은 CR과 결정적 helper 이름을 다시 관찰해 같은 action으로 수렴한다.

## State machine

```mermaid
stateDiagram-v2
    [*] --> Pending
    Pending --> Locking
    Locking --> Evicting
    Evicting --> WaitingForUnpublish
    WaitingForUnpublish --> WaitingForReplacement
    WaitingForReplacement --> WaitingForDestination
    WaitingForDestination --> WaitingForCapacity
    WaitingForCapacity --> Copying
    Copying --> Promoting
    Promoting --> Committing
    Committing --> ReleasingDestination
    ReleasingDestination --> WaitingForDestinationPublish
    WaitingForDestinationPublish --> CleaningSource
    CleaningSource --> Succeeded
    Pending --> Blocked: terminal safety failure
    Locking --> Blocked: terminal safety failure
    Evicting --> Blocked: terminal safety failure
    WaitingForUnpublish --> Blocked: terminal safety failure
    WaitingForReplacement --> Blocked: terminal safety failure
    WaitingForDestination --> Blocked: terminal safety failure
    WaitingForCapacity --> Blocked: capacity failure
    Copying --> Blocked: transfer failure
    Promoting --> Blocked: promotion failure
    Committing --> Blocked: authority failure
    ReleasingDestination --> Blocked: release failure
    WaitingForDestinationPublish --> Blocked: publish failure
    CleaningSource --> Blocked: cleanup failure
    Succeeded --> Succeeded
    Blocked --> Blocked
```

| Phase | 영속 증거 | 다음 action |
|---|---|---|
| `Pending` | eligible source, consumer, candidates | Volume CAS lock |
| `Locking` | `Moving`, expected owner와 `activeMove` | UID-bound eviction |
| `Evicting` | original consumer 없음 | source unpublish 관찰 |
| `WaitingForUnpublish` | source가 `publishedNodes`에서 제거됨 | held replacement 관찰 |
| `WaitingForReplacement` | Placement Hold가 있는 replacement | placement reservation 생성 |
| `WaitingForDestination` | scheduler가 reservation node 선택 | destination 영속화 |
| `WaitingForCapacity` | source bytes와 destination admission | 승인 저장 후 copy 시작 |
| `Copying` | checksum 검증 완료 | staging promotion |
| `Promoting` | destination final과 Move marker | owner CAS commit |
| `Committing` | destination owner/Ready read-back | placement reservation 삭제 |
| `ReleasingDestination` | reservation 부재 | replacement pin과 Hold 해제 |
| `WaitingForDestinationPublish` | destination이 `publishedNodes`에 존재 | source purge |
| `CleaningSource` | source final/retired 모두 부재 | helper와 `activeMove` 정리 |

Pending과 waiting phase는 cluster 조건 변화에 따라 재평가한다. `Succeeded`와 `Blocked`는 안정적인
terminal phase다. Destination readiness가 일시적으로 사라지면 Copying부터 CleaningSource까지 현재
authority와 phase를 보존한다. 실제 copy, promotion, commit, release, publish, cleanup 실패는 원인에
맞는 Blocked reason으로 닫는다. Capacity 부족은 copy와 authority 변경 전에 Blocked로 닫는다.

`consumerUID`는 같은 이름의 StatefulSet replacement와 원 consumer를 구분한다. 기존 in-flight Move의
schema 변경은 active Move가 없는 upgrade window에서 CRD와 Controller를 함께 갱신한다.

## Explicit owner recovery

`ResumeOwner`는 Blocked Move에 기록된 current owner를 다시 연다.

```bash
kubectl patch shiftpvmove <move-name> --type=merge \
  -p '{"spec":{"recovery":"ResumeOwner"}}'
```

```mermaid
stateDiagram-v2
    [*] --> Quiescing
    Quiescing --> Verifying
    Verifying --> Retiring
    Retiring --> Resuming
    Resuming --> Completing
    Completing --> Recovered
```

| Recovery phase | Action과 증거 |
|---|---|
| `Quiescing` | 원 helper Job을 UID 조건부 foreground 삭제하고 Pod 종료 관찰 |
| `Verifying` | current owner final directory를 read-only Job으로 확인 |
| `Retiring` | non-owner final/staging을 `.shiftpv/aborted/`로 same-filesystem rename |
| `Resuming` | owner를 유지한 Ready CAS와 owner-bound workload 재생성 |
| `Completing` | owner publish 확인, recovery resource와 `activeMove` 정리 |
| `Recovered` | 같은 owner에서 정상 publication 수렴 |

| Recovery invariant | 증거 |
|---|---|
| Owner identity 유지 | Recovery는 owner를 선택하거나 변경하지 않음 |
| Binding 일치 | PVC/PV UID와 volume handle 확인 |
| Publication 단일화 | foreign published node 부재 |
| Current owner data 존재 | read-only verification Job 성공 |
| Destination authority 증명 | destination owner라면 exact Move marker 일치 |
| Non-owner data 격리 | symlink·marker·기존 quarantine 충돌 없이 rename |
| Workload disruption 준수 | UID-bound Eviction API와 PDB 사용 |

`spec.recovery`는 Blocked Move에 한 번 기록하는 immutable 요청이다. Current owner가 uncordon되고 관련
source/destination Pool이 Ready여야 한다. Commit 전 생성되지 않은 destination artifact의 부재와 commit
후 purge된 source의 부재는 정상 상태로 판정한다. Recovery는 데이터를 덮어쓰거나 자동 삭제하지 않고
검증되지 않은 artifact를 quarantine으로 보존한다.

API 오류와 불명확한 authority는 recovery phase를 유지한다. 실패한 recovery Job은 증거로 남는다.
운영자가 filesystem 또는 권한 원인을 해소하고 해당 Job을 foreground 삭제하면 같은 phase가 재시도된다.
Recovery Job은 300초 deadline, `backoffLimit=0`, completion TTL 없음으로 실행되고 단계 전진 뒤 Controller가
정리한다. CRD schema는 Helm upgrade 전에 명시적으로 적용한다.

## Operator diagnostics

| Status field | 의미 |
|---|---|
| `phase`, `reason`, `message` | 현재 transaction 상태와 다음 운영 행동 |
| `lastTransitionTime` | Move 또는 recovery phase가 마지막으로 바뀐 시각 |
| `lastProgressTime` | 영속 진행 증거가 마지막으로 바뀐 시각 |
| `recoveryPhase`, `recoveryReason`, `recoveryMessage` | 명시적 recovery journal |

CR status가 현재 상태의 source of truth다. Kubernetes Event는 phase, reason, recovery 변화를 보조한다.
같은 관찰은 timestamp와 Event를 반복 갱신하지 않는다. Observation/action API 오류는 현재 phase를
보존하고 재평가하며 status 저장 자체가 실패한 경우 Controller log가 진단 경로다. 시간 필드는 관측
정보이며 자동 rollback이나 실패 deadline을 만들지 않는다.

## Placement coordination and CSI publish guard

| Provisioning namespace | PV accessible topology |
|---|---|
| Mobility opt-in | 당시 등록된 모든 Pool node |
| 일반 namespace 또는 metadata 없음 | 최초 owner node |

Admission webhook은 opt-in namespace의 Pod CREATE를 `failurePolicy=Fail`로 처리한다.

| Pod 상태 | Mutation |
|---|---|
| Unbound ShiftPV PVC | 변경 없이 WFFC가 최초 owner 선택 |
| Ready volume과 schedulable owner | owner hostname에 pin |
| cordon/unready owner | Placement Hold 추가 |
| Moving 또는 Blocked volume | Placement Hold 추가 |
| 기존 hostname selector 충돌 또는 bound ShiftPV volume 여러 개 | admission 거부 |

Controller는 source unpublish 뒤 실제 replacement의 Hold를 유지하고 Move UID가 소유한 결정적 이름의
placement reservation Pod를 만든다. Reservation은 candidate affinity, workload node selector/affinity,
toleration, scheduler, priority, runtime class, OS, host network/port와 aggregate resource request를
복제한다. kube-scheduler가 선택한 node를 destination으로 영속화한다.

Copy부터 commit까지 reservation이 persisted destination에 계속 scheduled되어야 한다. Reservation이
사라지면 같은 destination 후보로 재생성하고 owner CAS 직전에도 exact Move UID와 node를 live read로
확인한다. Inter-Pod affinity, topology spread, resource claim, generic ephemeral/inline CSI는 preflight가
자동 입력에서 제외한다.

Owner commit read-back 뒤 reservation을 UID 조건부 삭제하고 실제 부재를 관찰한 다음 replacement를
destination에 pin하고 Hold를 해제한다. `NodePublishVolume`은 Ready, current owner, final directory를 모두
확인한다. Moving, Blocked, owner mismatch와 CR 조회 실패는 즉시 publish를 닫으며 CSI 호출 안에서 상태
변화를 기다리지 않는다.

## Copy, promotion and commit

```text
source       <source Pool>/volumes/<volume-id>/
staging      <destination Pool>/.shiftpv/incoming/<move-name>/
destination <destination Pool>/volumes/<volume-id>/
retired      <source Pool>/.shiftpv/retired/<move-name>/
```

```mermaid
sequenceDiagram
    participant S as Source Pool
    participant D as Destination Pool
    participant V as ShiftPVVolume
    participant P as Destination Pod

    S->>D: rsync -a --delete
    S->>D: checksum dry-run
    D->>D: staging → final (same-filesystem mv)
    D->>V: owner CAS → destination/Ready
    V->>P: Placement Hold 해제
    P->>V: destination publish 관찰
    V->>S: source → retired → purge
```

Source helper는 one-time password로 인증하는 read-only rsync service다. Destination은 checksum
itemized diff가 비어 있는지 확인하고 exact Move marker와 device identity를 검증한 뒤 `mv`로 promotion한다.
Partial 또는 marker가 다른 staging은 non-authoritative 상태로 남는다.

Owner commit은 `Moving`, source owner, matching `activeMove`를 전제로 한 CAS다. Commit 뒤에도
`activeMove`를 유지하고 destination owner/Ready를 read-back한 다음 workload를 해제한다. Destination
publish가 관찰되면 source를 retired로 rename하고 `rm -rf --one-file-system`으로 삭제한다. Source final과
retired가 모두 없어야 cleanup이 성공한다. 삭제 실패는 `CleanupFailed`로 닫는다.

Transfer Secret, ConfigMap, source Pod와 Service는 Move-derived deterministic name을 사용하고 성공 뒤
정리한다. Operation Job은 300초 deadline, `backoffLimit=2`, 600초 completion TTL을 사용한다.

## Safety invariants

| 순서 | 불변식 |
|---:|---|
| 1 | Source unpublish 뒤 copy |
| 2 | Verified staging 뒤 promotion |
| 3 | Promotion 뒤 owner CAS |
| 4 | Owner read-back 뒤 reservation release |
| 5 | Reservation 부재 관찰 뒤 workload release |
| 6 | Destination publish 뒤 source purge |
| 7 | Source final/retired 부재 확인 뒤 `Succeeded` |
| 8 | API 또는 authority가 불명확하면 publication과 disk action 대기 |

결정적 resource 이름과 영속 관찰이 API response-loss window를 닫는다. 다음 reconcile은 관찰된 성공을
재사용하고 미관찰 action만 멱등 재시도한다. Destination, source bytes와 capacity approval은 copy 전에
Move status에 저장한다. Owner commit 응답이 유실되면 destination, Ready, matching active Move를 함께
관찰해야 완료로 인정한다.

## Closure and limits

| 속성 | 현재 경계 |
|---|---|
| FSM closure | 모든 알려진 phase가 허용 transition 또는 terminal self-loop 선택 |
| Waiting duration | 조건 기반이며 phase deadline은 현재 계약 밖 |
| Controller availability | replica 1, `Recreate` strategy |
| Data authority | owner 하나, planned cold migration |
| Replication, failover, backup | 외부 시스템 |
| Webhook availability | opt-in Pod admission은 단일 Controller 가용성을 따름 |

Workload controller가 replacement를 만들지 않거나 scheduler resource가 부족하면 waiting phase가 계속된다.
Reservation 삭제와 workload Hold 해제 사이에 capacity가 선점되면 destination owner를 유지하고 source
cleanup은 destination publish까지 대기한다. Runtime과 restart 실행 증거는
[Validation](../validation/README.md)에 있다.
