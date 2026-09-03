# Source Layout

ShiftPV Go 코드는 repository root에 flat하게 두지 않고 `src/*` 아래에서 역할과
실행 단위별 tree로 관리한다. 현재 CSI provisioning과 automatic cordon mobility에
필요한 package만 둔다.

```text
ShiftPV/
├── go.mod
├── go.sum
├── Makefile
├── build/package/Dockerfile
├── src/
│   ├── cmd/
│   │   ├── controller/main.go
│   │   └── node/main.go
│   ├── csi/
│   │   ├── server/server.go
│   │   ├── identity/service.go
│   │   ├── controller/service.go
│   │   └── node/service.go
│   ├── kubernetes/
│   │   ├── helperpod/runner.go
│   │   └── volumeapi/registry.go
│   ├── mobility/
│   │   ├── admission/handler.go
│   │   ├── controller/
│   │   │   ├── reconciler.go
│   │   │   ├── observe.go
│   │   │   ├── actions.go
│   │   │   └── resources.go
│   │   └── fsm/fsm.go
│   ├── node/mount/bind.go
│   └── volume/
│       ├── id.go
│       └── path.go
├── charts/shiftpv/
│   ├── Chart.yaml
│   ├── values.yaml
│   ├── values.schema.json
│   ├── crds/
│   └── templates/
│       ├── controller/
│       ├── node/
│       └── storage/
└── test/e2e/kind/
    ├── run.sh
    └── mobility/
        ├── README.md
        ├── run.sh
        └── manifests/
```

## Boundary rules

- `src/cmd/*`는 flag, dependency wiring, process lifecycle만 담당한다.
- `src/csi/*`는 CSI request/response, status code, capability 계약을 담당한다.
- `src/kubernetes/helperpod`는 node-local directory operation을 Kubernetes Pod로
  실행하는 adapter다.
- `src/kubernetes/volumeapi`는 Pool/Volume/Move CR을 읽고 status를 CAS로 갱신하는
  Kubernetes adapter다.
- `src/mobility/fsm`은 Kubernetes client에 의존하지 않는 phase/observation/decision
  규칙만 담당한다.
- `src/mobility/controller`는 cluster 상태 관찰과 FSM action 실행만 담당한다.
- `src/mobility/admission`은 bound ShiftPV Pod의 owner pin 또는 Placement Hold mutation만
  담당한다.
- `src/node/mount`는 bind mount/unmount와 target path 제한만 담당한다.
- `src/volume`은 CSI/Kubernetes 타입에 종속되지 않는 volume ID/path 규칙을
  담당한다.
- unit test는 대상 package 옆에 둔다. cluster가 필요한 test만 `test/e2e`에 둔다.
- 외부용 Go library가 아니므로 root `pkg/`를 만들지 않는다.
- 현재 구현에 존재하지 않는 역할의 빈 package tree를 미리 만들지 않는다.
