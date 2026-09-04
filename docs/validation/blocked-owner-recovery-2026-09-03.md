# Explicit owner recovery validation — 2026-09-03

## Scope and outcome

SP-STAB-001의 개발 구현과 격리 검증을 완료했다. 전체 안정화나 공개 배포 완료가 아니다.
`codex/blocked-owner-recovery`, 기준 commit `c294c960` 위의 작업 트리를 검증했다.
public chart/image는 변경하지 않았다. local chart와 개발용 combined image를 사용했다.

- Kind: Kubernetes `v1.35.8`, control-plane 1 + worker 2, 서로 다른 임시 Pool 경로.
- 최종 cluster: `shiftpv-recovery-0903c`, 별도 KUBECONFIG 사용.
- image: `shiftpv:dev`, ID `sha256:59e5e9d7a4af936df27f3593916f262cb1baaacbe6b46d272e402db9a89410c2`.
- `make verify`: exit 0. fresh `go test -count=1 -race -covermode=atomic ...`: exit 0,
  전체 statement coverage **82.4%**.
- 독립 code/spec/security review APPROVE, architecture review CLEAR. 검토에서 발견한
  final CAS 이후 복구 journal 확정 전 재발견 경계를 수정하고 회귀 테스트했다.

## Scenario matrix

| ID | 모델 / 조건 | 실행 / 기대 결과 | 실제 결과 | 증거 |
|---|---|---|---|---|
| R1 | 운영자, source-only selector로 Blocked | uncordon + ResumeOwner; 같은 PVC/owner/payload 재개 | 통과, Recovered + Ready + activeMove 없음 | mobility/recovery.sh, Kind cycle 3 |
| R2 | 운영자, commit 후 cleanup mkdir 실패 | destination에 새 데이터 기록; stale source로 rollback 금지 | 통과, 최신 데이터 유지와 source aborted 격리 | Kind cycle 3, 서로 다른 checksum |
| R3 | 잘못된/중복 요청 | ForceSource, 요청 제거, Blocked 전 요청은 API 거부; 중복 ResumeOwner 멱등 | 통과 | 실제 CRD 검증, recovery.sh |
| R4 | controller interruption | 복구 Verifying 재시작, 정상 Copying/Promoting/Committing 재시작 | 통과 | Kind cycle 3; fresh reconciler 단위 테스트 |
| R5 | stale authority/binding, foreign publication, API timeout | Ready 공개 금지, 이유 기록 | 단위 테스트 통과 | recovery_test.go, registry_test.go |
| R6 | 미종료 helper, failed check, PDB 거부 | 종료 read-back 전 대기; UID 조건과 PDB 유지 | 단위 테스트 통과 | recovery_test.go |
| R7 | filesystem 잘못된 marker/symlink/경로 충돌 | 실제 셸이 거부하고 기존 격리 데이터 보존 | Linux container 실행 통과 | recovery_scripts_test.go |
| R8 | partial copy / 이미 retire / 반복 실행 | optional staging 부재 허용; 불명확한 committed source 부재 거부 | Linux container 실행 통과 | recovery_scripts_test.go |
| R9 | dirty workspace / 중단 / bounded waits | 기존 .omc/.omx 보존; 소유한 실행만 종료·재개 | 통과 | cycle 2 SIGTERM 후 새 cluster 재검증 |
| R10 | misleading success / 재실행 | 메시지가 아니라 종료 코드와 identity/checksum 판정 | 최종 전체 runner exit 0 | cycle 1 exit 1을 성공으로 처리하지 않음 |

Prompt injection은 LLM 기능이 아닌 이 driver의 직접 입력 경로가 아니다. 잘못된 CR 입력
거부(R3)를 해당 공격 표면의 대체 검증으로 사용했다. production fault injection은 하지 않았다.

## Data and authority evidence

- PVC UID: `fda09ca1-66d2-4df0-a48a-ea43699c5865`.
- PV: `pvc-fda09ca1-66d2-4df0-a48a-ea43699c5865`.
- volume: `shiftpv-e3327b3874df4402ee7c1f998d441652`.
- 첫 정상 이동: worker → worker2, Move `move-e3327b3874df4402ee7c1f998d441652-chhng` Succeeded.
- 반환 이동: worker2 → worker, Move `move-e3327b3874df4402ee7c1f998d441652-bggg7`
  `Blocked/CleanupFailed` → `recoveryPhase=Recovered`.
