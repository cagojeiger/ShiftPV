# Development

이 디렉터리는 ShiftPV를 변경하고 검증할 때 필요한 개발자 문서를 담는다.

| 문서 | 책임 |
|---|---|
| [source-layout.md](source-layout.md) | source tree와 package 경계 |
| [testing.md](testing.md) | 로컬 검사, CI와 E2E 실행 기준 |

제품 동작은 [`spec/`](../spec/README.md), 설계 이유는 [`adr/`](../adr/README.md), 현재 commit의
검증 결과는 required CI job을 따른다.
