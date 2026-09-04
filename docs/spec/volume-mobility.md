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

## Non-disruptive preflight

discovery, lock 직전, 최초 eviction 및 거부 후 재시도 직전에 같은 read-only 점검을 한다.
ReplicaSet(Deployment 포함)/StatefulSet의 실제 owner UID와 template을 확인한다. 지원하지
않는 controller, 삭제 중인 consumer/owner, 여러 consumer/PVC, custom scheduler는 보류한다.
추가 PVC는 다른 driver여도 공동 배치 보장이 없으므로 보류한다.
Retain PV와 Volume CR이 남아 있어도 `PV.claimRef.uid == PVC.uid`와
`PVC.spec.volumeName == PV.name`이 일치하지 않거나 binding이 삭제 중이면 이동 대상에서
제외한다. namespace/PVC 이름 재사용은 동일한 볼륨 사용이라는 증거가 아니다.

- live Pod와 template의 nodeSelector/required node affinity를 모두 검사한다. template에
  없는 owner hostname pin은 `shiftpv.io/placement=owner`일 때만 live 검사에서 제외한다.
- candidate의 현재 label, Ready/cordon 상태, NoSchedule/NoExecute taint와 양쪽 toleration,
  기존 PV의 required node affinity를 검사한다. Kubernetes `component-helpers`의 node
  affinity matcher를 사용한다. Equal/Exists toleration만 사용하며 alpha 비교 operator는 제외한다.
- required inter-Pod affinity/anti-affinity, DoNotSchedule topology spread, 별도 scheduling
  gate는 사전 판정 범위 밖이므로 보수적으로 보류한다. soft preference는 scheduler에 맡긴다.
- matching PDB가 있으면 최신 observedGeneration과 양수 disruptionsAllowed가 필요하다.
  여러 matching PDB도 보류한다. unhealthyPodEvictionPolicy의 예외를 예측하지 않는 보수적
  검사이며, 실제 eviction은 UID precondition을 붙여 Eviction API/PDB가 최종 결정한다.

lock 전 불합격이면 Move를 생성하지 않고 Ready volume/기존 Pod를 유지한다. 이미 생성된
Pending Move는 reason과 함께 재평가한다. lock 후 eviction 전에 조건이 바뀌면 Moving에서
기존 Pod를 종료하지 않고 기다린다. 이때 기존 mount는 유지되지만 새 CSI publish는 닫혀
있다. 요청 결과가 불명확한 eviction을 자동 취소했다고 가정해 unlock하지 않는다.

`NoCompatibleDestination`, `DisruptionBudgetDenied` 등의 이유는 기존 Move.status.reason,
discovery에서 건너뛴 경우 controller verbosity 2 로그로 확인한다. 후보가 추가되거나
PDB가 허용하면 다음 reconcile에서 다시 시도한다. terminal Blocked를 새로 만들지 않는다.

preflight는 reservation이 아니다. CPU/memory fit, 추가 admission 변경, 동시 rollout,
조회 이후 PDB/node/template 변경은 이후 배치를 막을 수 있다. 이런 경우에도 scheduler를
우회하지 않는다. replacement 지연에는 기존 waiting/Blocked 복구 규칙을 적용한다.
설정 검사와 실제 배치의 구분은 [Kubernetes node 배치 규칙](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/)을 따른다.

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
| `Pending` / 최초 eviction 전 `Locking`, `Evicting` | preflight 보류 | 기존 phase 유지, 재평가; eviction 안 함 |
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
`Blocked`는 자동 rollback/retry가 없는 terminal 이동 결과이며 source 또는 이미 commit된
destination authority를 추측해서 바꾸지 않는다. lock 전 지원되는 preflight 불합격은 대기한다.
authority/binding이 사라지는 안전 위반은 Move만 Blocked이고 아직 소유하지 않은 Ready volume은 변경하지 않는다. lock 이후
또는 commit 이후 실패는 Volume도 Blocked로 닫는다.

`consumerUID`는 이름이 같은 StatefulSet replacement를 기존 consumer와 구분한다. 새 이동은
lock 때 UID를 기록한다. 해당 필드가 없는 기존 in-flight Move는 종전 이름 기반 관찰을
유지하므로 이동이 없는 시점에 CRD/controller를 함께 upgrade한다.

## Explicit owner recovery

운영자가 원인을 확인한 뒤 current owner를 uncordon하고 해당 Move에 한 번의 복구 요청을
남긴다. 이 요청은 소유권을 지정하거나 변경하지 않는다.

```sh
kubectl patch shiftpvmove <move-name> --type=merge \
  -p '{"spec":{"recovery":"ResumeOwner"}}'
kubectl get shiftpvmove <move-name> -o yaml
```

`spec.recovery`는 설정 후 제거/변경할 수 없으며 같은 요청은 멱등적이다. Blocked 아닌
Move에 미리 요청하면 CRD 검증에서 거부한다. `status.phase=Blocked`와 원래 reason은 이력으로 유지하고
`recoveryPhase`, `recoveryOwner`, `recoveryReason`, `recoveryMessage`로 복구를 구분한다.

```text
Quiescing -> Verifying -> Retiring -> Resuming -> Completing -> Recovered
   old helpers   owner     non-owner    same-owner     resource/lock
   stop + wait   directory quarantine   Ready/publish cleanup
```

