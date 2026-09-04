# Non-disruptive mobility preflight — 2026-09-04

## Scope and current verdict

SP-STAB-002, `codex/blocked-owner-recovery`, uncommitted development tree on
`c294c960`. Includes the preceding SP-STAB-001 recovery work; not a public release.
Current verdict: **local implementation and verification complete**, 2026-09-04 KST.
Final Kind cycle and independent final-state audit both exited 0. Cycle 1 was not
accepted despite its success text; its Retain/PVC binding defect was fixed and retested.

## Goal and success criteria

- Preserve an existing consumer when observable hard constraints make migration infeasible.
- Finish only after baseline, adversarial Kind scenarios, authority/data audit and both
  independent reviews pass, with owned fixtures cleaned up. Stop bound: five QA cycles.
- Preserve pre-existing SP-STAB-001 changes and unrelated `.omc/` / `.omx/` files.
  Use only an isolated Kind cluster; no PR/release/Home mutations or unbounded soak.
- UltraQA complete after **2 cycles**. This is not overall stabilization completion.

## Contract

- Before discovery, lock and first eviction/retry, inspect actual ReplicaSet/StatefulSet
  owner UID and template, live constraints, registered destination health, required
  node/PV affinity, hard taints and conservative PDB allowance.
- Do not confuse an admission-injected owner pin with a template hostname selector.
- Reject reused PVC names unless claimRef UID and PVC volumeName identify this PV.
- Pending transient ineligibility waits and rechecks. A late extra consumer does not
  cause eviction or overwrite the deferral reason. Source/authority failures still block.
- Pod UID eviction preconditions and consumerUID distinguish same-name replacements.
- No scheduler reservation, resource-fit guarantee, workload spec modification, hard
  quota or dead-source failover. Explicit unsupported hard placement constraints defer.

