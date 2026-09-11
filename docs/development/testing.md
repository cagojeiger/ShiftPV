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

Dashboard의 현재값·관측 유효기간·결측 처리는 `bash test/helm/dashboard/run.sh`로 검증한다.
CI의 `verify` job은 `make verify` 다음 단계에서 이 명령을 실행한다. 전용 Prometheus에 합성 정상·실패·지연·첫 관측 전 데이터를
생성하고 실제 dashboard PromQL 결과를 검사한다. 테스트 container는 종료 시 제거한다.

메트릭스 비용은 `go test ./src/metrics -run TestCachedScrapeLatency -v -bench BenchmarkCachedScrape -benchmem`으로
측정한다. 2 Pool / 100 Volume fixture의 cached HTTP p99 기준은 100ms이며 실제 workload I/O 성능과 구분한다.

Unit test는 다음 고비용 경계를 포함한다.

| Domain | 주요 경계 |
|---|---|
| CSI | capacity, topology, parameter, 멱등 create/delete, publish authorization |
| Pool | directory/write/statfs readiness, freshness, reservation, concurrent admission |
| Mobility | 전체 phase closure, CAS, API response loss, diagnostics, recovery, source purge |
| Lifecycle | dependency 검사, quiesce, read-only admission, provisioning drain |
| Certificate | 최초 발급, 갱신, CA 전환, Secret 복구, hot reload |
| Metrics | cached scrape 무 I/O, API 오류·freshness, 예약 회계, bounded label, HTTP 장애 분리 |
| Chart | kubelet root, component 경계, StorageClass, webhook mode |

### Action boundaries

| 계약 | 회귀 테스트 |
|---|---|
| 모든 phase의 정상·대기·실패 결정 | `src/mobility/fsm/fsm_test.go` |
| 관찰·결정·action 오류에서 phase와 activeMove 보존 | `TestMoveErrorsPreservePhaseAndActiveMove` |
| Action 후 journal 거절·Controller 재생성 시 단일 Job 수렴 | `TestMoveActionSurvivesJournalFailureAndControllerRestart` |
| Destination publish·cleanup 완료 전 잠금 유지 | `TestMoveCompletionWaitsForPublishAndCleanupEvidence` |
| 요청 저장 실패 시 파일 작업 차단 | `TestCleanupIntentWriteFailureNeverStartsDiskWork` |
| 정리 acknowledgement 후 Job 소멸·Move 기록 실패 복구 | `TestCleanupAcknowledgementSurvivesJobLoss` |
| Pool·Job UID·owner·잠금·publisher 변경 차단 | `TestCleanupRejectsChangedPoolOrJobIncarnation` |
| 요청 생성·완료 응답 유실 수렴 | `src/lifecycle/cleanup/journal_test.go` |
| 부모 없는 미완료 요청의 uninstall 차단 | `TestCheckRetainsCleanupObligationWithoutParent` |
| 정리 확인의 UID·시도 번호·권한·순회·보관 경계 | `src/mobility/controller/cleanup_edges_test.go` |
| 읽기 전용 확인에서 권한·I/O 오류와 경로 부재 구분 | `src/lifecycle/cleanup/pathcheck/check_test.go` |
| 최종 잠금 해제 후 journal 실패·Job 소멸·재시작 복구 | `TestMoveCompletionJournalFailureRecoversAfterRestart` |
| 완료 증거 저장 전 잠금·transfer resource 보존 | `TestCompletionConfirmationPrecedesResourceDeletionAndUnlock` |
| CAS·journal·삭제 응답 유실과 반복 기록 실패 | `TestCompletionRecoversAcrossAPIFailureBoundaries` |
| 완료 중 다른 owner·Move 잠금·phase 보존 | `TestCompletionRejectsConflictingAuthority` |
| 관찰 직후 다른 Move가 잠금을 얻는 경합 | `TestCompletionCASPreservesConcurrentMove` |
| 잠금 해제 뒤 Volume 삭제·관찰 오류 구분 | `TestCompletionAfterVolumeDeletion`, `TestCompletionDoesNotTreatReadErrorsAsDeletion` |
| API 수락 후 응답 유실 | `src/mobility/controller/fault_boundary_test.go` |
| Spec의 phase·action 목록과 코드 enum 일치 | `TestMobilityContractNamesMatchFSM` |

