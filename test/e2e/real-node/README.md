# Real-node qualification

This lane qualifies ShiftPV only for its supported boundary: node-local RWO
planned cold mobility between recoverable nodes. It is not an HA, replication,
backup, or dead-disk recovery test.

## Stages

Run one stage at a time. A later stage must not start until the previous stage
has passed and all test resources have returned to the recorded baseline.

1. **Read-only preflight**: prove exact context, nodes, immutable candidate
   images, no active Volume or unsettled Move, healthy pools/inventory,
   non-default StorageClass, SSH recovery access, and either an isolated fault
   node or an exact reviewed shared-workload inventory.
2. **Service interruption**: stop and restore the worker node's Kubernetes
   service during one precommit move and one postcommit source-cleanup move.
3. **Operating-system reboot**: repeat the two boundaries with a forced worker
   reboot, prove the boot ID changed, and hold MicroK8s stopped after boot until
   the control plane has observed the node unavailable.
4. **Soak**: alternate a synthetic volume between nodes for at least 100 moves
   and 12 hours. Inject a controller restart every tenth move and a cleanup Pod
   deletion every twentieth move.

A physical power cut is an optional qualification for a specific host and
filesystem, and requires an independently proven out-of-band power-on path.
A separate low-frequency canary is not a release gate: it repeats the same
synthetic move contract already exercised more densely by the soak.

## Preflight

`preflight.sh` is intentionally read-only. Its default workload gate requires a
dedicated fault node apart from DaemonSets. For an explicitly approved shared
node maintenance window, hash the exact sorted inventory printed by a blocked
run and pass it as `EXPECTED_NON_DAEMONSET_PODS_SHA256`. A changed Pod name,
owner, or count blocks the run rather than broadening the approval.

```bash
KUBECTL_CONTEXT=lab \
SOURCE_NODE=lab-worker-1 \
DESTINATION_NODE=lab-worker-2 \
FAULT_NODE=lab-worker-2 \
FAULT_SSH_TARGET=ubuntu@lab-worker-2 \
EXPECTED_CONTROLLER_IMAGE=registry.example/shiftpv-controller:candidate@sha256:... \
EXPECTED_NODE_IMAGE=registry.example/shiftpv-node:candidate@sha256:... \
EXPECTED_NON_DAEMONSET_PODS_SHA256=... \
./test/e2e/real-node/preflight.sh
```

The command must end with `PREFLIGHT_OK`. Treat `PREFLIGHT_BLOCKED` as a safety
decision, not as a test failure to bypass.

## Service interruption

`service-interruption.sh` performs only stage 2. The fault node must be the
source node. It first interrupts the source during `Copying` and proves
`ResumeOwner` restores the original PVC/PV/volume identity and checksum. It then
interrupts the source after the owner commit but before source cleanup, and
proves cleanup eventually reaches `Completed` after the node returns.

The interruption stops the MicroK8s container runtime and kubelite units
together so a small copy cannot commit while a serial full-snap shutdown is
still in progress. Recovery starts the full MicroK8s snap. Settled terminal Move
journals remain as durable evidence: `Succeeded` requires cleanup `Completed`,
and recovered `Blocked` requires `RecoverySettled`; both must have no finalizer.
They do not block later qualification runs.

The script never drains the shared node and never edits non-ShiftPV workload
specs. Those workloads still experience downtime while MicroK8s is stopped. It
records their UID and specs plus all existing PVCs, PVs, and StorageClasses
before the test and requires exact equality afterward. Controller-owned mutable
labels and annotations are excluded from this baseline comparison; full resource
snapshots remain in the evidence bundle. A failed run restores MicroK8s and
uncordons both nodes, but deliberately preserves test storage for diagnosis.

```bash
KUBECTL_CONTEXT=home-prod-kr \
SOURCE_NODE=server-02 \
DESTINATION_NODE=server-01 \
FAULT_NODE=server-02 \
FAULT_SSH_TARGET=ubuntu@home-server-02 \
SOURCE_SSH_TARGET=ubuntu@home-server-02 \
DESTINATION_SSH_TARGET=ubuntu@home-server-01 \
EXPECTED_CONTROLLER_IMAGE=ghcr.io/example/controller:candidate@sha256:... \
EXPECTED_NODE_IMAGE=ghcr.io/example/node:candidate@sha256:... \
EXPECTED_NON_DAEMONSET_PODS_SHA256=... \
./test/e2e/real-node/service-interruption.sh
```

