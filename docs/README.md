# ShiftPV Documentation

이 문서는 현재 구현된 ShiftPV의 결정, 동작 계약, 개발 검사와 실행 증거만 다룬다.

| 위치 | 단일 책임 |
|------|-----------|
| [`adr/`](adr/README.md) | 현재 구조를 선택한 이유 |
| [`spec/`](spec/README.md) | 현재 코드와 Helm이 지켜야 하는 동작 계약 |
| [`development/`](development/testing.md) | 로컬 검사와 CI 합격 기준 |
| [`validation/`](validation/README.md) | 실제 환경에서 실행한 검증 증거 |

현재 구현에 없는 기능은 요구사항이나 설계 문서로 유지하지 않는다. 지원 범위를
변경할 때는 새 ADR을 승인한 뒤 spec과 구현을 함께 추가한다.
