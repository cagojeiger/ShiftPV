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
- mobility admission의 owner pin, Placement Hold 멱등성, invalid state fail-closed
- mobility FSM의 모든 phase closure, illegal transition과 action failure terminal 처리
- Move/Volume status CAS와 Controller action 재실행 멱등성
- Move의 표준 진단 message, phase/progress timestamp, 변화 기반 Event, API/action 오류 기록과
  정상 reconcile 뒤 임시 오류 제거
- uninstall guard의 PV/PVC/Volume/Move 검출, terminal Move 제외와 API 오류 fail-closed
- uninstall quiesce 중 새 CreateVolume 거부, 진행 중 CreateVolume drain/acknowledgement,
  실패 rollback과 provisioning 재개
- lifecycle admission의 read-only DELETE/dry-run 거부, UID-bound permit 승인과 API 오류
  fail-closed
- admission 인증서의 최초 생성, 멱등 reconcile, serving/CA 갱신, Secret 삭제 복구,
  dual-CA 전환 중 API 실패, unmanaged resource 거부, hot reload와 mobility 비활성화/
  재활성화 정책 전환, owner UID 검증과 uninstall 중 validation 재생성 차단
- CSI capability 광고 범위와 Markdown 내부 링크

## kind E2E

각 kind E2E 실행은 임시 작업 디렉터리의 전용 `KUBECONFIG`를 사용한다. 따라서 서로
다른 cluster name으로 기본 E2E와 mobility E2E를 동시에 실행해도 전역 kubectl context를
공유하거나 바꾸지 않는다. `KEEP_CLUSTER=1`이면 출력된 kubeconfig와 작업 디렉터리를
후속 조사에 사용한다.

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
6. ShiftPV workload가 실행 중이면 Helm 제거가 실패하고 release와 mount가 유지된다.
7. workload를 중지해도 retained PVC/PV/Volume이 남아 있으면 제거가 실패한다.
8. lifecycle `ValidatingWebhookConfiguration`을 명시적으로 제거하고 `--no-hooks`로
   긴급 제거한 뒤에도 PVC, PV, reservation과 데이터가 남는다.
9. 같은 namespace와 Pool 등록으로 재설치하면 같은 데이터를 다시 mount한다.
10. 기존 기본 StorageClass가 있을 때 ShiftPV를 기본값 `false`로 설치하면 기존
   기본값이 유지되고, 명시적으로 `shiftpv`를 선택한 PVC만 ShiftPV로 provision된다.
11. 고정 worker의 pool을 inode가 고갈된 tmpfs로 가려 provisioning이
   `Unavailable`로 실패하고 reservation을 보존한 뒤, 복구 시 같은 volume으로 bind된다.
12. 같은 volume의 pool을 read-only로 바꿔 deletion이 `Unavailable`로 실패할 때
    reservation과 데이터를 보존하고, read-write 복구 후 삭제가 완료된다.
13. 이동 copy 중 실제 ENOSPC가 불완전한 staging을 남겨도 `Blocked/CopyFailed`와 source
    authority를 유지하고, 용량 복구 후 `ResumeOwner`가 staging을 격리한다.
14. 검증된 copy 뒤 destination을 read-only로 바꾸면 promotion이
    `Blocked/PromotionFailed`로 owner commit 전에 멈추고, read-write 복구 후 같은 source로
    재개한다.

테스트는 성공과 실패 모두 cluster와 임시 host directory를 정리한다.
`KEEP_CLUSTER=1`은 로컬 실패 진단에만 사용한다.

filesystem mobility 두 경로만 빠르게 재현할 때는 별도 cluster 이름으로 실행한다.

```bash
MOBILITY_FILESYSTEM_FAULTS_ONLY=1 \
  CLUSTER_NAME=shiftpv-mobility-fs-focused \
  ./test/e2e/kind/run.sh
```

## kind mobility E2E

사전 점검 회귀는 `preflight.sh`에서 selector, required node affinity, taint, PDB 거부를
독립 실행한다. 같은 Pod UID/쓰기 가능/Ready volume/no Move를 확인하고, controller 재시작과
PDB 제거 후 자동 이동까지 검사한다. controller unit test는 API 실패, UID 재사용,
Pending/Locking/Evicting 재평가, 같은 이름의 replacement와 UID 조건부 eviction을 검증한다.

```bash
./test/e2e/kind/mobility/run.sh
```

