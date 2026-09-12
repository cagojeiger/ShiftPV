# Testing

모든 명령은 repository root에서 실행한다. Pull request는 required CI job이 모두 성공한 뒤 병합한다.

## Verification map

```mermaid
flowchart LR
    FAST[make verify] --> PR[Pull request]
    LINUX[Linux mount] --> PR
    CSI[kind CSI] --> PR
    MOVE[kind mobility] --> PR
    RESTART[kind node restart] --> PR
    ARGO[kind Argo CD] --> PR
    IMG[image builds] --> PR
    PR --> MAIN[main]
    MAIN --> RELEASE[component and chart release]
```

| 계층 | 명령 | 증거 |
|---|---|---|
| Fast | `make verify` | unit, race, coverage, vet, build, release logic, link, ShellCheck, Helm |
| Linux mount | `make linux-mount-integration` | 실제 bind mount namespace와 권한 경계 |
| CSI lifecycle | `./test/e2e/kind/run.sh` | provision, publish, capacity, fault, uninstall, reinstall |
| Mobility | `./test/e2e/kind/mobility/run.sh` | preflight, FSM, transfer, recovery, source purge |
| Argo CD | `./test/e2e/kind/argocd/run.sh` | Application PreDelete와 lifecycle admission |
| 공개 artifact | `./test/e2e/kind/artifact/run.sh` | 공개 chart SHA와 multi-arch image digest |

## Evidence levels

| 계층 | 증명하는 범위 | 증명하지 않는 범위 |
|---|---|---|
| Unit + fake API / fake binder | 상태 전이, 오류 주입, 멱등성, API 객체 계약 | 실제 mount, kube-scheduler, kubelet, filesystem 효과 |
| Linux integration | 실제 mount namespace, bind/unbind, 권한·경로 경계 | Kubernetes control plane과 scheduling |
| Isolated Kind | 실제 API server, scheduler, external-provisioner, kubelet/CSI, node-container filesystem 효과 | 홈 운영체제·디스크·네트워크의 장기 특성 |
| Published artifact Kind | 공개 chart와 digest-pinned multi-arch image의 출처·설치·upgrade | 아직 배포하지 않은 source checkout |
| Home cluster | 승인된 동일 artifact의 대상 환경 동작과 정리 결과 | 다른 cluster와 영구 disk failure 일반화 |

낮은 계층의 성공을 높은 계층의 성공으로 대체하지 않는다. Home 검증은 공개 artifact와 GitOps
revision을 고정한 뒤 별도 승인으로 수행하며, 현재 checkout의 unit 성공만으로 배포 완료를 선언하지 않는다.

## Fast checks

```bash
make verify
```

| Gate | 계약 |
|---|---|
| Go | format, module integrity, race, vet, build |
| Coverage | 제품 package statement ≥ 80% |
| Shell | build/test script ShellCheck |
| Helm | lint와 deterministic template |
| Release | image/chart resolver 순서와 artifact lock |
| Docs | local Markdown link와 ADR 번호·인덱스·목차 일관성 |

Coverage artifact는 `.tmp/coverage.out`과 `.tmp/coverage.txt`에 생성된다.

Dashboard의 현재값·관측 유효기간·결측 처리와 PrometheusRule의 firing·해제는
`bash test/helm/dashboard/run.sh`로 검증한다.
CI의 `verify` job은 `make verify` 다음 단계에서 이 명령을 실행한다. 전용 Prometheus에 합성 정상·실패·지연·첫 관측 전 데이터를
생성하고 실제 dashboard PromQL 결과를 검사한다. 테스트 container는 종료 시 제거한다.

메트릭스 비용은 `go test ./src/metrics -run TestCachedScrapeLatency -v -bench BenchmarkCachedScrape -benchmem`으로
측정한다. 2 Pool / 100 Volume fixture의 cached HTTP p99 기준은 100ms이며 실제 workload I/O 성능과 구분한다.

Unit test는 다음 고비용 경계를 포함한다.

