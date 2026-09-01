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
4. Controller Pod와 owner node의 Node Plugin Pod를 강제 교체해도 실행 중 데이터와
   이후 Pod 재생성 checksum이 유지된다.
5. Pod 재생성 후 checksum이 유지된다.
6. workload를 중지하고 Helm을 제거해도 PVC, PV, reservation과 데이터가 남는다.
7. 같은 namespace와 pool root로 재설치하면 같은 데이터를 다시 mount한다.
8. 기존 기본 StorageClass가 있을 때 ShiftPV를 기본값 `false`로 설치하면 기존
   기본값이 유지되고, 명시적으로 `shiftpv`를 선택한 PVC만 ShiftPV로 provision된다.
9. 고정 worker의 pool을 inode가 고갈된 tmpfs로 가려 provisioning이
   `Unavailable`로 실패하고 reservation을 보존한 뒤, 복구 시 같은 volume으로 bind된다.
10. 같은 volume의 pool을 read-only로 바꿔 deletion이 `Unavailable`로 실패할 때
    reservation과 데이터를 보존하고, read-write 복구 후 삭제가 완료된다.

테스트는 성공과 실패 모두 cluster와 임시 host directory를 정리한다.
`KEEP_CLUSTER=1`은 로컬 실패 진단에만 사용한다.

## Linux mount integration

```bash
make linux-mount-integration
```

Linux에서만 실행하며 `sudo`와 util-linux의 `unshare`가 필요하다. 테스트 binary를
별도 mount namespace에서 실행해 실제 bind mount의 publish/idempotency/unpublish를
검증한다. 같은 namespace 안에서 UID/GID 65534인 자식 프로세스도 실행해 권한 없는
mount가 실패하고, mount와 target을 남기지 않으며 source를 보존하는지 확인한다.

권한이 필요한 실행은 이 격리된 테스트에만 한정되며 ShiftPV 제품 container의 권한이나
배포 설정을 변경하지 않는다.

## CI

[`ci.yaml`](../../.github/workflows/ci.yaml)은 pull request, `main` push와 수동 실행에서
다음 세 job을 수행한다. `main`에 합치기 전에 모두 성공해야 한다.

| Check | 포함 항목 |
|-------|-----------|
| `verify` | fast checks, 80% coverage gate와 coverage artifact |
| `linux-mount` | 격리된 실제 mount namespace, bind mount와 권한 실패 정리 |
| `kind-e2e` | pinned toolchain, image build와 전체 kind E2E; 실패 진단 artifact |
| `image-controller`, `image-node` | 독립 runtime image 빌드와 각 entrypoint smoke test |

## Container image builds

Controller와 Node Plugin 이미지는 같은 source version에서 독립적으로 빌드한다.

```sh
make image
```

[`versions/controller`](../../versions/controller)와
[`versions/node`](../../versions/node)가 각 이미지와 바이너리에 들어가는 버전의
독립 source of truth다. 기본 출력은 각 파일의 값을 tag로 사용한다. 하나만 빌드할
때는 `make image-controller` 또는 `make image-node`를 사용한다.

```sh
make image-controller CONTROLLER_VERSION=0.2.0
make image-node NODE_VERSION=0.1.3
```

현재 Helm 및 kind 전환 호환용 통합 이미지는 `make image-combined`로만 명시적으로
빌드한다. 통합 이미지 자체의 제품 버전은 없으며, 내부 두 바이너리는 각 component
version을 유지한다.

CI는 두 독립 이미지를 각각 빌드하고 기본 entrypoint의 `--help` 실행을 확인한다.

Component version file이 `main`에서 변경되고 CI가 성공하면 해당 이미지만 GHCR에
독립적으로 배포한다.

| Version source | Image | Release tag |
|----------------|-------|-------------|
| `versions/controller` | `ghcr.io/cagojeiger/shiftpv-controller:<version>` | `controller/v<version>` |
| `versions/node` | `ghcr.io/cagojeiger/shiftpv-node:<version>` | `node/v<version>` |

각 version tag와 `latest`는 `linux/amd64`, `linux/arm64`를 포함하는 multi-platform
manifest다. Release branch는 version file 변경을 준비하는 용도이며, merge된 `main`
commit의 CI 성공이 실제 이미지 배포를 trigger한다. Helm image reference 분리는 아직
별도 작업이다.

CI의 특정 실행 결과가 해당 commit의 합격 증거다. [`validation/`](../validation/README.md)의
문서는 재현 환경과 관찰 결과를 남기는 기록이며, 최신 commit의 CI 상태를 대신하지 않는다.
