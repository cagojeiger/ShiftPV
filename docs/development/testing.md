# Testing

ShiftPV의 merge 전 검증은 빠른 정적 검사와 격리된 kind E2E로 나눈다. 모든
명령은 repository root에서 실행한다.

## Fast checks

```bash
make verify
```

`verify`는 Go format/module/race/coverage/vet/build, ShellCheck, Helm lint/template과
Markdown 내부 링크를 검사한다. 제품 package statement coverage는 80% 미만이면
실패하며 `.tmp/coverage.out`과 `.tmp/coverage.txt`를 생성한다.

현재 엣지 테스트는 다음 실패 비용이 큰 경로를 포함한다.

- 잘못된 capacity, topology, access mode와 StorageClass parameter
- CreateVolume idempotency와 directory 생성 실패 후 retry reservation
- DeleteVolume 실패 시 reservation 보존
- owner node 불일치, unsafe target, read-only/raw-block publish 거부
- bind mount 멱등성, unmount 실패와 target 보존
- helper Pod 성공/실패와 항상 실행되는 cleanup
- CSI capability 광고 범위와 Markdown 내부 링크

## kind E2E

```bash
./test/e2e/kind/run.sh
```

이 테스트는 다른 서비스나 기존 cluster를 사용하지 않고 전용 3-node kind cluster를
만든다. 두 worker의 `/mnt/shiftpv`는 서로 다른 임시 host directory다.

합격 조건은 다음과 같다.

1. ShiftPV가 기본 StorageClass로 선언된다.
2. `storageClassName`이 없는 PVC가 `shiftpv`를 선택하고 Bound가 된다.
3. PV의 CSI driver가 `csi.shiftpv.io`이고 Pod가 volume을 mount해 데이터를 쓴다.
4. Pod 재생성 후 checksum이 유지된다.
5. workload를 중지하고 Helm을 제거해도 PVC, PV, reservation과 데이터가 남는다.
6. 같은 namespace와 pool root로 재설치하면 같은 데이터를 다시 mount한다.

테스트는 성공과 실패 모두 cluster와 임시 host directory를 정리한다.
`KEEP_CLUSTER=1`은 로컬 실패 진단에만 사용한다.

## CI

[`ci.yaml`](../../.github/workflows/ci.yaml)은 pull request, `main` push와 수동 실행에서
다음 두 job을 수행한다. `main`에 합치기 전에 둘 다 성공해야 한다.

| Check | 포함 항목 |
|-------|-----------|
| `verify` | fast checks, 80% coverage gate와 coverage artifact |
| `kind-e2e` | pinned toolchain, image build와 전체 kind E2E; 실패 진단 artifact |

CI의 특정 실행 결과가 해당 commit의 합격 증거다. [`validation/`](../validation/README.md)의
문서는 재현 환경과 관찰 결과를 남기는 기록이며, 최신 commit의 CI 상태를 대신하지 않는다.