| Domain | 주요 경계 |
|---|---|
| CSI | capacity, topology, parameter, 멱등 create/delete, publish authorization |
| Pool | directory/write/statfs readiness, freshness, reservation, concurrent admission |
| Mobility | phase closure, exact copy identity, CAS, API response loss, recovery |
| Cleanup | immutable intent, exact helper identity, receipt, bounded observation, review-only orphan |
| Lifecycle | Cleanup 포함 dependency 검사, quiesce, read-only admission, provisioning drain |
| Certificate | 최초 발급, 갱신, CA 전환, Secret 복구, hot reload |
| Metrics | cached scrape 무 I/O, API 오류·freshness, 예약 회계, bounded label, HTTP 장애 분리 |
| Chart | kubelet root, component 경계, StorageClass, webhook mode |

### Action boundaries

| 계약 | 회귀 테스트 |
|---|---|
| 모든 phase의 정상·대기·실패 결정 | `src/mobility/fsm/fsm_test.go` |
| 관찰·결정·action 오류에서 phase와 activeMove 보존 | `TestMoveErrorsPreservePhaseAndActiveMove` |
| 생성 의도 → 파일 effect → 완료 순서 | `TestCreateVolumeOrdersDurableIntentEffectAndCompletion` |
| Move UID별 source/incoming/destination copy 고정 | `TestMoveCopyIdentityIsPersistedBeforeJobsAndCommittedExactly` |
| 이전 Move incarnation의 Secret·ConfigMap·Pod·Service 거부 | `TestTransferResourcesRejectPreviousMoveIncarnation` |
| helper command와 Job identity 변조 거부 | `TestMoveJobsUseIdentityHelperAndRejectReplacement` |
| Destination publish·Cleanup 완료 전 잠금 유지 | `TestMoveCompletionWaitsForPublishAndCleanupEvidence` |
| exact source receipt만 Move 완료 허용 | `TestMoveCleanupSettlesOnlyReceiptForExactSource` |
| Cleanup spec 불변·UID fencing·terminal 상태 | `src/kubernetes/cleanupapi/store_test.go` |
| authority 재확인 뒤 inode 고정 retire/purge | `src/node/ownership/reclaim_linux_test.go` |
| Job·Pool 변경과 영구 실행 실패를 NeedsReview로 수렴 | `src/kubernetes/helperpod/cleanup_test.go` |
| Pending 재개·receipt settlement·review-only orphan 발견 | `src/lifecycle/cleanupcontroller/reconciler_test.go` |
| recovered Move의 superseded copy 발견과 cycle당 단일 authority snapshot | `src/lifecycle/cleanupcontroller/reconciler_test.go` |
| create/copy/promotion marker 경계 crash 복구와 active Move 외 unrecorded path 비채택 | `src/node/ownership/{store,transfer}_test.go`, `src/kubernetes/volumeapi/registry_test.go`, `src/mobility/controller/reconciler_test.go` |
| inventory exact-limit와 overflow admission | `src/node/observation/scanner_test.go`, `src/kubernetes/volumeapi/registry_test.go` |
| reservation·미완료 Cleanup의 Helm/Argo CD 제거 차단 | `TestCheckReportsEveryShiftPVDependency`, `TestCheckBlocksUnsettledCleanupContract` |
| 완료 증거 저장 전 잠금·transfer resource 보존 | `TestCompletionConfirmationPrecedesResourceDeletionAndUnlock` |
| CAS·journal·삭제 응답 유실과 반복 기록 실패 | `TestCompletionRecoversAcrossAPIFailureBoundaries` |
| 완료 중 다른 owner·Move 잠금·phase 보존 | `TestCompletionRejectsConflictingAuthority` |
| 관찰 직후 다른 Move가 잠금을 얻는 경합 | `TestCompletionCASPreservesConcurrentMove` |
| 잠금 해제 뒤 Volume 삭제·관찰 오류 구분 | `TestCompletionAfterVolumeDeletion`, `TestCompletionDoesNotTreatReadErrorsAsDeletion` |
| API 수락 후 응답 유실 | `src/mobility/controller/fault_boundary_test.go` |
| Spec의 phase·action 목록과 코드 enum 일치 | `TestMobilityContractNamesMatchFSM` |