See [ADR 0009](../adr/0009-nondisruptive-mobility-preflight.md) and
[mobility contract](../spec/volume-mobility.md#non-disruptive-preflight).

## Scenario matrix

| ID | User/fault model and setup | Expected signal | Command/harness | Actual result | Status | Evidence | Cleanup |
|---|---|---|---|---|---|---|---|
| P1 | User source-only hostname selector | Same Pod UID, writable data, Ready volume, no Move after restart | Kind preflight.sh | Unchanged consumer before/after restart | PASS | cycle2 log lines 139–143 | K |
| P2 | Required node affinity / untolerated destination taint | No lock/eviction | unit + Kind preflight.sh | Same consumers, Ready, writable | PASS | cycle2 log lines 157, 171 | K |
| P3 | PDB denies, then user removes it | Preserve consumer; later migrate automatically | unit + Kind | No Move while denied; Succeeded with same data after removal | PASS | cycle2 log lines 186–190 | K |
| P4 | Missing/recreated owner, API error, multiple PVCs, unsupported hard constraints | No unsafe action; reason/error | preflight_test.go | Conservative rejection/error | PASS | final-race + boundaries-repeat logs | no external fixture |
| P5 | Late second/missing consumer or namespace opt-out | Wait and resume; no terminal eligibility failure | preflight_test.go + FSM | Red before fix; wait/resume and post-lock reason now pass | PASS | boundaries-red, late-consumer-red, boundaries-repeat logs | no external fixture |
| P6 | Retain PV plus reused namespace/PVC name | Old volumes never join new claim's migration | binding tests + Kind retained assertions | Exact expected Move set; all six volumes Ready | PASS | cycle2 JSON snapshots + final-audit log | K |
| P7 | Admission owner pin and controller interruption in three phases | Same PVC/PV/checksum; Succeeded | full mobility runner | Copying/Promoting/Committing restarts and normal move pass | PASS | cycle2 log lines 229, 234, 239 and final result | K |
| P8 | Real copy/cleanup filesystem faults; repeated recovery request/restart | Resume authoritative source/destination | full runner + Linux script tests | Source resumes; newest committed-destination writes preserved | PASS | cycle2 log lines 213, 253 + recovery-scripts log | K |
| P9 | Malformed recovery CR, stale state/UID, malicious path/marker | Reject invalid input; preserve data | recovery/preflight tests + Kind CR validation | Invalid/reversed recovery rejected; script guards pass | PASS | final-race, recovery-scripts logs + retained fixture outputs | K / unit |
| P10 | Dirty tree, interrupted session, false success, bounded waits, flake | Preserve work; validate exits and full state | QA cycle evidence + final-audit.sh | Cycle1 false green rejected; final exit/state pass; flaky test repaired | PASS | both cycle logs + uninstall-fixed log | K; intentional evidence retained |

Evidence paths above use `.tmp/preflight-` prefixes. K means both owned clusters removed
and fixture data/kubeconfigs moved to recoverable Trash; details below. Permanent tests
are intentional uncommitted changes, not temporary debris.

No prompt-injection surface exists in this CSI controller; malformed Kubernetes input
is the relevant substitute. Home/production faults and unbounded soak are out of scope.

## Failures found and fixes

1. Cycle 1 reused a deleted namespace/PVC name with Retain PVs still present. The controller
   used claimRef namespace/name without validating UID or PVC.spec.volumeName, so old
   volumes acquired unintended Moves. Fixed the binding guard; kept name reuse in the
   test and now assert previous volumes' Ready state, empty activeMove and exact Move set.
2. A late second consumer produced terminal Blocked before preflight could flag a deferral.
   Pending preconditions now wait; source health and authority checks remain dominant.
3. After lock, replacement analysis overwrote MultipleConsumers with InvalidDestination.
   Destination analysis now starts only after the pre-eviction window.
4. Existing uninstall test attempt timeout equaled the ACK poll interval (200ms), and
   sleep-based blocker removal could miss the blocked observation. Test-only deterministic
   snapshots now require blocked then clear checks, with 1s attempts and a 5s total bound.
   Race-mode package repetition passed 10 times. Production uninstall logic is unchanged.

## Commands and evidence

- `make verify`: exit 0, statement coverage **83.4%**.
- Fresh standard-package race/atomic coverage: exit 0; `.tmp/preflight-final-race.log`
  and `.tmp/preflight-final-coverage.out`.
- `go test -count=10 -race -p 1 ./src/cmd/uninstall-guard`: exit 0.
- `go test -race -count=10 ./src/mobility/controller -run 'Test(Preflight|PendingEligibility|ConsumerUID|EvictionUses)'`:
  exit 0; `.tmp/preflight-boundaries-repeat.log`.
- `RECOVERY_SCRIPT_IMAGE=shiftpv:dev go test ./src/mobility/controller -run 'TestRecovery(Retirement|OwnerVerification)Script' -count=1 -v`:
  exit 0; `.tmp/preflight-recovery-scripts.log` (actual Linux filesystem operations).
- Independent code/spec/security review: APPROVE. Architecture review: CLEAR after
  verifying the late-consumer diagnostic repair.
- A separate initial `go test -coverprofile ... ./...` encountered local missing `covdata`
  for no-test main packages; use the repository's coverage package selection and run
  command tests separately. This command was not counted as passing.
- Red evidence: `.tmp/preflight-boundaries-red.log`, `.tmp/preflight-late-consumer-red.log`.
- Cycle 1: `.tmp/preflight-kind-cycle1.log`, `.tmp/preflight-cycle1-resources.yaml`.
  Its success message is insufficient because unrelated retained volumes were moved.
- Cycle 2: `.tmp/preflight-kind-cycle2.log`, cluster `shiftpv-preflight-0904`, Kind
  Kubernetes `v1.35.8`, image `shiftpv:dev` ID
  `sha256:a7c2ad09dfd8fdc2607417594d5098df60ae71abf8a0b0a4d628728237b5dda4`.
- `[0] KEEP_CLUSTER=1 CLUSTER_NAME=shiftpv-preflight-0904 E2E_KUBECONFIG=/Users/kangheeyong/project/ShiftPV/.tmp/preflight-0904-kubeconfig bash test/e2e/kind/mobility/run.sh`:
  about 24 minutes including build/setup; bounded per-step waits (180–600s).
- `[0] bash .tmp/preflight-final-audit.sh`: independent count/authority/binding assertions
  after the runner exited; `.tmp/preflight-final-audit.log`. JSON/YAML snapshots:
  `.tmp/preflight-cycle2-{moves,volumes,workload}.json`,
  `.tmp/preflight-cycle2-resources.yaml`, `.tmp/preflight-cycle2-events.txt`.

## Final identity and data evidence

- Exactly **4 Moves**: 2 Succeeded and 2 Blocked with `recoveryPhase=Recovered`.
  Recovered Moves intentionally retain the original failure history.
- Exactly **6 volumes**, all Ready, empty activeMove, no published node differing from owner.
  Selector/affinity/taint Retain volumes have zero Moves; the PDB fixture has exactly one.
- Normal volume: `shiftpv-079bfe56dd2c7f3e5ad6280ed5792d94`.
  PVC UID `0ca57597-cbb9-4d28-90ae-bf97307c88dd`,
  PV `pvc-0ca57597-cbb9-4d28-90ae-bf97307c88dd` remained unchanged.
- Normal Move `move-079bfe56dd2c7f3e5ad6280ed5792d94-cgncl` succeeded worker2 → worker
  through three controller restarts. SHA-256 before/after:
  `5e5f6fbe9c6e6e15fec4d9f6bbad305a5d8a5ca18856b05949deca446d7398b1`.
- Return Move `move-079bfe56dd2c7f3e5ad6280ed5792d94-mmnrf` failed cleanup after owner
  commit to worker2. Recovery preserved the appended `after owner commit` payload:
  `b8618f15ed4937ff8fae543f6f547a57d360622676c5f65015901c5438b2a720`.
  Pod and owner-directory checksums agree. The stale worker copy keeps the old checksum
  in `.shiftpv/aborted/<move>-final`; it is absent from the active volume directory.
- PDB Move `move-94710b49e5f6c6acea066b95b0b838cc-2c8d6` succeeded after PDB removal.
  Copy-failure Move `move-76f1371cc0a73bacfa2b24e38e7a02cf-c5knb` recovered its source.
- Helm mobility disable retained TLS material and made mutating admission inert.

## Cleanup and rollback

Both clusters `shiftpv-preflight-0903` and `shiftpv-preflight-0904` were removed after
evidence capture. `kind get clusters` returned none; no final-cluster containers remain.
Both fixtures (`shiftpv-mobility.zrkRB2`, `shiftpv-mobility.j6kqxe`) and isolated kubeconfigs
are recoverable under `/Users/kangheeyong/.Trash/shiftpv-preflight-20260904.mYux57/`.
Docker was restarted after it stopped between turns; the engine and unrelated resources
were not reset/pruned. The runner and cleanup process exited normally.

Logs, coverage, final audit script and snapshots are intentionally retained under `.tmp/`
as local evidence. The audit script is a historical one-shot tied to the removed cluster;
use the permanent runner for fresh reproduction. Existing SP-STAB-001 edits and unrelated
`.omc/` / `.omx/` files were preserved; no commits or broad rollback were performed.
UltraQA mode state is cleared through its CLI on completion, not by deleting user files.

## Residual risks

No PR/merge/image or chart publication, Home changes, or background automation.
Remaining stabilization: permanent artifact/edge CI coverage (SP-STAB-003), residual
diagnostics, broader fault boundaries and agreed repetition/soak criteria.
StatefulSet template/UID handling has unit coverage, not a separate full Kind workload
scenario. Resource fit, admission races, node loss, full/read-only destination filesystem
and certificate near-expiry/CA rollover were not all exercised in this stage. A successful
preflight is not a scheduling reservation. Actual GitHub CI was not run.
