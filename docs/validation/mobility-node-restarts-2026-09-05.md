# Mobility node-container restart recovery — 2026-09-05

## Scope

This is the third bounded SP-STAB-005 fault-validation slice. It stops and starts
the actual Kind container for each storage node while a Move is active but before
copy-side disk work begins. It verifies safe source-owner recovery; it does not
claim dead-source failover, mid-rsync recovery, or post-commit node-loss coverage.

The expected result is deliberately not automatic continuation. A source or
selected destination that becomes unavailable before owner commit closes the Move
as terminal `Blocked`. The existing source remains authoritative until both the
node prerequisite is restored and an operator requests the one-way, idempotent
`ResumeOwner` recovery.

## Scenarios

| Fault | Expected terminal state | Authority invariant | Recovery |
| --- | --- | --- | --- |
| Source Kind container stops after eviction and replacement scheduling | `Blocked/SourceUnavailable` | Volume owner and payload remain on source; no destination final directory | Start source, wait Ready, uncordon it, request `ResumeOwner` |
| Selected destination Kind container stops before copy | `Blocked/InvalidDestination` | Volume owner and payload remain on source; no destination final directory | Start destination, wait Ready, uncordon source, request `ResumeOwner` |

Both cases must finish with the original PVC UID and PV, `Ready` on the original
source owner, an empty `activeMove`, a replacement writer on that source, and the
same payload checksum.

## Harness

The focused test uses an isolated three-node Kind cluster and real
`docker stop -t 1` / `docker start` operations. It pauses the Controller only to
make the fault boundary deterministic:

1. create a source workload while the destination is cordoned;
2. uncordon the destination, cordon the source, and wait for the generated Move;
3. wait for `WaitingForDestination`, scale the Controller to zero, and confirm the
   held replacement has been scheduled on the destination;
4. stop one node and wait until Kubernetes reports its Ready condition as
   `False` or `Unknown`;
5. restart the Controller and assert the expected `Blocked` reason and source
   authority;
6. restart the node, wait for Ready and the Node DaemonSet, then request
   `ResumeOwner` and verify identity, owner, mount, and checksum.

When the destination is stopped, the source is uncordoned after the transaction
has passed preflight so the Controller has a schedulable worker. This does not
start another Move or change storage authority.

Run only this slice with:

```bash
MOBILITY_NODE_RESTARTS_ONLY=1 \
  CLUSTER_NAME=shiftpv-mobility-node-restart-focused \
  ./test/e2e/kind/run.sh
```

## Result

The clean local rerun completed both cases without manual intervention.

- source checksum:
  `50ba0cb43518e42a4b50fe866646cd5a42ff2328e9e5700c1f641dc405ab4d09`;
- destination-fault case checksum:
  `0146b36285c8b2fe360d16f4a2db94d173613f9efccd1fe6a615b2661adc709f`;
- both Moves retained their terminal failure history while
  `status.recoveryPhase=Recovered` and the Volume returned to source-owned Ready;
- the focused runner printed
  `ShiftPV mobility node-container restart recovery passed` and exited zero;
- the isolated Kind cluster and temporary pool directories were removed.

The first exploratory run exposed a test-only scheduling deadlock: source was
cordoned while destination was down, so a scaled-up Controller had no worker.
The harness now uncordons source only after preflight for the destination-fault
case. The same run then completed, and the following clean run proved the fix.

`make verify` also passed with race coverage 84.4%, including shellcheck,
actionlint, release resolver tests, Helm lint, and deterministic template checks.

## CI responsibility

`kind-mobility-node-restart-e2e` runs this focused mode in its own cluster and
uploads node, workload, ShiftPV CR, Event, and Kind logs on failure. This keeps a
slow real node-heartbeat fault out of unit tests and independent from the larger
closed-loop mobility suite.

## Remaining boundary

This result does not close all node-restart risk. A later slice still needs to
stop source or destination during copy/promotion and after owner commit, then
distinguish restartable Job behavior from a safe terminal block. Permanent source
loss remains outside the product contract.
