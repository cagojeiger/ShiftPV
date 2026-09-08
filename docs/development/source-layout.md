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
│   │   └── uninstall/
│   ├── mobility/
│   │   ├── admission/
│   │   ├── controller/
│   │   └── fsm/
│   ├── node/mount/
│   ├── volume/
│   └── webhook/certificate/
└── test/
    ├── docs/
    ├── e2e/kind/
    ├── integration/linux-mount/
    ├── performance/
    └── release/
```

## Package responsibilities

| 경로 | 책임 |
|---|---|
| `src/cmd/*` | flag, dependency wiring과 process lifecycle |
| `src/csi/*` | CSI service, capability, request/response와 status code |
| `src/kubernetes/helperpod` | node-local filesystem 작업용 helper Pod adapter |
| `src/kubernetes/volumeapi` | Pool, Volume, Move resource와 status persistence |
| `src/pool/capacity` | Pool filesystem statfs 값의 안전한 byte 변환 |
| `src/lifecycle/*` | 안전한 uninstall과 component deletion admission |
| `src/mobility/admission` | bound ShiftPV workload의 owner pin 또는 Placement Hold |
| `src/mobility/controller` | cluster 관찰, 이동 action, recovery와 diagnostics 조정 |
| `src/mobility/fsm` | Kubernetes client와 분리된 상태 결정 규칙 |
| `src/node/mount` | bind mount/unmount와 target path 제한 |
| `src/volume` | 외부 API type에 독립적인 volume ID와 path 규칙 |
| `src/webhook/certificate` | admission certificate, CA rotation과 hot reload |

| 검증 종류 | 위치 |
|---|---|
| package unit test | 대상 package 옆 |
| Kubernetes cluster 검증 | `test/e2e` |
| 실제 Linux mount namespace 검증 | `test/integration` |

내부 제품 코드는 `src/`, 실행 검증은 `test/`가 소유한다.
