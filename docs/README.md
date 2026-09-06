# ShiftPV Documentation

이 문서는 현재 구현된 ShiftPV의 결정, 동작 계약, 개발 검사와 실행 증거만 다룬다.

```text
docs/
├── README.md
├── adr/
│   ├── README.md
│   └── 0001-...md ~ 0010-...md
├── spec/
│   ├── README.md
│   ├── csi-driver.md
│   ├── storage-class.md
│   └── volume-mobility.md
├── development/
│   ├── README.md
│   ├── source-layout.md
│   └── testing.md
└── validation/
    ├── README.md
    └── <dated-evidence>.md
```

| 위치 | 책임 |
|---|---|
| [`adr/`](adr/README.md) | 현재 구조를 선택한 이유 |
| [`spec/`](spec/README.md) | 현재 코드와 Helm이 지켜야 하는 동작 계약 |
| [`development/`](development/README.md) | source 구조, 로컬 검사와 CI 합격 기준 |
| [`validation/`](validation/README.md) | 실제 환경에서 실행한 검증 증거 |

현재 구현에 없는 기능은 요구사항이나 설계 문서로 유지하지 않는다. 구조적 결정이 새로
생기거나 기존 결정을 대체할 때 ADR을 추가하고 spec과 구현을 함께 변경한다. 정식 버전 전에는
결정의 의미를 유지하는 범위에서 기존 ADR의 잘못된 표현과 문서 경계를 바로잡을 수 있다.

각 영역은 자기 질문에만 답한다. ADR은 동일한 네 목차를 사용하고, spec과 development는
도메인에 필요한 목차만 둔다. Validation은 실행 환경과 결과가 다른 과거 snapshot이므로
형식을 억지로 다시 쓰지 않고 README 색인으로 탐색한다.
