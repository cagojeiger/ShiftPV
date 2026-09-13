# 0005. Pool filesystem 총량으로 신규 할당을 제어한다

- 상태: Accepted
- 날짜: 2026-09-06
- 적용 범위: 0.4 target contract
- 상세 계약: [CSI driver](../spec/csi-driver.md), [StorageClass](../spec/storage-class.md)

## Context

Pool filesystem은 다른 프로세스와 공유될 수 있다. 개별 directory 합계는 외부 소비를 반영하지
못하고, hard quota는 filesystem별 기능과 OS 설정에 의존한다. ShiftPV가 보증해야 하는 범위는
Pool 전체 고갈을 피하면서 자기 Volume과 Move가 만든 물리 copy를 빠뜨리지 않는 것이다.

## Decision

```text
신규 할당 가능량 = min(Pool 논리 잔여량, statfs 물리 잔여량)
```

Pool은 ShiftPV 총예약량을 선언한다. PVC requested bytes와 승인된 이동 예약을 논리 회계에 포함하고,
`statfs`로 외부 소비를 포함한 현재 물리 여유를 확인한다.

`ShiftPVVolume`은 현재 owner의 예약을 가진다. Move는 commit 전에는 destination copy hold를, commit
후에는 source cleanup hold를 가진다. Hold는 local path 변화만으로 풀지 않고, API purge receipt와
fresh absence observation이 모두 정산된 뒤에만 release한다. Unknown orphan은 capacity pressure의
증거로 보고하지만 자동 삭제하지 않는다.

## Alternatives considered

| 대안 | 절충점 |
|---|---|
| `statfs` 단독 | 아직 쓰지 않은 PVC의 약속이 물리 사용량에 나타나지 않는다. |
| directory 정기 순회만 사용 | 파일 수에 따라 비용이 커지고 외부 소비를 설명하지 못한다. |
| filesystem project quota | 강한 격리 대신 filesystem 종류와 OS 설정 범위가 좁아진다. |
| local path absence로 예약 해제 | crash window나 관측 race에서 실제 cleanup 정산을 건너뛸 수 있다. |

## Consequences

신규 provisioning과 이동 복사는 논리 overcommit과 관측된 물리 부족을 함께 피한다. 일시적으로 source와
destination을 모두 잡는 hold가 생기므로 이동 중 사용 가능량은 보수적으로 보인다. 외부 writer의 미래
소비와 requested bytes를 넘는 개별 volume write는 host filesystem 운영 정책이 담당한다.