- 실패 로그: `mkdir: cannot create directory '/pool/.shiftpv/retired': File exists`.
- commit 이후 최신 owner checksum:
  `b8618f15ed4937ff8fae543f6f547a57d360622676c5f65015901c5438b2a720`.
- 격리된 stale source checksum:
  `5e5f6fbe9c6e6e15fec4d9f6bbad305a5d8a5ca18856b05949deca446d7398b1`.

두 checksum이 달라진 뒤에도 최신 owner의 checksum, PVC UID와 PV가 유지됐다. recovery는
owner를 뒤집지 않았으며 non-owner final은 aborted 경로로 이동했다. 원래 Move의 Blocked
이력은 남고 Volume은 Ready, activeMove는 비었다. 마지막 mobility disable Helm upgrade도
통과했으며 TLS Secret 유지와 inert admission을 확인했다.

## Commands and iteration history

- `[0] make verify` — format/module/race/coverage/vet/build, release resolver,
  ShellCheck, Helm lint/template, doc links. resolver의 기대 음수 fixture 출력은 최종 exit와 구분.
- `[0] go test -count=1 -race -covermode=atomic ...` — 최종 fresh 전체 회귀.
- `[0] RECOVERY_SCRIPT_IMAGE=shiftpv:dev go test ./src/mobility/controller
  -run 'TestRecovery(Retirement|OwnerVerification)Script' -count=1 -v` — 실제 Linux 셸.
- `[0] KEEP_CLUSTER=1 CLUSTER_NAME=shiftpv-recovery-0903c E2E_KUBECONFIG=<isolated>
  bash test/e2e/kind/mobility/run.sh` — 최종 전체 E2E.
- cycle 1: source 복구와 정상 이동 통과 후, 테스트가 source node만으로 과거 Move를 고르는
  harness 오류로 exit 1. 정확한 Volume.activeMove를 사용하도록 수정했다.
- cycle 2: source 복구 재통과. 별도 단위 테스트가 cleanup 실패를 ControlledConsumerMissing으로
  가리던 기존 진단 오류를 재현했다. 수정 전 이미지 검증을 이어가지 않고 shell SIGTERM
  (exit 143), cluster 제거, rebuild 후 cycle 3을 실행했다.
- 진단 수정: eligibility reason은 Pending/preflight에만 적용한다. 이동 중 Job 실패의
  CopyFailed/PromotionFailed/CleanupFailed를 사라진 옛 consumer가 가리지 않게 했다.

## Evidence and cleanup

로컬 상세 증거는 `.tmp/recovery-kind-cycle{1,2,3}.log`, `recovery-final-crs.yaml`,
`recovery-final-resources.txt`, `recovery-verify-reviewed.log`, `recovery-final-race.log`,
`recovery-final-coverage.out`, `recovery-script-tests.log`, `recovery-qa-report.md`에 보존한다.
임시 Kind cluster 세 개는 제거했다. 테스트 Pool 데이터와 임시 kubeconfig는 휴지통의
`shiftpv-recovery-cycle1-Bwe3NT`, `shiftpv-recovery-cycle2-nux1yg`,
`shiftpv-recovery-cycle3-ACn384`로 이동해 복구 가능하게 정리했다. 기존 `.omc/`, `.omx/`
및 다른 프로젝트 자원은 보존했다. `kind get clusters`에서 잔여 cluster가 없음을 확인했다.

## Limits and next step

이번 실행은 Home/Argo CD 실환경 배포, 장시간 반복 부하, node/container 장애 및 모든 API·CSI
in-flight 경합을 검증한 것은 아니다. bare/multi-consumer/custom scheduler 등 기존 지원 제한은
확대하지 않았다. waiting phase의 deadline/진단 개선, 같은 이름의 Pod 재생성 같은 추가
워크로드 경계는 후속 안정화 대상이다. 공개 image/chart를 올리거나 PR/merge하지 않았다.
다음 순서는 **SP-STAB-002: 예측 가능한 이동 불가 조건에서 불필요한 eviction 줄이기**다.