별도 2-worker cluster에서 제품 admission, reconciler, CSI publish guard와 rsync helper를 함께
실행한다. source-only selector는 eviction 없이 Ready/기존 Pod를 유지해야 한다.
실제 staging mkdir 실패는 `Blocked/CopyFailed` 뒤 source 복구를 검증한다.
Move가 생성된 경로는 Reason/message와 transition/progress 시각을 노출하고, 같은 상태의
polling이 진행 시각을 바꾸지 않으며, phase/reason/recovery 변화가 Kubernetes Event로
남아야 한다.
정상 cordon 이동은 `Copying`과 `Committing` 중 Controller
Pod를 강제 교체해도 같은 Move가 `Succeeded`로 수렴해야 한다. PVC UID, PV, volume handle,
checksum, destination final과 source retired directory도 검증한다. Controller가 생성한
TLS Secret의 네 key, Service/CSIDriver owner reference, webhook `caBundle` 일치, restart 시
certificate 유지와 `mobility.enabled=false` 전환 시 Service/Secret/webhook이 유지되면서
webhook이 `failurePolicy=Ignore`와 항상 false인 match condition으로 비활성화되는지도
함께 확인한다.

## kind Argo CD E2E

```bash
./test/e2e/kind/argocd/run.sh
```

별도 `shiftpv-argocd-e2e` cluster에 pinned Argo CD 3.5.2와 cluster-local Helm
repository를 설치한다. 의존 storage가 없는 Application 삭제 성공, 실제 mount된
ShiftPV volume이 있을 때 `PreDelete` Job이 Running 상태로 삭제를 대기하고 lifecycle
validation이 CSI workload/StorageClass/checksum을 보존하는지, blocker 제거 후 같은
Job과 삭제 요청이 자동 완료되는지 검증한다. cluster 이름,
kubeconfig, worker directory와 image tag가 기본/mobility E2E와 겹치지 않는다.

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
다음 job을 수행한다. `main`에 합치기 전에 모두 성공해야 한다.

| Check | 포함 항목 |
|-------|-----------|
| `verify` | fast checks, 80% coverage gate와 coverage artifact |
| `linux-mount` | 격리된 실제 mount namespace, bind mount와 권한 실패 정리 |
| `kind-e2e` | pinned toolchain, image build와 전체 kind E2E; 실패 진단 artifact |
| `kind-mobility-e2e` | automatic mobility의 Blocked/Succeeded, Controller restart, 명시적 source/destination owner 복구 |
| `kind-argocd-e2e` | Argo CD Application 삭제 허용, lifecycle admission 거부, 보존과 대기 후 수렴 |
| `image-controller`, `image-node` | 독립 runtime image 빌드와 각 entrypoint smoke test |

## Container image builds

Controller와 Node Plugin 이미지는 같은 source version에서 독립적으로 빌드한다.
Controller image에는 `shiftpv-controller`와 pre-delete용 `shiftpv-uninstall-guard`가
함께 들어가며 Node image에는 두 binary를 포함하지 않는다.

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

kind 검증용 통합 이미지는 `make image-combined`로만 명시적으로 빌드한다. 통합 이미지
자체의 제품 버전은 없으며, 내부 두 바이너리는 각 component version을 유지한다. Helm
chart는 `controller.image.*`, `node.image.*`와 `mobility.helperImage`를 독립적으로 받는다.

CI는 두 독립 이미지를 각각 빌드하고 기본 entrypoint의 `--help` 실행을 확인한다.

Component version file이 `main`에서 변경되고 CI가 성공하면 해당 이미지만 GHCR에
독립적으로 배포한다.

| Version source | Image | Release tag |
|----------------|-------|-------------|
| `versions/controller` | `ghcr.io/cagojeiger/shiftpv-controller:<version>` | `controller/v<version>` |
| `versions/node` | `ghcr.io/cagojeiger/shiftpv-node:<version>` | `node/v<version>` |

각 version tag와 `latest`는 `linux/amd64`, `linux/arm64`를 포함하는 multi-platform
manifest다. Release branch는 version file 변경을 준비하는 용도이며, merge된 `main`
commit의 CI 성공이 실제 이미지 배포를 trigger한다.

CI의 특정 실행 결과가 해당 commit의 합격 증거다. [`validation/`](../validation/README.md)의
문서는 재현 환경과 관찰 결과를 남기는 기록이며, 최신 commit의 CI 상태를 대신하지 않는다.

## Published artifact smoke

```bash
./test/e2e/kind/artifact/run.sh
```

이 경로는 checkout에서 제품 이미지를 빌드하지 않는다. GitHub Pages의 chart package를
`helm repo add`로 내려받아 SHA-256을 확인하고, controller/node의 multi-platform manifest
digest를 포함한 image reference로 설치한다. 고정값은
[`artifact/versions.env`](../../test/e2e/kind/artifact/versions.env)에 있으며 공개 artifact가
실제로 존재한 뒤에만 갱신한다. mutable tag, 누락·중복 키와 잘못된 digest는 fast check에서
거부한다. 이 smoke는 공개 배포물 연결을 확인하며 source 기반 장애·이동 회귀를 대체하지 않는다.
외부 publication 상태가 PR merge gate를 흔들지 않도록 `artifact-smoke.yaml`의 수동 실행과
매일 정기 실행으로 분리한다. PR CI는 같은 lock의 형식·불변성 규칙만 fast check로 검사한다.
