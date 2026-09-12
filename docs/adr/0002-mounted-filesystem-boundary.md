# 0002. Pool directory와 host filesystem 책임을 분리한다

- 상태: Accepted
- 날짜: 2026-09-01
- 상세 계약: [CSI driver](../spec/csi-driver.md), [volume mobility](../spec/volume-mobility.md)

## Context

ShiftPV는 기존 hostPath StorageClass를 대체하면서 데이터 이동과 운영 상태를 제공한다. 소규모
cluster는 별도 mount와 root filesystem 하위 directory를 모두 저장소로 사용한다.

## Decision

| 경계 | 소유자 |
|---|---|
| disk, filesystem, 암호화, mount lifecycle | cluster 운영자 |
| 기존 absolute Pool directory 등록 | storage 운영자 |
| Pool 아래 volume directory, reservation, 가용량 판단 | ShiftPV |

Pool directory는 기존 absolute directory이며 `/`보다 좁은 경계를 가진다. `spec.mountPath`는
node에서 사용할 Pool directory를 지정한다.

## Alternatives considered

| 대안 | 절충점 |
|---|---|
| block device부터 mount까지 관리 | node dependency와 OS 운영 책임이 커진다. |
| exact mount point만 허용 | 일반 directory 기반 hostPath 환경을 포괄하지 못한다. |
| directory 자동 생성 | 오타와 mount 누락을 정상 Pool로 오인할 수 있다. |

## Consequences

같은 filesystem의 외부 사용량도 신규 할당 판단에 반영된다. 별도 mount를 Pool로 쓰는 환경은
OS mount 감시를 함께 운영한다. Pool 변경 권한은 storage operator 경계에 둔다.
