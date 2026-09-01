# ShiftPV Contracts

이 디렉터리는 현재 바이너리와 Helm chart가 구현하는 계약만 포함한다.

| 문서 | 단일 책임 |
|------|-----------|
| [csi-driver.md](csi-driver.md) | CSI RPC, topology, provision/publish와 mount 동작 |
| [storage-class.md](storage-class.md) | StorageClass, 기본 클래스 설정과 용량 의미 |
| [source-layout.md](source-layout.md) | source tree와 package 경계 |

계약 변경이 구조적 결정을 바꾸면 ADR을 먼저 추가하거나 대체한다.
