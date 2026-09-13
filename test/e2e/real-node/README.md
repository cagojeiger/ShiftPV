# Real-node qualification

This lane qualifies ShiftPV only for its supported boundary: node-local RWO
planned cold mobility between recoverable nodes. It is not an HA, replication,
backup, or dead-disk recovery test.

## Stages

Run one stage at a time. A later stage must not start until the previous stage
has passed and all test resources have returned to the recorded baseline.

1. **Read-only preflight**: prove exact context, nodes, immutable candidate
   images, empty ShiftPV state, healthy pools/inventory, non-default StorageClass,
   SSH recovery access, and a fault node with no non-DaemonSet workload.
2. **Service interruption**: stop and restore the worker node's Kubernetes
   service during one precommit move and one postcommit source-cleanup move.
3. **Operating-system reboot**: repeat the two boundaries with a worker reboot.
4. **Hard power loss**: repeat them only after an independent out-of-band method
   for powering the worker back on has been demonstrated.
5. **Soak**: alternate a synthetic volume between nodes for at least 100 moves
   and 12 hours. Inject a controller restart every tenth move and a cleanup Pod
   deletion every twentieth move.
6. **Canary**: run one isolated synthetic PVC for 72 hours with at least 25
   scheduled moves. Do not place application data on the candidate StorageClass.

## Preflight

`preflight.sh` is intentionally read-only. It never relaxes its workload gate;
the fault node must be dedicated to the qualification apart from DaemonSets.

```bash
KUBECTL_CONTEXT=lab \
SOURCE_NODE=lab-worker-1 \
DESTINATION_NODE=lab-worker-2 \
FAULT_NODE=lab-worker-2 \
FAULT_SSH_TARGET=ubuntu@lab-worker-2 \
EXPECTED_CONTROLLER_IMAGE=registry.example/shiftpv-controller:candidate@sha256:... \
EXPECTED_NODE_IMAGE=registry.example/shiftpv-node:candidate@sha256:... \
./test/e2e/real-node/preflight.sh
```

The command must end with `PREFLIGHT_OK`. Treat `PREFLIGHT_BLOCKED` as a safety
decision, not as a test failure to bypass.

## Acceptance contract

Every interruption, soak iteration, and canary move must preserve all of these
invariants:

- PVC UID, PV name, and ShiftPV volume identity remain unchanged.
- Exactly one node is authoritative; no split ownership is observed.
- The payload checksum after recovery equals the checksum before interruption.
- Before ownership commit, recovery returns to the source. After commit, the
  move converges forward to the destination.
- Source cleanup reaches `Completed` with the exact executor/receipt identity.
  `NeedsReview` preserves data but fails qualification.
- The source copy is absent only after destination publication and ownership are
  proven; the destination copy remains present and published.
- Capacity holds, finalizers, helper Jobs/Pods, Moves, and test volumes return to
  baseline after cleanup.
- Both pools finish `Ready` with fresh, valid, non-truncated inventory.
- Non-test workloads and existing StorageClasses/PVs/PVCs are unchanged.

Archive the resource snapshots, events, component logs, node journal, checksums,
timestamps, and capacity readings for each run. A green command without this
evidence is not release qualification.
