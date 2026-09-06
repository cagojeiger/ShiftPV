# Source layout

ShiftPV Go 코드는 `src/*` 아래에서 실행 단위와 책임별로 나눈다. 현재 구현에 없는 역할의
빈 package는 미리 만들지 않는다.

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
│   ├── spec/
│   └── validation/
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
| `src/lifecycle/*` | 안전한 uninstall과 component deletion admission |
| `src/mobility/admission` | bound ShiftPV workload의 owner pin 또는 Placement Hold |
| `src/mobility/controller` | cluster 관찰, 이동 action, recovery와 diagnostics 조정 |
| `src/mobility/fsm` | Kubernetes client와 분리된 상태 결정 규칙 |
| `src/node/mount` | bind mount/unmount와 target path 제한 |
| `src/volume` | 외부 API type에 독립적인 volume ID와 path 규칙 |
| `src/webhook/certificate` | admission certificate, CA rotation과 hot reload |

Unit test는 대상 package 옆에 둔다. Cluster가 필요한 검증만 `test/e2e`, 실제 Linux mount
namespace가 필요한 검증은 `test/integration`에 둔다. 외부 Go library가 아니므로 root
`pkg/`는 만들지 않는다.
