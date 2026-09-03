# Volume Mobility Contract

ShiftPV mobility는 source가 정상인 계획 정비에서 동일한 PVC, PV와 CSI volume handle을
유지한 채 authoritative directory를 다른 registered Pool로 옮기는 cold migration이다.

## Responsibility

ShiftPV가 소유하는 책임은 다음과 같다.

- cordon된 owner node에서 이동 transaction 생성
- 이동 중 CSI publish 차단
- 기존 consumer eviction과 실제 unpublish 확인
- replacement Pod의 Placement Hold와 Release
- authenticated rsync copy, destination-local promotion, owner commit, source retire
- 각 관찰과 action 결과를 `ShiftPVMove.status`에 저장하고 재조정

kube-scheduler가 nodeSelector, affinity, taint/toleration과 resource fit을 평가한다.
Deployment/StatefulSet 같은 workload controller가 replacement Pod를 만든다. ShiftPV는
workload template, replica 수, PV node affinity를 수정하지 않는다.

## Trigger and supported input

Controller는 같은 관찰 규칙으로 eligibility preflight를 통과한 뒤 `ShiftPVMove`를 하나
생성한다.

- `ShiftPVVolume.status.phase=Ready`, `activeMove` 없음
- owner Node가 `Ready=True`이면서 `spec.unschedulable=true`
- source와 destination에 유효한 absolute `ShiftPVPool.spec.mountPath`가 각각 등록됨
- bound RWO Filesystem volume
- admission이 활성화된 namespace의 controller-owned consumer Pod 하나
- schedulable하고 Ready인 destination Pool 하나 이상

Pod의 `Pending` 또는 `PodScheduled=False/Unschedulable` 자체는 trigger가 아니다. source
Node/Pool을 읽을 수 없거나 Node가 NotReady이면 자동 이동을 시작하지 않는다. bare Pod,
여러 consumer, 한 Pod의 여러 ShiftPV PVC와 custom scheduler는 지원 입력이 아니다.

## Resources and authority

- `ShiftPVPool`: 참여 node와 이미 마운트된 `mountPath`를 등록한다.
- `ShiftPVVolume`: volume handle별 phase, authoritative owner, active move와 published node를
  기록한다.
- `ShiftPVMove`: 한 번의 source-to-destination transaction과 FSM 관찰 결과를 기록한다.

한 volume에는 active move가 하나다. `activeMove`는 lock부터 source cleanup 성공까지
유지된다. commit 전 source, commit 후 destination이
authoritative하다. creation-time CSI volume context의 node 값은 최초 배치 기록일 뿐 현재
authority가 아니다.

## Reconcile loop

Controller는 기본 2초 간격으로 다음 루프를 반복한다.

```text
Kubernetes API + ShiftPV CR + Job 상태 관찰
                    |
                    v
             순수 FSM Decide
                    |
                    v
       하나의 idempotent action 실행
                    |
                    v
       CR status/CAS owner 상태 저장
                    |
                    +---------------------> 다음 reconcile
```

관찰, 결정, action과 영속 상태가 제품 Controller 안에 있으므로 host-side runner가
진행 상태를 주입하지 않는다. Controller가 재시작되면 CR과 이름이 결정적인 helper
resource를 다시 관찰하고 같은 action을 안전하게 반복한다.

## State machine

```text
Pending
  -> Locking
  -> Evicting
  -> WaitingForUnpublish
  -> WaitingForReplacement
  -> WaitingForDestination
  -> Copying
  -> Promoting
  -> Committing
  -> WaitingForDestinationPublish
  -> CleaningSource
  -> Succeeded

Pending .. CleaningSource -- safety/action failure --> Blocked
Succeeded -- reconcile --> Succeeded
Blocked   -- reconcile --> Blocked
```

| Current phase | Required observation | Action and next phase |
|---|---|---|
| `Pending` | source healthy, supported consumer, candidate 존재 | volume CAS lock, `Locking` |
| `Locking` | `Moving`, expected `activeMove`, source owner | Eviction API, `Evicting` |
| `Evicting` | original consumer 없음 | `WaitingForUnpublish` |
| `WaitingForUnpublish` | source가 `publishedNodes`에서 제거됨 | `WaitingForReplacement` |
| `WaitingForReplacement` | workload controller의 replacement Pod 존재 | Placement Release, `WaitingForDestination` |
| `WaitingForDestination` | scheduler가 candidate node에 Pod 지정 | copy resources 생성, `Copying` |
| `Copying` | copy Job complete | promotion Job 생성, `Promoting` |
| `Promoting` | promotion Job complete | owner CAS commit, `Committing` |
| `Committing` | destination owner와 `Ready` read-back | `WaitingForDestinationPublish` |
| `WaitingForDestinationPublish` | destination이 `publishedNodes`에 존재 | cleanup Job 생성, `CleaningSource` |
| `CleaningSource` | source retire Job complete | transfer resource 정리, `Succeeded` |

각 non-terminal phase에는 허용된 self-transition 또는 다음 transition만 있다. source
unhealthy, scheduling constraint 충돌, copy/promotion/cleanup Job 실패는 `Blocked`로 끝난다.
`Blocked`는 자동 rollback/retry가 없는 terminal 운영 상태이며 source 또는 이미 commit된
destination authority를 추측해서 바꾸지 않는다. lock 전 eligibility가 경합으로 사라진
경우 Move만 Blocked이고 아직 소유하지 않은 Ready volume은 변경하지 않는다. lock 이후
또는 commit 이후 실패는 Volume도 Blocked로 닫는다.

