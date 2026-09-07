# 0002. 기존 filesystem의 지정 directory를 Pool로 관리한다

- 상태: Accepted
- 날짜: 2026-09-01
- 상세 계약: [CSI driver](../spec/csi-driver.md), [volume mobility](../spec/volume-mobility.md)

## Context

ShiftPV의 목적은 기존 hostPath StorageClass를 대체하면서 데이터 이동과 운영 상태를 제공하는
것이다. 일반적인 소규모 cluster는 별도 storage mount뿐 아니라 root filesystem 안의 지정
directory도 hostPath 저장소로 사용한다. ShiftPV가 disk 검색, partition, filesystem 생성과
mount까지 소유하면 OS 운영 책임을 침범한다.

## Decision

ShiftPV는 운영자가 이미 준비한 filesystem 안의 기존 absolute directory를 Pool로 등록한다.
그 directory 자체가 mount point일 필요는 없으며 root filesystem의 하위 directory도 허용한다.
다만 `/` 자체는 Pool 경계를 잃으므로 거부하고, 오타를 root filesystem에 자동 생성하지 않도록
등록 directory도 만들지 않는다.

Disk, filesystem, 암호화와 mount lifecycle은 운영자가 책임진다. ShiftPV는 등록 directory
아래의 자기 데이터 영역, 논리 reservation과 그 directory가 속한 filesystem의 현재 여유
공간만 관리한다. `spec.mountPath`라는 필드명은 호환성을 위해 유지하지만 의미는 exact mount
point가 아니라 node의 Pool directory다.

## Alternatives considered

- ShiftPV가 block device부터 mount까지 관리하면 책임과 node dependency가 크게 늘어난다.
- exact mount point만 허용하면 별도 mount가 없는 기존 hostPath 환경을 대체할 수 없다.
- 없는 directory를 자동 생성하면 오타나 mount 누락을 정상 Pool로 오인할 수 있다.

## Consequences

Pool 등록은 지정 directory와 그 기반 filesystem을 운영자가 준비했다는 명시적 선언이다.
같은 filesystem을 다른 프로세스와 공유하면 그 사용량도 신규 할당에 영향을 주며 ShiftPV가
이를 격리하거나 quota로 제한하지 않는다. Filesystem 장애나 mount lifecycle 복구는 ShiftPV
범위 밖이고, Pool 변경 권한은 storage operator로 제한해야 한다. 별도 mount directory를
Pool로 쓰는 운영에서는 mount가 사라진 뒤 아래의 기존 directory가 여전히 writable이면
일반 directory Pool과 구분할 수 없으므로 OS mount 감시도 운영자가 맡는다.