```bash
go test -race -count=1 ./src/mobility/... ./src/csi/... ./src/kubernetes/cleanupapi ./src/kubernetes/helperpod ./src/lifecycle/cleanupcontroller ./src/node/ownership ./test/docs
```

`test/e2e/kind/mobility/completion.sh`는 격리 Kind에서 한 Volume의 성공 기록만 admission policy로
거절한다. 실제 이동·source purge 뒤 cleanup Job 삭제, CSI Volume 삭제, Controller 재시작을 겹치고
정책 해제 후 `Succeeded` 수렴과 effect 재생성 부재를 확인한다. Cleanup의 Move UID·source copyID·
executor UID·purge receipt·settledAt도 검증한다.

`test/e2e/kind/mobility/cleanup-lifecycle.sh`는 영구 cleanup 실패가 exact copy를 보존하고
`NeedsReview`에 수렴하는지 확인한다. effect Job 삭제와 Controller 재시작 뒤에도 자동 재실행하지 않으며,
deletion admission은 volume reservation과 `ShiftPVCleanup`을 uninstall blocker로 보고한다. 현재
owner checksum과 PVC/PV identity는 전 과정에서 유지한다. Argo CD E2E는 metadata 제거와 orphan
발견 사이에도 reservation이 Application 삭제를 막고, 승인된 exact cleanup 정산 뒤 같은 PreDelete가
자동 완료되는지 확인한다.

## kind E2E

각 실행은 격리된 cluster, kubeconfig와 host directory를 만든다. 서로 다른 `CLUSTER_NAME`으로
suite를 병렬 실행한다. 성공과 실패 모두 소유 resource를 정리하며 `KEEP_CLUSTER=1`은 진단을 위해
cluster를 보존한다.

```text
kind control-plane
├── worker A ── Pool A
└── worker B ── Pool B
```

| Scenario | 검증 |
|---|---|
| Directory Pool | 기존 non-mount directory에서 provision, write, Retain 보존·명시적 폐기, move·cleanup |
| Metrics | 격리 Prometheus target 3개, 실제 copy 관측, CSI 호출, cleanup 후 예약·active Move 0 |
| Capacity | 외부 사용량과 reservation으로 신규 claim 제어, 해제 용량 단일 반환 |
| StorageClass | default와 명시 선택의 결정적 공존 |
| Restart | Controller/Node 교체 뒤 mounted data와 republish checksum 보존 |
| Filesystem fault | ENOSPC/read-only에서 data 보존과 복구 뒤 수렴 |
| Lifecycle | mounted/retained dependency 제거 차단과 reinstall 복구 |
| Orphan cleanup | Retain PV와 실제 mount 중 보존, 명시 승인, unmount 뒤 exact data·marker·reservation 정리 |

집중 실행:

| 범위 | 명령 |
|---|---|
| Pool capacity | `POOL_CAPACITY_ONLY=1 CLUSTER_NAME=shiftpv-capacity-focused ./test/e2e/kind/run.sh` |
| 일반 directory | `DIRECTORY_POOL_ONLY=1 CLUSTER_NAME=shiftpv-directory-focused ./test/e2e/kind/run.sh` |
| Mobility filesystem fault | `MOBILITY_FILESYSTEM_FAULTS_ONLY=1 CLUSTER_NAME=shiftpv-mobility-fs-focused ./test/e2e/kind/run.sh` |
| Mobility node restart | `MOBILITY_NODE_RESTARTS_ONLY=1 CLUSTER_NAME=shiftpv-mobility-node-restart-focused ./test/e2e/kind/run.sh` |

### Mobility suite

```bash
./test/e2e/kind/mobility/run.sh
```

