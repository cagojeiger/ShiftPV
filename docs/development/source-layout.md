# Source layout

ShiftPV Go 코드는 `src/*` 아래에서 현재 실행 단위와 책임별로 나눈다. 각 package는 하나의
구체적인 제품 책임을 나타낸다.

## Tree

```text
ShiftPV/
├── build/package/Dockerfile
├── charts/shiftpv/
│   ├── Chart.yaml
│   ├── values.yaml
│   ├── values.schema.json
│   ├── crds/
│   └── templates/
├── docs/
│   ├── adr/
│   ├── development/
│   └── spec/
├── src/
│   ├── cmd/
│   │   ├── cleanup-check/
│   │   ├── controller/
│   │   ├── node/
│   │   └── uninstall-guard/
│   ├── csi/
│   │   ├── controller/
│   │   ├── identity/
│   │   ├── node/
│   │   └── server/
│   ├── kubernetes/
│   │   ├── helperpod/
│   │   └── volumeapi/
│   ├── lifecycle/
│   │   ├── admission/
│   │   ├── cleanup/
│   │   └── uninstall/
│   ├── metrics/
│   ├── mobility/
│   │   ├── admission/
│   │   ├── controller/
│   │   └── fsm/
│   ├── node/mount/
│   ├── pool/
│   │   ├── capacity/
│   │   └── readiness/
│   ├── volume/
│   └── webhook/certificate/
└── test/
    ├── docs/
    ├── e2e/kind/
    ├── helm/
    ├── integration/linux-mount/
    └── release/
```

## Package responsibilities

| 경로 | 책임 |
|---|---|
| `src/cmd/*` | flag, dependency wiring과 process lifecycle |
| `src/csi/*` | CSI service, capability, request/response와 status code |
| `src/kubernetes/helperpod` | node-local filesystem 작업용 helper Pod adapter |
| `src/kubernetes/volumeapi` | Pool, Volume, Move resource와 status persistence |
| `src/pool/capacity` | statfs byte 변환, 예약 집계와 공유 Pool admission lock |
| `src/metrics` | 읽기 전용 snapshot, CSI 관측과 HTTP endpoint |
| `src/pool/readiness` | node-local Pool probe와 readiness condition 갱신 |
| `src/lifecycle/cleanup` | 불변 요청·실행 식별자·완료 증거·보존 관리 |
| `src/lifecycle/cleanup/pathcheck` | 고정 경로의 오류 구분형 읽기 전용 부재 검사 |
| `src/lifecycle/admission`, `src/lifecycle/uninstall` | 안전한 uninstall과 component deletion admission |
| `src/mobility/admission` | bound ShiftPV workload의 owner pin 또는 Placement Hold |
| `src/mobility/controller` | cluster 관찰, 이동 action, recovery와 diagnostics 조정 |
| `src/mobility/fsm` | Kubernetes client와 분리된 상태 결정 규칙 |
| `src/node/mount` | bind mount/unmount와 target path 제한 |
| `src/volume` | 외부 API type에 독립적인 volume ID와 path 규칙 |
| `src/webhook/certificate` | admission certificate, CA rotation과 hot reload |

## Mobility boundaries

```text
mobility/
├── fsm/fsm.go                 순수 상태·action 결정
└── controller/
    ├── reconciler.go          실행 주기·Move 순회·recovery 분기
    ├── discovery.go           cordon 후보 관찰·Move 생성
    ├── move.go                단일 Move의 관찰→판단→실행→기록
    ├── observe.go             Kubernetes 상태와 Job 결과 관찰
    ├── actions.go             action dispatch·API 작업·helper 요청
    ├── resources.go           transfer 리소스·공통 Job 구성
    ├── cleanup.go             정리 요청 발행·Job 실행 연결·완료 확인
    ├── cleanup_lifecycle.go   정리 요청 순환 관찰·안전 조건·보존 기간
    ├── cleanup_check.go       읽기 전용 검증 Job 구성·결과 확인
    ├── diagnostics.go         journal 변경·시간·Event 기록
    ├── recovery.go            명시적 owner 복구 조정
    └── recovery_resources.go  복구 실행과 helper 종료 확인
```

FSM은 관찰값으로 `Next/Action/Reason`을 반환한다. Controller는 action을 실행하고 journal을 기록한다.
Helper는 파일 작업만 실행한다. [상태 전이와 action 계약](../spec/volume-mobility.md#state-machine)은
Spec이 소유하며 이 표는 파일 책임을 소유한다.

| 검증 종류 | 위치 |
|---|---|
| package unit test | 대상 package 옆 |
| Kubernetes cluster 검증 | `test/e2e` |
| 실제 Linux mount namespace 검증 | `test/integration` |

내부 제품 코드는 `src/`, 실행 검증은 `test/`가 소유한다.
