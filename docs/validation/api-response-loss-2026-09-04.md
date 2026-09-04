# API response-loss mobility boundaries — 2026-09-04

## Scope

This is the first bounded SP-STAB-005 fault-validation slice. It exercises requests that the
Kubernetes API accepted while the client received a timeout instead of the success response.
The test does not claim to cover ENOSPC, read-only filesystems, node-container restart, stale Job
completion, PVC/Pod mutation or certificate rollover.

## Safety contract

- A lost ShiftPVMove create response must not create a second Move for the volume.
- Destination and copy Job identity must be durable before transfer resources can start.
- Secret, ConfigMap, source Pod, Service, copy Job, promotion Job and cleanup Job creation must
  reuse the accepted object on retry rather than create another operation.
- A lost volume-lock CAS response must preserve source authority and converge to the same Move.
- A lost owner-commit CAS response must never roll back or select another owner; the accepted
  destination remains authoritative with the same active Move.
- A lost transfer-resource delete response and a lost final activeMove-clear response must
  converge through NotFound/read-back without leaving a second writer.

## Verification

The deterministic fault repository applies a mutation and then returns a timeout once. The fake
Kubernetes reactor likewise stores or deletes the named object before returning a timeout. Every
test then retries through the production controller method and checks the persisted authority,
object count and journal ordering.

Initial focused result:

- `go test -race ./src/mobility/controller -run 'Test(MoveDiscoveryConverges|CopyResources|MobilityJobsConverge|VolumeLockConverges|OwnerCommitConverges|CompletionConverges|TransferCleanupConverges)' -count=5`: exit 0.

Repository result:

- `make verify`: exit 0; race coverage 84.4%, format/module/vet/build, release resolver,
  artifact lock, ShellCheck, Helm lint, actionlint and documentation checks passed.

Fresh Kind result:

- cluster: `shiftpv-api-loss-e2e`, Kubernetes 1.35.8;
- result: `ShiftPV closed-loop mobility E2E passed`, exit 0;
- selector, affinity, taint and PDB preflight cases remained non-disruptive;
- PDB removal resumed and completed the deferred Move;
- a real copy filesystem failure reached `Blocked/CopyFailed`, then recovered on the same source
  owner across Controller restart and duplicate recovery request;
- Controller replacement during Copying, Promoting and Committing converged to the same Move;
- a real post-commit cleanup failure recovered on the committed destination, retained its latest
  write and quarantined the stale source;
- final Pod and authoritative host payload SHA-256 were both
  `5e5f6fbe9c6e6e15fec4d9f6bbad305a5d8a5ca18856b05949deca446d7398b1`;
- the test disabled mobility through Helm at the end, deleted its Kind cluster and left no Kind
  cluster running.

The merge-commit baseline CI run
[`33863353459`](https://github.com/cagojeiger/ShiftPV/actions/runs/33863353459) also completed with
all seven jobs successful. No Home cluster, release, image, chart, tag, PR or merge was changed by
this validation step.