```mermaid
flowchart LR
    PREFLIGHT[selector / affinity / taint / PDB] --> MOVE[cordon Move]
    MOVE --> COPY[copy + checksum]
    COPY --> COMMIT[owner CAS]
    COMMIT --> INTENT[immutable Cleanup]
    INTENT --> RECEIPT[exact purge receipt]
    RECEIPT --> SETTLE[Completed]
    MOVE --> RECOVERY[Blocked → ResumeOwner]
```

| 증거 | 검증 |
|---|---|
| Identity | PVC UID, PV, volume handle 유지 |
| Data | checksum 유지 |
| Placement | persisted destination에서 replacement 실행 |
| Authority | destination이 Ready owner, `activeMove`는 빈 값 |
| Cleanup | source copyID 대상 intent/receipt/settlement, source final/retired 부재 |
| Restart | Copying, Promoting, Committing 중 Controller 교체 수렴 |
| TLS | Secret key, owner reference, CA bundle, disable/enable 정책 수렴 |

### Node restart suite

집중 node suite는 이동의 여섯 경계에서 실제 Kind worker container를 중지한다.

| 중단 지점 | Authority 결과 | 복구 |
|---|---|---|
| commit 전 source | source owner 유지 | source 복귀 뒤 `ResumeOwner` |
| commit 전 destination | source owner 유지 | destination 복귀 뒤 같은 Move 재개 |
| commit 후 destination | destination owner 유지 | 복귀 뒤 publish와 source cleanup 재개 |

### Argo CD suite

```bash
./test/e2e/kind/argocd/run.sh
```

고정 버전 Argo CD를 전용 cluster에서 실행한다. Dependency가 없는 Application 삭제, mounted volume
차단, resource 보존, blocker 해소 뒤 자동 완료를 검증한다.

## Linux mount integration

```bash
make linux-mount-integration
```

Linux 전용 runner는 `sudo`와 util-linux `unshare`를 사용한다. Private namespace에서 bind publish,
멱등성, unpublish와 unprivileged failure cleanup이 source를 보존하는지 검증한다.

## CI

[`ci.yaml`](../../.github/workflows/ci.yaml)은 pull request, `main`, manual dispatch에서 실행된다.

| Job | 범위 |
|---|---|
| `verify` | fast check와 coverage artifact |
| `linux-mount` | 실제 mount namespace |
| `kind-e2e` | CSI, Pool, capacity, filesystem, lifecycle |
| `kind-mobility-e2e` | 자동 이동과 Controller restart |
| `kind-mobility-node-restart-e2e` | source/destination node 중단 경계 |
| `kind-argocd-e2e` | Argo CD 삭제 수렴 |
| `image-controller` | Controller/uninstall-guard image 경계 |
| `image-node` | Node image 경계 |

## Images and releases

```bash
make image
make image-controller CONTROLLER_VERSION=0.2.0
make image-node NODE_VERSION=0.3.0
make image-combined
```

| Version source | Artifact | Release tag |
|---|---|---|
| `versions/controller` | `ghcr.io/cagojeiger/shiftpv-controller:<version>` | `controller/v<version>` |
| `versions/node` | `ghcr.io/cagojeiger/shiftpv-node:<version>` | `node/v<version>` |
| `charts/shiftpv/Chart.yaml` | Helm repository package | `chart/v<version>` |

기능 PR은 version source를 바꾸지 않는다. Controller, Node, Chart는 각각 독립된 `release:` PR로
순서대로 병합한다. Chart PR은 참조할 image가 공개된 뒤 `Chart.yaml`과 기본 image reference를 갱신한다.
병합된 version source 경로가 release workflow를 직접 실행하며, image는 `linux/amd64`, `linux/arm64`
manifest로 공개된다. Combined image는 Kind 검증용이다.

## Published artifact smoke

```bash
./test/e2e/kind/artifact/run.sh
```

Smoke test는 GitHub Pages chart와 [`versions.env`](../../test/e2e/kind/artifact/versions.env)의
digest-pinned 공개 image를 설치한다. PR CI는 lock 형식과 불변성을 검사하고 scheduled/manual workflow는
동적인 공개 가용성과 배포된 upgrade 경로를 검증한다. 현재 commit의 판정은 required CI job으로 확인한다.