## Placement coordination and CSI publish guard

mobility opt-in namespace에서 provisioning한 PV만 당시 등록된 Pool node 전체를
accessible topology로 허용한다. opt-in하지 않았거나 provisioner metadata가 없는 PV는
최초 owner node만 허용한다. placement admission webhook은 opt-in namespace의 Pod CREATE만
보고 `failurePolicy=Fail`로 동작한다. 규칙은 다음과 같다.

- unbound ShiftPV PVC: 변경하지 않아 `WaitForFirstConsumer`가 최초 owner를 선택한다.
- bound volume, Ready/schedulable owner: owner에 pin한다.
- bound volume, cordoned/unready owner: `shiftpv.io/placement-hold` Placement Hold를
  적용한다.
- bound volume, Moving/Blocked: Placement Hold를 적용한다.
- 기존 hostname selector가 현재 owner와 충돌하거나 bound ShiftPV volume이 여러 개면
  admission을 거부한다.

Controller는 source unpublish 뒤 Placement Release를 한 번 수행한다. 이 Hold는 Kubernetes
Pod Scheduling Readiness의 `spec.schedulingGates`로 구현한다. Placement Handoff 이후
kube-scheduler가 실제 Pod spec 전체를 평가하고 Controller는 `pod.spec.nodeName`을
destination으로 채택한다. candidate와 충돌하는 hostname selector 또는 candidate 밖의
binding은 promotion 전에 `Blocked`다.

CSI Node Plugin은 `ShiftPVVolume`이 `Ready`이고 현재 node가 owner이며 final directory가
있을 때만 `NodePublishVolume`을 허용한다. `Moving`, `Blocked`, owner 불일치와 CR 조회
실패는 fail-closed다.

## Copy, promotion and commit

```text
source:      <pool>/volumes/<volume-id>/
destination:<pool>/.shiftpv/incoming/<move-name>/
retired:    <pool>/.shiftpv/retired/<move-name>/
```

Controller image를 helper image로 사용한다. source와 destination helper의 hostPath는 각
node의 `ShiftPVPool.spec.mountPath`에서 해석한다. source Pod는 one-time Secret으로 인증하는
read-only rsync daemon이고 destination copy Job은 staging에 `rsync -a --delete`를 실행한
뒤 checksum dry-run 결과가 비어 있는지 확인한다. promotion Job은 move marker와 device ID를
검사한 뒤 같은 filesystem에서 `mv`로 final directory를 만든다.

owner commit은 expected `phase=Moving`, source owner와 `activeMove=<move>`를 전제로 하는
status CAS다. phase와 owner를 destination/Ready로 바꿔도 `activeMove`는 유지한다.
commit read-back 뒤 kubelet의 CSI retry가 destination publish를 기록해야 source cleanup을
실행한다. cleanup은 source를 즉시 삭제하지 않고 recoverable retired path로 rename한다.
cleanup과 transfer resource 정리가 끝나야 `activeMove`를 비우고 Move를 Succeeded로 만든다.
CSI `DeleteVolume`은 phase가 Ready가 아니거나 active move가 있으면 거부한다.

Secret, ConfigMap, source Pod와 Service 이름은 move name에서 결정되며 성공 시 제거된다.
copy/promotion/cleanup Job은 `activeDeadlineSeconds=300`, `backoffLimit=2`, 완료 후 TTL 600초다.

## Safety invariants

1. source unpublish가 확인되기 전 copy를 시작하지 않는다.
2. verified staging을 promotion하기 전 owner를 바꾸지 않는다.
3. owner CAS가 성공하기 전 destination CSI publish를 열지 않는다.
4. destination publish가 확인되기 전 source를 retire하지 않는다.
5. API/CR 상태가 불명확하면 publish와 promotion을 허용하지 않는다.
6. Controller restart는 CR status와 helper resource 관찰로 같은 action에 수렴한다.

## Closure and limits

FSM 그래프와 reconcile control loop는 닫혀 있다. 모든 알려진 phase는 허용된 transition과
action을 반환하고 `Succeeded` 또는 `Blocked`는 안정적인 terminal self-loop다. kind 검증은
두 terminal 결과와 `Copying`, `Promoting`, commit CAS 직후 `Committing` 중 Controller 강제
재시작 수렴을 확인한다.

종료 시간이 bounded라는 뜻은 아니다. workload controller가 replacement를 만들지 않거나
scheduler가 constraint/resource 부족으로 결정을 내리지 못하거나 destination publish가
계속 실패하면 해당 waiting phase에 머문다. 현재 구현은 phase-level deadline, 자동 Pod
교체, automatic rollback, leader election, replication, failover와 backup을 제공하지 않는다.
opt-in namespace는 placement webhook outage 동안 새 Pod admission이 실패한다. chart는
Controller replica를 1개로 제한하고 Deployment strategy를 `Recreate`로 고정해 upgrade 중
old/new mobility reconciler가 겹치지 않게 한다.