## Operating-system reboot

Stage 3 reuses the same workload, identity, checksum, cleanup, and baseline
gates as stage 2. Set `FAULT_MODE=reboot`. Each fault immediately reboots the
source host with `systemctl reboot --force --force`, waits for a different boot
ID, then stops the MicroK8s runtime and kubelite together until Kubernetes has
observed the node unavailable. This makes the fault boundary deterministic
without claiming a physical power cut.

```bash
FAULT_MODE=reboot \
KUBECTL_CONTEXT=home-prod-kr \
SOURCE_NODE=server-02 \
DESTINATION_NODE=server-01 \
FAULT_NODE=server-02 \
FAULT_SSH_TARGET=ubuntu@home-server-02 \
SOURCE_SSH_TARGET=ubuntu@home-server-02 \
DESTINATION_SSH_TARGET=ubuntu@home-server-01 \
EXPECTED_CONTROLLER_IMAGE=ghcr.io/example/controller:candidate@sha256:... \
EXPECTED_NODE_IMAGE=ghcr.io/example/node:candidate@sha256:... \
EXPECTED_NON_DAEMONSET_PODS_SHA256=... \
./test/e2e/real-node/service-interruption.sh
```

This proves the supported unclean OS reboot boundary, not physical power-loss
durability. A physical power-cut qualification requires an independently
demonstrated IPMI, managed-PDU, smart-plug, or other out-of-band power-on path.

## Soak

`soak.sh` alternates one synthetic PVC between the two reviewed nodes while
preserving its PVC/PV/ShiftPV identity and payload checksum. It proves the old
copy is absent and the new serving copy is present after every move. Every
tenth move restarts the controller; every twentieth move deletes the active
cleanup Pod and requires the move to converge through retry.

`CONTROLLER_RESTART_EVERY` and `CLEANUP_POD_DELETE_EVERY` may lower those
intervals for a short fault-injection smoke run. The release gate uses their
defaults, 10 and 20.

The release gate requires both 100 successful moves and 12 elapsed hours. The
script spaces moves across `MIN_DURATION_SECONDS=43200` by default and prints
`REAL_NODE_SOAK_OK` only when both thresholds were actually met. Shorter runs
are smoke tests and print `REAL_NODE_SOAK_SMOKE_OK`; they are not release
qualification.

Because other controllers remain active during a long soak, the baseline gate
compares non-test resource UID and spec rather than controller-owned mutable
labels or annotations. Full resource snapshots are still archived for review.

```bash
KUBECTL_CONTEXT=home-prod-kr \
SOURCE_NODE=server-02 \
DESTINATION_NODE=server-01 \
FAULT_NODE=server-02 \
FAULT_SSH_TARGET=ubuntu@home-server-02 \
SOURCE_SSH_TARGET=ubuntu@home-server-02 \
DESTINATION_SSH_TARGET=ubuntu@home-server-01 \
EXPECTED_CONTROLLER_IMAGE=ghcr.io/example/controller:candidate@sha256:... \
EXPECTED_NODE_IMAGE=ghcr.io/example/node:candidate@sha256:... \
EXPECTED_NON_DAEMONSET_PODS_SHA256=... \
ITERATIONS=100 \
MIN_DURATION_SECONDS=43200 \
./test/e2e/real-node/soak.sh
```

## Acceptance contract

Every interruption and soak iteration must preserve all of these
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
- Capacity holds, finalizers, helper Jobs/Pods, and test volumes return to the
  safe baseline after cleanup. Move journals may remain only in a settled
  terminal state with no finalizer or capacity hold.
- Both pools finish `Ready` with fresh, valid, non-truncated inventory.
- Non-test workload configuration and existing StorageClasses/PVs/PVCs are
  unchanged. A HorizontalPodAutoscaler may change its target's live
  `spec.replicas`; that controller-owned field is excluded while the rest of
  the workload spec remains under exact comparison.

Archive the resource snapshots, events, component logs, node journal, checksums,
timestamps, and capacity readings for each run. A green command without this
evidence is not release qualification.
