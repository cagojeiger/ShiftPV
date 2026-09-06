# Event-driven placement validation — 2026-09-05

## Scope

This validation replaces the rejected `NodePublishVolume` wait with a controller-side
placement reservation. The real replacement workload remains scheduling-gated while
kube-scheduler selects a destination for an internal reservation Pod. ShiftPV pins the
held workload, copies and promotes data, commits destination authority, deletes the
reservation, and only then releases the workload.

The Node Plugin still performs one live `ShiftPVVolume` read and immediately fails
closed for `Moving`, `Blocked`, owner mismatch, or API failure. CSI requests do not
poll and do not open Kubernetes watches.

## Event and API behavior

- Node, managed workload Pod, mobility Pod/Job, Pool, Volume, and Move watches feed
  one buffered wake channel.
- 100 wake notifications coalesce to one pending reconcile signal.
- A closed watch reconnects; a canceled context stops without reopening.
- With a one-hour test safety interval, the reconciler performs one initial scan,
  stays idle without another scan, and reacts immediately to one wake signal.
- The chart and controller default safety interval is 30 seconds. Compared with the
  previous 2-second idle scan, this changes the fallback bound from 1,800 to 120 full
  scans per hour, a 15x or 93.3% reduction. Relevant events remain the fast path.
- 64 concurrent `Moving` publishes produced exactly 64 live state reads and 64
  `FailedPrecondition` results, with no bind attempt or request-scoped wait.

These are deterministic behavior and call-count checks, not an API-server throughput
benchmark.

## Scheduler reservation contract

The reservation Pod is owned by the exact Move UID and has a deterministic name. It
copies candidate affinity, supported node constraints, tolerations, scheduler and
runtime settings, host ports, and aggregate workload resource requests. The controller
persists destination plus replacement name/UID before creating copy resources. A
foreign same-named Pod is rejected, and deletion uses a UID precondition.
If the reservation disappears before commit, the FSM recreates it only on the
persisted destination and waits for scheduling again. Owner commit also performs a
live exact-UID/destination reservation check.

Inter-Pod affinity/anti-affinity, topology spread, resource claims, generic ephemeral
volumes, inline CSI volumes, custom schedulers, pre-existing scheduling gates, and
unsupported multi-consumer/PVC shapes fail closed before eviction because the
reservation cannot preserve their scheduling semantics.

## Runtime result

The final evaluator passed from a clean set of fresh Kind clusters:

```bash
make verify && \
go test -race ./src/csi/node ./src/mobility/controller ./src/mobility/fsm && \
CLUSTER_NAME=shiftpv-event-placement ./test/e2e/kind/mobility/run.sh && \
CLUSTER_NAME=shiftpv-event-placement-restart \
  MOBILITY_NODE_RESTARTS_ONLY=1 MOBILITY_NODE_RESTART_CASE=destination \
  ./test/e2e/kind/run.sh && \
CLUSTER_NAME=shiftpv-event-placement-committed \
  MOBILITY_NODE_RESTARTS_ONLY=1 MOBILITY_NODE_RESTART_CASE=committed-destination \
  ./test/e2e/kind/run.sh
```

`make verify` reported 82.6% statement coverage. The complete evaluator exited zero.

The full isolated Kind mobility suite passed:

```bash
CLUSTER_NAME=shiftpv-event-placement ./test/e2e/kind/mobility/run.sh
```

It covered selector/affinity/taint and PDB preflight, obsolete pre-lock cancellation,
copy failure and `ResumeOwner`, normal movement, controller restarts in `Copying`,
`Promoting`, and `Committing`, post-commit recovery, checksum preservation, certificate
reconciliation, and mobility disable behavior.

Two fresh-cluster destination restart boundaries also passed:

```bash
CLUSTER_NAME=shiftpv-event-placement-restart \
  MOBILITY_NODE_RESTARTS_ONLY=1 MOBILITY_NODE_RESTART_CASE=destination \
  ./test/e2e/kind/run.sh

CLUSTER_NAME=shiftpv-event-placement-committed \
  MOBILITY_NODE_RESTARTS_ONLY=1 MOBILITY_NODE_RESTART_CASE=committed-destination \
  ./test/e2e/kind/run.sh
```

- Before copy, the destination container stopped after reservation scheduling. The
  Move waited in `Copying/DestinationUnavailable`, the source stayed authoritative,
  the real workload stayed held, and destination return completed automatically with
  checksum `ee6dc1b4bb9d256815dfb32efbd50aabfe422e922ff69e609e5e4d392d94630a`.
- After owner commit, destination loss retained destination authority and postponed
  source cleanup. Return completed with checksum
  `09def66b59517b3e7623ecd0a8bdd3d2fe7399dca3f617148e7142aefb5f2e91`.
- The committed-destination rerun verified the live Pod's kubelet `vol_data.json`,
  found no missing-file journal error, confirmed the placement Hold was gone and
  `shiftpv.io/placement=owner` was present, and found no reservation Pod.
- All test clusters and temporary pool directories were removed.

## Boundary

This proves bounded event-driven reconciliation and supported scheduling shapes on
Kind. It does not benchmark a production API server, physical disk, or network, and
does not add replication, dead-source recovery, backup, or crash-consistent snapshots.
Reservation deletion and workload-gate release are separate Kubernetes updates; if
another workload consumes the freed capacity in that handoff, authority remains safe
and source cleanup waits, but application scheduling can remain Pending until capacity
returns.
