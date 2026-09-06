# Mobility node-container restart recovery — 2026-09-05

## Scope

This validation now includes the third and fourth bounded SP-STAB-005
fault-validation slices. It stops and starts the actual Kind container for each
storage node before disk-side work, in `Copying`, in `Promoting`, and after owner
commit. It verifies safe source-owner recovery plus automatic continuation after
a selected destination returns. It does not claim dead-source failover or that an
individual rsync/rename system call is interrupted at a deterministic byte boundary.

Before destination selection, a lost source closes the Move as terminal `Blocked`.
A source loss during `Copying` also blocks because the authoritative copy cannot
be trusted. Once the scheduler assigns the placement reservation, destination
loss before or during copy keeps the source authoritative, holds the real workload,
and waits automatically. The same rule applies in `Promoting` and pre-commit
`Committing`. After commit, destination authority remains and source cleanup waits.
Destination-return paths continue automatically; blocked source-owner paths require
the one-way, idempotent `ResumeOwner` recovery.

## Scenarios

| Fault | Expected terminal state | Authority invariant | Recovery |
| --- | --- | --- | --- |
| Source Kind container stops after eviction and replacement scheduling | `Blocked/SourceUnavailable` | Volume owner and payload remain on source; no destination final directory | Start source, wait Ready, uncordon it, request `ResumeOwner` |
| Selected destination Kind container stops before copy | `Copying/DestinationUnavailable` | Volume owner and payload remain on source; real replacement remains held | Start destination; automatic continuation |
| Source stops in `Copying` | `Blocked/SourceUnavailable` | Source remains authoritative even if staging work exists | Start source and request `ResumeOwner` |
| Destination stops in `Copying` | `Copying/DestinationUnavailable` | Source remains authoritative; no commit | Start destination; automatic continuation |
| Destination stops in `Promoting` | `Promoting/DestinationUnavailable` | Source remains authoritative; completed promotion cannot trigger commit while destination is unavailable | Start destination; automatic continuation |
| Destination stops after owner commit | `WaitingForDestinationPublish/DestinationUnavailable` | Destination remains authoritative; source payload is retained | Start destination; automatic publish and cleanup |

All cases retain the original PVC UID, PV, and payload checksum. Blocked cases
finish recovered and `Ready` on source. Automatically continued cases finish
`Succeeded` and `Ready` on destination with an empty `activeMove` and retired
source directory.

## Harness

The focused test uses an isolated three-node Kind cluster and real
`docker stop -t 1` / `docker start` operations. It pauses the Controller only to
make the fault boundary deterministic:

1. create a source workload while the destination is cordoned;
2. uncordon the destination, cordon the source, and wait for the generated Move;
3. wait for the scenario's exact fault phase and scale the Controller to zero;
4. stop one node and wait until Kubernetes reports its Ready condition as
   `False` or `Unknown`;
5. restart the Controller and assert either the expected source-failure Blocked reason or
   a `DestinationUnavailable` self-loop with the phase-specific owner and held workload;
6. restart the node, wait for Ready and the Node DaemonSet, then either request
   `ResumeOwner` or observe automatic continuation; verify identity, owner, source
   retirement policy, mount, and checksum.

When the destination is stopped, the source is uncordoned after the transaction
has passed preflight so the Controller has a schedulable worker. This does not
start another Move or change storage authority.

Run only this slice with:

```bash
MOBILITY_NODE_RESTARTS_ONLY=1 \
  CLUSTER_NAME=shiftpv-mobility-node-restart-focused \
  ./test/e2e/kind/run.sh
```

실패한 경계만 재현할 때는 `MOBILITY_NODE_RESTART_CASE`에 `source`, `destination`,
`copying-source`, `copying-destination`, `promoting-destination`,
`committed-destination` 중 하나를 지정한다. 지정하지 않으면 여섯 사례를 모두 실행한다.

## Result

The earlier clean local rerun completed all six phase cases. The event-driven
placement change was then revalidated with a full mobility suite plus isolated
pre-copy and post-commit destination restart cases. Every isolated cluster was removed.

- source checksum:
  `50ba0cb43518e42a4b50fe866646cd5a42ff2328e9e5700c1f641dc405ab4d09`;
- current pre-copy destination-fault checksum:
  `ee6dc1b4bb9d256815dfb32efbd50aabfe422e922ff69e609e5e4d392d94630a`;
- Copying-source checksum:
  `c695053f0f6cbb779c47815a204f1d61bad08d5d3dda4bf0f78e3e33f67a6f3a`;
- Copying-destination checksum:
  `8bc2970a7ac7fc25e260e978a39ec016d51e4585c0e0ce00453a5c9d70b30198`;
- Promoting-destination checksum:
  `79944caecd62d43d8115a167e31a6c5bc047c92c15f37821afaf5e117fa82432`;
- committed-destination checksum:
  `09def66b59517b3e7623ecd0a8bdd3d2fe7399dca3f617148e7142aefb5f2e91`;
- source-failure Blocked Moves retained their terminal failure history while
  `status.recoveryPhase=Recovered` and the Volume returned to source-owned Ready;
- destination-loss Moves waited without crossing their owner boundary and
  automatically reached `Succeeded` after the destination returned;
- the post-commit case verified the destination workload's kubelet
  `vol_data.json`, found no missing-volume-data journal error, observed
  `shiftpv.io/placement=owner` with the Hold removed, and found no reservation Pod;
- the focused runner printed
  `ShiftPV mobility node-container restart recovery passed` and exited zero;
- the isolated Kind cluster and temporary pool directories were removed.

The first exploratory run exposed a test-only scheduling deadlock: source was
cordoned while destination was down, so a scaled-up Controller had no worker.
The harness now uncordons source only after preflight for the destination-fault
case. The same run then completed, and the following clean run proved the fix.

The final event-driven placement evaluator's `make verify` passed with race coverage
82.6%, including shellcheck,
actionlint, release resolver tests, Helm lint, and deterministic template checks.

## CI responsibility

`kind-mobility-node-restart-e2e` runs this focused mode in its own cluster and
uploads node, workload, ShiftPV CR, Event, and Kind logs on failure. This keeps a
slow real node-heartbeat fault out of unit tests and independent from the larger
closed-loop mobility suite.

## Remaining boundary

This result closes the phase-level storage-node restart matrix currently in scope.
It does not prove crash consistency inside a filesystem or rsync/rename syscall,
nor permanent source loss, replication, backup, or automatic failover; those remain
outside the product contract.
