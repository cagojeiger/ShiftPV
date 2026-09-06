# 0002. 사전 마운트된 filesystem만 Pool로 관리한다

- 상태: Accepted
- 날짜: 2026-09-01
- 상세 계약: [CSI driver](../spec/csi-driver.md), [volume mobility](../spec/volume-mobility.md)

## Context

운영자가 이미 준비한 filesystem을 사용하는 환경에서 ShiftPV까지 disk 검색, partition,
filesystem 생성과 mount를 소유하면 OS 운영 책임을 침범한다. 반대로 임의의 host directory를
허용하면 mount가 사라졌을 때 root filesystem에 데이터를 쓰는 위험이 있다.

## Decision

ShiftPV는 운영자가 미리 준비해 mount한 filesystem만 Pool로 등록해 사용한다. Disk,
filesystem, 암호화와 mount lifecycle은 운영자가 책임지고, ShiftPV는 등록된 Pool 안의 자기
데이터 영역과 전체 여유 공간만 관리한다. ShiftPV는 장치를 포맷하거나 filesystem을
mount·repair하지 않지만 신규 할당 판단을 위해 등록 path의 filesystem 통계를 읽는다.

## Alternatives considered

- ShiftPV가 block device부터 mount까지 관리하면 책임과 node dependency가 크게 늘어난다.
- 임의의 host directory 허용은 root filesystem fallback을 안전하게 구분하지 못한다.
- 별도 상시 node agent는 현재 범위에 비해 구성과 운영 비용이 크다.

## Consequences

Pool 등록은 올바른 mount를 준비했다는 운영자의 명시적 선언이다. Mount가 없거나 잘못 교체된
상태를 완전히 복구하는 기능은 ShiftPV 범위 밖이며, Pool 변경 권한은 storage operator로
제한해야 한다.
