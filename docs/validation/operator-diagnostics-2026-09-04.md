# Operator-visible mobility diagnostics — 2026-09-04

## Scope and verdict

SP-STAB-004 was implemented and validated locally on `codex/blocked-owner-recovery`, based on
`c294c960`. The change adds operator diagnostics to the existing `ShiftPVMove` journal; it does
not add a CRD kind, controller, automatic rollback, owner selection, timeout policy or forced
Kubernetes operation. The tree remains uncommitted and unpublished.

## Contract exercised

- Every created Move exposes a human message, `lastTransitionTime`, and `lastProgressTime`.
- `kubectl get shiftpvmove` shows Phase, Reason, Source, Recovery, Destination and LastProgress.
- Pending/Waiting/progress messages identify automatic retry; Blocked identifies required
  operator action; Recovered and Succeeded say no further action is required.
- Observation and action API errors retain the current phase and authority, record a transient
  reason/message, and clear that diagnostic after a successful reconciliation.
- Phase, reason and recovery changes emit Kubernetes Events after status persistence. Identical
  polling does not advance timestamps or emit another event.
- Time fields are diagnostic only and do not trigger failure or rollback.

## Static and unit verification

- `make verify`: exit 0, race coverage 83.3%, format/module/vet/build, release checks,
  ShellCheck, Helm lint, actionlint and documentation checks passed.
- `go test ./...`: exit 0.
- `TestMoveDiagnostics*` with `-race -count=10`: exit 0.
- Registry round-trip covers both timestamp fields.
- Unit cases cover initial status, reason-only changes, phase progress, polling without time/Event
  drift, observation API timeout, action API timeout, and clearing a recovered transient error.

## Kind execution

Two complete isolated mobility runs were used. The first run found one diagnostics defect:
`recoveryPhase=Recovered` still carried the old Blocked instruction to request ResumeOwner.
The implementation and E2E assertion were corrected, then a fresh cluster reran the whole suite.

Final run:

- cluster: `shiftpv-diagnostics-repeat`, Kubernetes 1.35.8;
- result: `ShiftPV closed-loop mobility E2E passed`, exit 0;
- non-disruptive selector, affinity, taint and PDB scenarios passed;
- PDB removal converged to `Succeeded` with the same data;
- real copy failure converged to `Blocked/CopyFailed`, then `Recovered` across a Controller restart;
- Copying, Promoting and Committing each survived a forced Controller Pod replacement;
- real post-commit cleanup failure converged to `Blocked/CleanupFailed`, then `Recovered`,
  preserving new destination writes and quarantining the stale copy;
- every one of four Moves had non-empty RFC3339 transition/progress timestamps and a current
  message; both recovered Moves retained the original reason as history while reporting that no
  further operator action was required;
- Events included CopyFailed, CleanupFailed, MobilitySucceeded and exactly two
  RecoveryRecovered observations for the two recovery transactions;
- all six ShiftPVVolumes were Ready with empty activeMove; the only published volume was
  published on its owner;
- final Pod and authoritative host payload SHA-256 were both
  `b8618f15ed4937ff8fae543f6f547a57d360622676c5f65015901c5438b2a720`;
- Controller logs contained no Event sink rejection or diagnostic persistence error.

## Cleanup and boundary

Both owned Kind clusters were deleted and `kind get clusters` returned none. Their fixture and
kubeconfig directories were moved to recoverable Trash locations:

- `/Users/kangheeyong/.Trash/shiftpv-diagnostics-first-20260904/`
- `/Users/kangheeyong/.Trash/shiftpv-diagnostics-repeat-20260904/`

No Home cluster, release, image, chart publication, PR, merge, tag or remote workflow was changed.
SP-STAB-005 fault-boundary work and SP-STAB-006 soak/release-readiness work remain separate.
