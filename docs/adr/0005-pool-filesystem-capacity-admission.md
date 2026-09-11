# 0005. Pool filesystem 총량으로 신규 할당을 제어한다

- 상태: Accepted
- 날짜: 2026-09-06
- 상세 계약: [CSI driver](../spec/csi-driver.md), [StorageClass](../spec/storage-class.md)

## Context

Pool filesystem은 다른 프로세스와 공유될 수 있다. 개별 directory 합계는 외부 소비를 반영하지
못하고, hard quota는 filesystem별 기능과 OS 설정에 의존한다. 우선 위험은 Pool 전체 고갈이다.

## Decision

```text
신규 할당 가능량 = min(Pool 논리 잔여량, statfs 물리 잔여량)
```

Pool은 ShiftPV 총예약량을 선언한다. PVC requested bytes와 승인된 이동 예약을 논리 회계에 포함하고,
`statfs`로 외부 소비를 포함한 현재 물리 여유를 확인한다. 이동은 source unpublish 뒤 실제 directory
bytes를 한 번 측정한다. 승인은 Move status에 기록한다.

## Alternatives considered

| 대안 | 절충점 |
|---|---|
| `statfs` 단독 | 아직 쓰지 않은 PVC의 약속이 물리 사용량에 나타나지 않는다. |
| directory 정기 순회 | 파일 수에 따라 비용이 커지고 외부 소비를 설명하지 못한다. |
| filesystem project quota | 강한 격리 대신 filesystem 종류와 OS 설정 범위가 좁아진다. |

## Consequences

신규 provisioning과 이동 복사는 논리 overcommit과 관측된 물리 부족을 함께 피한다. 외부 writer의
미래 소비와 requested bytes를 넘는 개별 volume write는 host filesystem 운영 정책이 담당한다.
`statfs` 승인은 filesystem block을 독점하지 않으며 이후 실제 I/O 실패가 최종 결과를 결정한다.
