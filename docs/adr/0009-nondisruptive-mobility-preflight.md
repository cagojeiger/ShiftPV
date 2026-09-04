# 0009. 이동 전 실행 중인 consumer를 보존하는 사전 점검

- 상태: Accepted
- 날짜: 2026-09-03

## Context

source-only selector나 목적지 taint처럼 이미 관측 가능한 제약을 eviction 이후에 알아내면
data authority는 안전해도 서비스가 불필요하게 중단된다. 실행 중인 Pod의 hostname selector는
ShiftPV admission이 추가했을 수 있으므로 그것만으로 사용자 제약을 추측하면 안 된다.

## Decision

- discovery, lock 전, 최초 eviction/거부 후 재시도 전에 같은 읽기 전용 preflight를 적용한다.
- ReplicaSet(Deployment 포함)/StatefulSet owner UID와 template을 읽는다. 소유자가 없거나
  삭제 중이거나 알 수 없는 종류이면 진행하지 않는다. workload spec/replicas는 수정하지 않는다.
- template의 배치 제약과 live Pod의 추가 제약을 모두 확인한다. live hostname만 template에
  없고 admission의 owner annotation이 있으면 ShiftPV pin으로 취급해 제외한다.
- Ready/schedulable registered destination 중 nodeSelector, required node affinity,
  기존 PV node affinity, NoSchedule/NoExecute toleration을 만족하는 후보가 있어야 한다.
  Kubernetes와 같은 버전의 component-helpers를 사용하며 자체 scheduler를 만들지 않는다.
- required inter-Pod affinity/anti-affinity, hard topology spread, 외부 scheduling gate 등
  이 preflight가 판단하지 않는 강제 배치 제약은 보수적으로 보류한다.
- 한 consumer/한 PVC와 default scheduler만 대상으로 한다. matching PDB의 최신 status에서
  disruptionsAllowed가 양수여야 시작한다. 보수적 예비 검사이며 실제 Eviction API가 최종 판정한다.
- lock 전 거부는 Volume Ready와 기존 Pod를 그대로 두고 다음 reconcile에서 재평가한다.
  생성된 Pending Move도 대기하며 terminal Blocked로 고정하지 않는다.
- lock 이후 최초 eviction 전에 제약이 바뀌면 Moving 상태에서 기존 Pod를 종료하지 않고
  대기한다. 이미 요청된 eviction의 결과가 불명확할 수 있으므로 자동 unlock/rollback하지 않는다.
- eviction에 Pod UID precondition을 사용하고 consumer UID를 기록한다. 같은 이름의 replacement를
  옛 consumer로 오인하지 않는다. helper 실행·copy·commit의 기존 authority 규칙은 유지한다.

## Consequences

이 검사는 예약이 아니며 template, admission policy, PDB, node 상태는 조회 직후 바뀔 수 있다.
CPU/memory fit, soft preference, 미래 admission 결과는 kube-scheduler/관련 controller가 판정한다.
통과 뒤에도 replacement가 대기하거나 Blocked될 수 있고, 그때 기존 복구 계약을 따른다.
cordon 뒤 Succeeded를 확인하기 전에 drain/shutdown하면 안 된다. 확인할 수 없는 경우의 보류는
서비스를 직접 중단하는 것보다 우선하며, 자동으로 제약을 완화하지 않는다.
