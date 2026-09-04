# 0010. 이동 상태와 다음 안전 행동을 CR 상태로 공개

- 상태: Accepted
- 날짜: 2026-09-04

## Context

이동 controller는 `ShiftPVMove.status.phase`, `reason`, `message`를 기록하지만 대기 중인
상태는 message가 비어 있고 Kubernetes API/action 오류는 controller log에만 남을 수 있다.
운영자는 로그 전체를 읽지 않고도 자동 재시도인지, 안전을 위해 멈춘 상태인지, 마지막으로
언제 진행했는지를 구분할 수 있어야 한다.

## Decision

- 기존 `ShiftPVMove`를 진단의 단일 transaction journal로 사용한다. 별도 CRD, 진단
  controller 또는 제품 전용 CLI를 추가하지 않는다.
- 모든 이동 phase는 `reason`과 사람이 읽을 수 있는 `message`를 가진다. message는 자동
  재시도 여부와 다음 안전 행동을 함께 설명한다. ResumeOwner가 시작되면 recovery 진행,
  오류 또는 완료가 최상위 message도 교체해 이미 끝난 조치를 다시 지시하지 않는다.
- `lastTransitionTime`은 이동 또는 recovery phase가 바뀔 때, `lastProgressTime`은 phase나
  transaction 증거가 실제로 전진할 때 갱신한다. 같은 대기 상태의 주기적 reconcile은
  시간을 갱신하지 않는다.
- 관찰/API 오류와 action 오류는 현재 phase를 유지하고 `ObservationFailed` 또는
  `ActionFailed`로 기록한다. 다음 성공한 reconcile은 임시 오류 진단을 정상 상태 진단으로
  교체한다. 상태 API 자체가 실패하면 CR에 기록할 수 없으므로 controller log가 최후 수단이다.
- phase, reason 또는 recovery 상태가 바뀐 뒤 status 저장이 성공하면 Kubernetes Event를
  한 번 발행한다. 반복 reconcile은 동일 Event를 새로 만들지 않는다. Event 발행 실패는
  authority/FSM 진행을 되돌리지 않는다.
- `kubectl get shiftpvmove`에 Reason과 마지막 진행 시각을 표시한다. 세부 message, resource
  identity, recovery 진단은 YAML/describe에서 확인한다.
- 시간 정보는 진단일 뿐 deadline이나 자동 rollback 근거가 아니다. SP-STAB-004는 timeout,
  자동 owner 변경, 강제 삭제 또는 PDB 우회를 추가하지 않는다.

## Consequences

운영자는 Pending/Waiting과 terminal Blocked를 구분하고 다음 행동을 찾을 수 있다. status와
Event는 관측 가능성을 높이지만 데이터 무결성 증거를 대신하지 않는다. 오래된 상태는
`lastProgressTime`으로 식별할 수 있으나, 이번 결정은 임의 임계값으로 이동을 실패시키지 않는다.
