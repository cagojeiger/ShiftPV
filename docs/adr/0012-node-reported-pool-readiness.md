# 0012. Node가 실제 Pool directory 상태를 readiness로 보고한다

- 상태: Accepted
- 날짜: 2026-09-07
- 상세 계약: [CSI driver](../spec/csi-driver.md), [Volume mobility](../spec/volume-mobility.md)

## Context

Pool CR의 경로 형식만 검증해서는 그 경로가 실제 directory인지, 쓰기 가능한지 또는 기반
filesystem 용량을 읽을 수 있는지 알 수 없다. 특히 ShiftPV는 별도 mount뿐 아니라 root
filesystem 안의 일반 directory도 Pool로 지원한다.

## Decision

host root를 이미 보는 Node Plugin이 자기 node에 등록된 Pool 경로를 주기적으로 검사하고
표준 Kubernetes Conditions로 상태를 보고한다. 기존 directory 접근, 임시 쓰기와 정리,
filesystem capacity 조회가 모두 성공하고 최근 probe인 Pool만 신규 provisioning과 이동에
사용할 수 있다. 상태가 없거나 오래됐거나 불명확하면 새 storage action은 fail-closed한다.

기존 owner volume의 publish 권한은 `ShiftPVVolume`이 계속 결정한다. Pool readiness는 신규
할당과 계획 이동의 입장 조건이며 기존 데이터의 authority를 변경하지 않는다.

## Alternatives considered

- CR admission은 node filesystem에 접근할 수 없어 경로의 실제 상태를 검증할 수 없다.
- exact mount point 검사만 사용하면 지원 대상인 일반 directory를 잘못 거부한다.
- 첫 PVC 요청 때만 검사하면 잘못 등록된 Pool을 미리 발견하거나 이동 candidate에서 제외할
  수 없다.

## Consequences

운영자는 `ShiftPVPool`의 `Accessible`, `Writable`, `CapacityReadable`, `Ready` Conditions로
노드 경로의 준비상태와 실패 이유를 확인할 수 있다. Node Plugin이 중단되면 마지막 성공
상태도 freshness 기한 뒤 사용할 수 없게 된다. 주기적인 probe는 작은 임시 파일을
생성·sync·삭제하므로 interval은 빠른 장애 감지와 filesystem 부하 사이의 운영 절충점이다.