- current Volume owner가 source 또는 기록된 destination이어야 하며 activeMove가 일치해야 한다.
- source 및 선택된 destination은 Ready, current owner는 uncordoned여야 한다.
- PV/ClaimRef/PVC UID와 volume handle을 검증하고 foreign published node가 있으면 거부한다.
- 원래 helper Job을 UID 조건부 foreground 삭제하고 Pod 종료를 다시 관찰한다. 강제 삭제하지 않는다.
- readonly Job으로 current owner의 final directory를 확인한다. destination이면 이 Move의
  promotion marker도 일치해야 한다. 파일 내용의 무결성/백업 복원을 대신하는 검사는 아니다.
- non-owner final/staging은 `.shiftpv/aborted/<move>-final` 및 `<move>-incoming`으로
  같은 filesystem에서 rename한다. symlink, 다른 marker, 기존 quarantine 충돌은 거부하고
  데이터를 덮어쓰거나 삭제하지 않는다. 기존 정상 retired 경로도 보존한다.
  commit 전에는 생성되지 않은 destination final/staging의 부재가 정상이다. commit 후
  source final과 aborted-final이 모두 없으면 정상 cleanup의 retired directory를 확인해야 한다.
- 모든 확인 후 Ready CAS가 마운트를 열되 owner는 변경하지 않는다. activeMove는 유지한다.
- held/misplaced controller Pod만 UID 조건부 Eviction API로 교체한다. PDB를 무시하지 않으며
  workload template/replica를 수정하지 않는다. admission이 새 Pod를 현재 owner에 pin한다.
- owner publish 확인과 recovery Job 정리 후 activeMove를 비우고 Recovered로 끝낸다.
  이력 Move는 새 이동을 막지 않는다.

API 오류/모호한 authority/실패한 Job은 phase를 유지하며 recoveryReason/Message를 기록한다.
owner Ready 공개 이후의 placement/PDB 대기는 Ready를 다시 Blocked로 바꾸지 않는다.
실패한 recovery Job은 무한 재생성하지 않는다. 운영자는 로그와 경로/권한을 확인하고 원인을
해소한 뒤 해당 실패 Job을 **foreground 삭제하고 Pod 종료를 확인**해야 같은 단계가 재시도된다.
Volume status 직접 patch, helper force-delete, pool 교체/수동 파일 변경은 안전한 복구가 아니다.
binding 기록이 없는 lock 이전 실패는 볼륨을 바꾸지 않으므로 이 복구 경로의 대상이 아니다.

Recovery Job은 300초 deadline, backoff 0이며 완료 증거의 TTL을 두지 않는다. 단계 진행 후
controller가 정리한다. Kubernetes 1.35 이상에서 Job terminal condition과 Pod 종료를
사용한다. [Job 종료 규칙](https://kubernetes.io/docs/concepts/workloads/controllers/job/#terminal-job-conditions)

기존 설치에서는 새 controller 실행 전에 CRD schema를 명시적으로 갱신해야 한다. Helm의
`crds/`는 기존 CRD를 upgrade하지 않는다. [Helm CRD 제한](https://helm.sh/docs/chart_best_practices/custom_resource_definitions/)

## 운영 진단 계약

`ShiftPVMove.status`는 이동 transaction의 운영 진단 journal이다.

- `phase/reason/message`: 현재 단계, 기계가 읽는 원인, 자동 재시도 여부와 다음 안전 행동.
- `lastTransitionTime`: 이동 phase 또는 recovery phase가 마지막으로 바뀐 시각.
- `lastProgressTime`: phase 전진, owner/destination 선택, eviction 요청, helper Job 기록처럼
  transaction 증거가 마지막으로 전진한 시각. 같은 상태를 관찰하는 polling은 갱신하지 않는다.
- `recoveryPhase/recoveryReason/recoveryMessage`: 원래 Blocked 원인을 보존하면서 명시적
  ResumeOwner 복구만 별도로 설명한다. 복구 중에는 최상위 `message`도 현재 recovery를
  설명하며 `Recovered` 뒤에는 원래 Blocked reason이 이력임을 명시한다.

Pending과 Waiting phase는 기본적으로 자동 재평가된다. `Blocked`는 terminal이며 자동
rollback하지 않는다. `ObservationFailed`와 `ActionFailed`는 현재 phase를 보존하고 자동
재시도하며, 오류가 해소되면 정상 phase 진단으로 교체한다. status API까지 쓸 수 없는 장애는
CR에 남길 수 없으므로 controller log와 Kubernetes component 상태를 확인해야 한다.

phase/reason/recovery 변화는 status 저장 뒤 Kubernetes Event로 발행한다. 동일한 상태의 반복
reconcile은 Event와 진행 시간을 계속 갱신하지 않는다. Event는 보조 신호이며 CR status가
권위 있는 현재 상태다. 시간 필드는 감지/알림용이며 자동 실패나 rollback deadline이 아니다.

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

Kubernetes API 요청이 실제 반영된 뒤 응답만 timeout 또는 연결 단절로 유실될 수 있다.
Controller는 이 경우 성공을 추측하지 않고 현재 phase를 유지한다. 다음 reconcile에서
결정적 이름의 Move/helper resource, Move status와 Volume CAS 결과를 다시 읽어 이미 반영된
action은 재사용하고 반영되지 않은 action만 재시도한다. 특히 destination과 copy Job 이름을
Move status에 먼저 기록하기 전에는 copy resource를 시작하지 않는다. owner commit 응답이
유실되어도 `Ready`, destination owner, 같은 `activeMove`의 세 값이 모두 관찰되어야
commit 완료로 인정한다. 성공 정리 중 delete 또는 `activeMove` 해제 응답이 유실되면
NotFound와 이미 비어 있는 `activeMove`를 멱등 성공으로 처리한다.

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
