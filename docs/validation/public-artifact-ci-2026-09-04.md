# Public artifact CI validation — 2026-09-04

## Scope and verdict

SP-STAB-003 on `codex/blocked-owner-recovery`, uncommitted tree based on `c294c960`.
Local implementation and verification are complete. Code/spec/security review returned
APPROVE with zero issues and architecture returned CLEAR after the public smoke was moved
out of the PR merge gate. This stage adds no product behavior and did not publish or deploy
an artifact.

## Goal and success criteria

- Keep source-built regression and published-artifact integration as independent CI duties.
- Reject incomplete, mutable or injection-like artifact locks before creating a cluster.
- Fetch the public chart through its Helm repository, verify the package SHA-256, install
  controller/node by manifest digest and mount a real PVC in Kind.
- Check command exits plus resource identity, authority and data; clean only owned resources.
  Bound: five QA cycles, no Home/release/remote-repository mutation.

## Artifact lock

| Artifact | Immutable identifier |
|---|---|
| Chart | `shiftpv` 0.1.0, SHA-256 `257a8ad8998aecfae7907ff363c856db6df379e6266285a3e99b69d02c3aea50` |
| Controller | `ghcr.io/cagojeiger/shiftpv-controller:0.1.1@sha256:fd1f1b58126d8fa01f7aa548706e70eacf15480af15676cd2278f16e206295a3` |
| Node | `ghcr.io/cagojeiger/shiftpv-node:0.1.1@sha256:7b2f5d3066375d88a7062cf09efb5006b47f6c7688f19366867db33fd7b121f9` |
| Kind | Kubernetes 1.35.8, `sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0` |

GHCR values are multi-platform index digests. The test checks the exact rendered references;
the node runtime resolves the matching platform manifest.

## Scenario matrix

| ID | User/fault model | Harness | Expected | Actual | Status | Evidence | Cleanup |
|---|---|---|---|---|---|---|---|
| A1 | Missing/duplicate/malformed/mutable lock | lock release test | reject before source/network/Kind | invalid cases rejected; valid lock 10/10 | PASS | make/focused logs | mktemp removed |
| A2 | Command substitution in known/unknown key | marker harness | reject without execution | rejected; marker absent | PASS | release test | mktemp removed |
| A3 | Public chart changed | runner with wrong valid-shaped hash | mismatch before cluster | actual hash reported; no cluster | PASS | `artifact-bad-hash.log` | bad lock/workdir removed |
| A4 | Mutable/wrong runtime image | real Kind + Pod-spec audit | exact version@digest refs | controller/node exact-match | PASS | cycle1 workload JSON | K |
| A5 | Published integration | remote chart/images, two mounted pools | Ready CSI, Bound PVC, checksum | two runs exit 0 | PASS | cycle1/2 logs | K |
| A6 | Misleading success output | post-run API/filesystem audit | authority and host/Pod checksum agree | one Ready volume; checksum `8c328652070131b5d4cdd29e7cc17f8eef6e9fbfa576425c9f71450cc2537f95` | PASS | cycle1 snapshots | K |
| A7 | Dirty prior-stage tree | status before/after | preserve existing edits | prior files and `.omc`/`.omx` retained | PASS | git status | intentional |
| A8 | Fetch/pull hangs or fails | workflow timeout/diagnostics | bounded failure without blocking PRs | 20-minute job, export/upload, manual+daily workflow | PASS (static) | artifact workflow | always delete K |

K means the owned Kind cluster was removed. First-run data/package/kubeconfig are recoverable
under `/Users/kangheeyong/.Trash/shiftpv-artifact-20260904.Nzr6Nu/`; repeat-run normal cleanup
left no cluster or host fixture. Prompt injection is not a CSI input here. The relevant hostile
surface is lock text later sourced by shell, so exactly six known keys are accepted.

## Existing edge-to-test mapping

SP-STAB-003 does not duplicate permanent checks already reached by `make verify` or CI:

- invalid capacity/topology/access mode/StorageClass parameters and missing Pool: controller tests;
- duplicate Pool and owner conflicts: registry tests;
- duplicate/stale image/chart release attempts: release resolver tests;
- deleted TLS Secret, CA transition and hot reload: certificate manager tests;
- real ENOSPC/read-only convergence: base Kind `filesystem-faults.sh`;
- recovery, preflight, restart and retained binding: mobility Kind runner.

The lock test joins `make verify`; public `kind-artifact-e2e` runs on demand and daily,
separate from both the PR merge gate and source-built Kind.
The mobility job timeout is 45 minutes because its permanent matrix outgrew the former margin.

## Commands and evidence

- `[0] make verify`: race coverage **83.4%**, vet/build, release tests, ShellCheck, actionlint,
  Helm and docs; `.tmp/artifact-make-verify.log`.
- `[0]` first and repeated `test/e2e/kind/artifact/run.sh`: public package/images and mount.
- `[0]` post-run kubectl/jq/filesystem audit: one Ready volume, no activeMove, correct owner,
  Bound PVC, exact images and matching checksum. Evidence: `.tmp/artifact-cycle1-*`.
- `[expected non-zero]` wrong-hash run: mismatch before Kind; lock invalid cases also rejected.
- `[0]` lock validation 10 times, ShellCheck, actionlint and `git diff --check`.
- Independent code/spec/security review: APPROVE, 13 scoped files, zero findings; it also
  reran verify, gopls, ShellCheck/actionlint and the public Kind smoke.
- Architecture review: initial WATCH for external public availability in the PR gate; final
  CLEAR after moving the dynamic smoke to a separate daily/manual workflow.

## Failures found and fixed

1. The draft concatenated two `sed` commands. ShellCheck caught competing redirections; fixed
   before product execution and rerun.
2. The first validator allowed an extra uppercase key before sourcing. It now accepts exactly
   six keys, with known/unknown command-substitution tests proving no marker execution.

## Cleanup and remaining boundary

Both artifact clusters were removed; `kind get clusters` returned none. No Docker prune/engine
reset, Home change, commit, push, PR, release or publication occurred. Logs/snapshots are retained
as evidence; disposable first-run data is recoverable in Trash.

This job verifies the current published baseline, not unpublished checkout behavior.
Publication workflows verify availability after publishing; update the lock only after new
immutable artifacts exist. Remote GitHub CI has not run, and this smoke does not replace source
fault/mobility/Argo CD suites.
