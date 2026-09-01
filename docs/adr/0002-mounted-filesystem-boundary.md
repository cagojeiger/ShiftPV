# 0002. 이미 준비된 로컬 filesystem부터 관리한다

- 상태: Accepted
- 날짜: 2026-09-01

## Context

디스크 검색, 파티션, RAID/LVM, filesystem 생성과 mount까지 CSI driver가 소유하면
운영 환경별 의존성이 커지고 기존 OS 관리 책임과 겹친다. 현재 제품에 필요한 것은
준비된 local path 아래에서 volume directory를 만드는 기능이다.

## Decision

- 운영자는 각 참여 node에 writable local filesystem 또는 directory를 준비한다.
- 모든 참여 node는 같은 설정 경로를 사용하며 기본값은 `/mnt/shiftpv`다.
- 각 node의 실제 데이터는 서로 공유하지 않는 별도 local path여야 한다.
- ShiftPV가 소유하는 데이터 경로는 `<poolRoot>/volumes/<csi-volume-id>`다.
- Helm chart는 `node.poolRoot`를 HostPath `Directory`로 mount하므로 경로가 없으면
  자동 생성하지 않고 Pod 시작을 실패시킨다.
- ShiftPV는 disk, partition, RAID/LVM, `mkfs`, mount, fsck와 장치 교체를 수행하지
  않는다.

## Consequences

- 운영자는 설치 전에 모든 참여 node의 경로와 쓰기 가능 여부를 확인해야 한다.
- 현재 구현은 filesystem type, mount identity와 실제 잔여 용량을 검사하지 않는다.
- 요청한 PVC 용량은 PV와 reservation에 기록되지만 write limit로 강제되지 않는다.
