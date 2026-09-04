# 0005. 정상 cordon 이동은 Placement Hold로 배치를 조정한다

- 상태: Accepted
- 날짜: 2026-09-02

## Context

ShiftPV volume은 한 owner node의 사전 마운트된 filesystem에만 authoritative data를
둔다. 운영자가 정상 node를 계획적으로 비울 때 PVC/PV identity를 유지하면서 data와
storage topology를 다른 registered Pool로 이전해야 한다. Kubernetes CSI에는
`MoveVolume` RPC가 없고 generic `kubectl drain`은 storage migration 완료를 기다리지
않는다.

## Decision

- 자동 이동의 유일한 trigger는 source `Node.spec.unschedulable=true`다.
- source Node, Node Plugin과 Pool을 정상으로 확인할 수 없으면 이동하지 않는다.
- 한 active consumer가 있는 RWO Filesystem volume만 초기 자동 이동 대상으로 한다.
- mobility opt-in namespace에서 생성한 PV만 registered Pool node 전체를 accessible
  topology로 허용한다. opt-in하지 않은 PV는 최초 owner node만 허용한다.
- Placement Coordination admission webhook은 opt-in namespace의 새 Pod만 검사하며
  fail-closed다.
  - unbound `WaitForFirstConsumer` PVC에는 Placement Hold를 적용하지 않는다.
  - owner가 Ready이고 schedulable이면 생성되는 Pod에 owner hostname selector를 추가한다.
  - owner가 cordon됐거나 사용할 수 없으면 mobility opt-in namespace에만 owner selector
    없이 Placement Hold를 적용한다.
- Controller는 volume을 `Moving`으로 먼저 잠그고 consumer를 Eviction API로 종료한다.
- Controller는 관찰, 순수 FSM 결정, 하나의 멱등 action과 CR status 저장을 주기적으로
  반복한다. 모든 이동 phase와 허용 transition은 제품 FSM에 명시한다.
- 실제 `NodeUnpublishVolume`과 published target zero를 확인하기 전에는 copy하지 않는다.
- replacement Pod는 Placement Hold 상태에서 대기한다. 안전 조건을 통과하면 Controller가
  Placement Release를 수행하고 실제 `spec.nodeName`을 목적지 결정으로 사용한다. 이
  Placement Handoff 이후의 Pod scheduling constraint는 kube-scheduler가 평가하며 ShiftPV가
  재구현하지 않는다.
- 이동 중 `NodePublishVolume`은 모든 node에서 fail-closed다.
- source와 destination helper Pod 사이에서 authenticated rsync로 staging copy를 만들고,
  검증 후 destination filesystem 안에서 rename한다.
- commit은 dynamic volume owner를 destination으로 변경하고 `Ready`로 여는 한 번의 상태
  전이이며 PV node affinity는 변경하지 않는다.
- source는 commit 전, destination은 commit 후 authoritative다. source 장애나 상태
  불명은 staging copy 승격 근거가 아니다.
- destination을 선택한 뒤에는 해당 Node가 Ready이고 Pool에 계속 등록되어 있을 때만
  copy/promotion/commit을 전진시킨다. commit 후 destination이 일시적으로 unavailable이면
  destination authority를 유지하고 source retire를 보류한 채 자동 재평가한다.
- `MutablePVNodeAffinity`나 scheduler plugin은 요구하지 않는다.

## Consequences

eviction 이전의 판정은 [ADR 0009](0009-nondisruptive-mobility-preflight.md)가 보완한다.
아래 selector Blocked 결과는 도입 당시 시험 이력이며 현재는 관측 가능한 충돌을 먼저 보류한다.

- nodeSelector, required affinity, taint/toleration과 resource fit은 replacement Pod를
  배치하는 kube-scheduler가 평가한다. candidate와 충돌하는 hostname selector나 이미
  잘못 bind된 node는 즉시 Blocked이고, 그 밖의 Unschedulable은 waiting phase에 머문다.
- Placement Hold는 Kubernetes의 `spec.schedulingGates`로 구현한다. 이 필드는 Pod 생성
  이후 항목을 다시 추가할 수 없으므로 현재 Controller는 replacement Pod를 자동 삭제하거나
  해제한 Hold를 다시 적용하지 않는다.
- Argo CD가 관리하는 Deployment/StatefulSet spec과 replica 수는 수정하지 않는다.
- bare Pod, multi-consumer, custom scheduler, multi-ShiftPV-PVC Pod와 unavailable source는
  초기 자동 이동 범위 밖이다.
- cordon은 이동을 시작하지만 node 종료 허가가 아니다. `Succeeded`를 확인한 뒤 drain과
  shutdown을 진행해야 한다.
- `Blocked`는 자동 rollback이나 자동 retry가 없는 terminal 상태다. `Succeeded`와
  `Blocked`는 reconcile에서 self-loop하며, waiting phase에는 phase-level deadline이 없다.
- Controller Deployment는 replica 1과 `Recreate` strategy로 mobility reconciler singleton을
  유지한다.
- 제품 admission, scheduler handoff, CSI retry, authenticated rsync와 dynamic owner CAS를
  `test/e2e/kind/mobility/`에서 검증했다. source-only selector는 authority를 보존한
  `Blocked`로 끝났고, 정상 이동은 `Copying`과 `Committing` 중 Controller를 강제
  재시작해도 `Succeeded`로 수렴했다. 실제 Kind storage node 중단도 `Copying`,
  `Promoting`, owner commit 이후 경계에서 authority를 보존하고 node 복귀 후 수렴했다.
