# 0002. 사전 마운트된 로컬 filesystem만 Pool로 관리한다

- 상태: Accepted
- 날짜: 2026-09-01
- 갱신: 2026-09-02

## Context

소규모 self-managed Kubernetes cluster에서는 운영자가 노드의 디스크를 준비해
filesystem으로 마운트한 뒤 애플리케이션 데이터를 저장하는 운영 방식이 일반적이다.
ShiftPV가 디스크 검색, partition, RAID/LVM, 암호화 해제, filesystem 생성과 mount까지
소유하면 OS와 기존 운영 자동화의 책임을 침범하고 노드 의존성이 커진다.

반대로 단순히 존재하는 host directory를 Pool로 허용하면 원래 filesystem의 mount가
사라졌을 때 같은 경로 아래의 root filesystem에 새 데이터를 쓸 수 있다. 이 root
fallback은 OS 디스크 고갈과 서로 다른 데이터 집합의 생성을 일으킬 수 있으므로 경로
존재나 쓰기 가능 여부만으로는 안전한 Pool을 판별할 수 없다.

## Decision

ShiftPV는 운영자가 미리 준비해 마운트한 로컬 filesystem을 Pool로 등록하고, 그
filesystem 안의 directory-backed PersistentVolume만 관리한다.

- 각 `ShiftPVPool.spec.mountPath`는 ShiftPV가 사용하기 전에 존재하는 실제 filesystem
  mount point여야 한다.
  root filesystem의 일반 하위 directory는 Pool로 등록할 수 없다.
- 운영자는 disk, partition, RAID/LVM, `mkfs`, 암호화와 key 관리, mount, `fsck`,
  장치 교체와 부팅 후 mount 복구를 책임진다.
- ShiftPV는 filesystem이나 Pool path를 생성, 포맷, 암호화, 해제, mount, remount 또는
  복구하지 않는다.
- ShiftPV는 `<mountPath>/volumes/`와 `<mountPath>/.shiftpv/` 아래만 소유한다. 다른
  경로와 기존 데이터는 수정하지 않는다.
- `ShiftPVPool` 등록은 운영자가 해당 node의 `mountPath`가 올바른 filesystem이라는
  사실을 attest하는 운영 입력이며 node마다 달라도 된다. ShiftPV는 중복 node 등록과
  root path를 거부하고 Node가 Ready인지 확인한다.
- helper와 Node Plugin은 `hostPath.type=Directory`를 사용하므로 path가 없으면 자동
  생성하지 않고 실패한다. filesystem mount identity와 read-write 상태를 별도 agent로
  지속 검증하지 않는다.
- 이미 실행 중인 Pod의 정상 I/O 경로에는 ShiftPV controller를 넣지 않는다. Pod의
  데이터 경로는 bind mount를 통해 사전 마운트된 filesystem으로 직접 연결된다.
- LUKS 같은 block-level 암호화는 운영자가 mount 전에 구성할 수 있다. ShiftPV는
  암호화 key를 보관하거나 filesystem을 unlock하지 않으며, unlock되어 마운트된 Pool을
  다른 Pool과 같은 방식으로 관리한다.

## Consequences

- 제품 계약상 단순 host directory는 Pool이 아니며 설치 전에 실제 mount가 필요하다.
  현재 안전성은 operator registration과 hostPath directory 실패에 의존한다.
- node별 path를 런타임에 해석하기 위해 privileged Node Plugin은 host root를 container에
  노출한다. Pool CR 변경 권한은 host path 선택 권한과 같으므로 cluster storage operator로
  제한한다.
- filesystem 준비, 복구 방식과 mount path는 노드마다 달라도 된다. 해당 node의 Pool CR이
  provisioning, publish와 mobility helper가 사용하는 단일 path authority다.
- 정상 Pod I/O에는 네트워크 storage layer나 ShiftPV controller가 추가되지 않는다.
  filesystem 또는 dm-crypt 자체의 성능 특성만 데이터 경로에 남는다.
- filesystem이 같은 path에서 잘못 교체되거나 root filesystem fallback이 이미 존재하면
  ShiftPV가 이를 독립적으로 판별하지 못한다. 이 범위에서는 해당 검증이 운영자 책임이다.