```bash
go test -race -count=1 ./src/mobility/... ./src/csi/... ./test/docs
RECOVERY_SCRIPT_IMAGE=shiftpv:dev go test -count=1 ./src/mobility/controller -run 'Test(CleanupSourceScript|RecoveryRetirementScript|RecoveryOwnerVerificationScript)'
```

두 번째 명령은 준비된 helper image에서 현재 script를 실행한다. Linux에서는 image 지정 없이 실행하고,
macOS에서 image를 지정하지 않으면 해당 script test는 skip된다. 이는 실제 Kind 이동 검증과 별도다.

`test/e2e/kind/mobility/completion.sh`는 격리 Kind에서 한 Volume의 성공 기록만 admission policy로
거절한다. 실제 이동·원본 삭제 뒤 cleanup Job 삭제, CSI Volume 삭제, Controller 재시작을 겹치고
정책 해제 후 `Succeeded` 수렴과 helper 재생성 부재를 확인한다. 정리 요청의 불변성·Move/Job UID·
경로·완료 기록도 검증한다. Post-commit recovery는 남은 요청이 실제 deletion admission에서
`CleanupRequest` blocker로 나타나는지 server-side dry-run으로 확인한다.

`test/e2e/kind/mobility/cleanup-lifecycle.sh`는 recovery 뒤 격리 데이터가 있으면 확인을 실패시킨다.
실패한 시도의 Job 삭제·Controller 재시작·같은 번호 재요청은 재실행하지 않는다. 테스트 소유 데이터를
Pool 밖으로 옮긴 뒤 증가한 번호로 다시 확인하여 완료 기록·TTL·uninstall 의무 해제를 검증한다.
최신 destination checksum과 PVC/PV identity는 전 과정에서 유지한다.

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
    COMMIT --> PURGE[source final + retired absent]
    MOVE --> RECOVERY[Blocked → ResumeOwner]
```

| 증거 | 검증 |
|---|---|
| Identity | PVC UID, PV, volume handle 유지 |
| Data | checksum 유지 |
| Placement | persisted destination에서 replacement 실행 |
| Authority | destination이 Ready owner, `activeMove`는 빈 값 |
| Cleanup | destination final 존재, source final/retired 부재 |
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
make image-node NODE_VERSION=0.1.3
make image-combined
```

| Version source | Artifact | Release tag |
|---|---|---|
| `versions/controller` | `ghcr.io/cagojeiger/shiftpv-controller:<version>` | `controller/v<version>` |
| `versions/node` | `ghcr.io/cagojeiger/shiftpv-node:<version>` | `node/v<version>` |
| `charts/shiftpv/Chart.yaml` | Helm repository package | `chart/v<version>` |

기능 PR은 version source를 바꾸지 않는다. 별도 `release:` PR에서 공개할 component의 version source와
Chart의 기본 image reference를 갱신한다. 병합된 release PR의 `main` CI가 성공하면 변경된 component만
`linux/amd64`, `linux/arm64` manifest로 공개된다. Chart는 `Chart.yaml`의 `version`이 바뀐 경우에만
참조 image의 공개를 확인한 뒤 배포된다. 실패한 release는 해당 workflow를 재실행하며, 이후의 무관한
commit에서는 미공개 version을 대신 배포하지 않는다. Combined image는 Kind 검증용이다.

## Published artifact smoke

```bash
./test/e2e/kind/artifact/run.sh
```

Smoke test는 GitHub Pages chart와 [`versions.env`](../../test/e2e/kind/artifact/versions.env)의
digest-pinned 공개 image를 설치한다. PR CI는 lock 형식과 불변성을 검사하고 scheduled/manual workflow는
동적인 공개 가용성과 배포된 upgrade 경로를 검증한다. 현재 commit의 판정은 required CI job으로 확인한다.
