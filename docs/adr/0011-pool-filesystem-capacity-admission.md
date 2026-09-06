# 0011. 개별 quota가 아닌 Pool filesystem 총량으로 신규 할당을 제어한다

- 상태: Accepted
- 날짜: 2026-09-06
- 상세 계약: [CSI driver](../spec/csi-driver.md), [StorageClass](../spec/storage-class.md)

## Context

ShiftPV Pool은 운영자가 이미 마운트했고 다른 프로세스도 사용할 수 있는 filesystem이다.
개별 volume directory의 사용량만 합산하면 외부 소비를 보지 못하고, 정확한 write limit는
filesystem별 quota 기능에 의존한다. 제품의 우선 위험은 개별 PVC 초과가 아니라 Pool 전체
고갈로 신규 provisioning과 계획 이동이 함께 실패하는 것이다.

## Decision

ShiftPV는 개별 volume의 실사용량이나 hard quota를 관리하지 않는다. 대신 Pool에 ShiftPV가
예약할 총량을 선언하고, 신규 PVC의 requested bytes 총예약과 Pool filesystem의 현재
`statfs` 여유를 함께 검사한다. requested bytes는 개별 write 제한이 아니라 Pool 총량 회계의
단위다. 외부 소비를 포함하는 filesystem 상태가 불명확하면 신규 할당은 진행하지 않는다.

## Alternatives considered

- `statfs`만 사용하면 아직 쓰지 않은 PVC들의 약속이 물리 사용량에 나타나지 않는다.
- volume directory를 정기적으로 순회하면 파일 수와 filesystem 특성에 따라 비용과 의미가
  불안정하고 외부 소비를 설명하지 못한다.
- XFS/ext4 project quota는 더 강한 격리를 제공하지만 현재의 일반 mounted-filesystem 범위를
  좁히고 OS 설정 책임을 추가한다.

## Consequences

ShiftPV는 Pool 단위 논리 overcommit과 현재 물리 여유 부족을 신규 provisioning과 이동 복사 전에 막는다.
이동은 source가 unpublish된 뒤 실제 directory bytes를 한 번 측정하고, destination에 아직 반영되지
않은 승인된 이동의 예약량도 함께 계산한다. 승인은 Move status에 기록해 재시작 후에도 유지한다.
하지만 승인 뒤 외부 writer의 소비나 PVC의 requested bytes 초과 쓰기는 막지 않으므로 미래
쓰기 공간을 보장하지 않는다.
