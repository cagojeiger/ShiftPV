# Isolated Kind E2E

Status: 0.4 target suite contract. A scenario is evidence only after its product path is implemented and the
script completes without direct status success patches or host-side replacement effects.

The suite uses one control-plane and at least two Linux workers backed by different temporary host directories.
A shared directory cannot prove node-local storage behavior.

```text
control-plane
├── worker-a ── /mnt/shiftpv-a
└── worker-b ── /srv/shiftpv-b
```

## Requirements

- healthy Docker-compatible engine
- kind 0.33.0 or newer
- kubectl and Helm 3
- space for pinned node, CSI sidecar, and ShiftPV images

```bash
./test/e2e/kind/run.sh
```

## Required scenarios

| Area | Evidence |
|---|---|
| Pool | existing ordinary directories, exact identity, generation-fenced complete inventory |
| Provision | `WaitForFirstConsumer`, exact topology, Volume owner hold before directory effect |
| Publish | RWO owner-only bind mount and scan/publish race serialization |
| Capacity | filesystem pressure plus Volume/Move holds; no release before cleanup closure |
| Retain/Delete | data preservation, explicit retirement, Volume journal/finalizer convergence |
| Mobility | bidirectional cold move, stable PVC/PV/handle/checksum, one owner |
| Restart | Controller and Node restart at every intent/effect/receipt boundary |
| Node outage | source and destination stop/start before and after owner commit |
| Filesystem fault | partial copy, ENOSPC, inode exhaustion, read-only, checksum mismatch |
| GC | parent-owned cleanup only; unknown orphan report-only and Pool removal blocked |
| Removal | mounted, retained, moving, deleting, hold, stale inventory and API error all fail closed |

Every scenario checks API state, actual paths and mounts, capacity holds, logs/events, restart count and final
fixture cleanup. One successful Move or checksum is not sufficient.

## Focused runs

```bash
POOL_CAPACITY_ONLY=1 CLUSTER_NAME=shiftpv-capacity-focused ./test/e2e/kind/run.sh
DIRECTORY_POOL_ONLY=1 CLUSTER_NAME=shiftpv-directory-focused ./test/e2e/kind/run.sh
VOLUME_DELETE_CLEANUP_ONLY=1 CLUSTER_NAME=shiftpv-delete-focused ./test/e2e/kind/run.sh
CLEANUP_JOB_RETRY_ONLY=1 CLUSTER_NAME=shiftpv-cleanup-retry-focused ./test/e2e/kind/run.sh
MOBILITY_FILESYSTEM_FAULTS_ONLY=1 CLUSTER_NAME=shiftpv-mobility-fs-focused ./test/e2e/kind/run.sh
MOBILITY_NODE_RESTARTS_ONLY=1 CLUSTER_NAME=shiftpv-mobility-restart-focused ./test/e2e/kind/run.sh
```

Each run owns a unique cluster name, kubeconfig, image tag and host directories. It removes exactly those
resources on success or failure. `KEEP_CLUSTER=1` is for bounded diagnosis only.

The mobility-specific contract is in [`mobility/README.md`](mobility/README.md). Argo CD removal and public
artifact provenance use isolated suites in [`argocd/`](argocd/README.md) and
[`artifact/`](artifact/README.md).
