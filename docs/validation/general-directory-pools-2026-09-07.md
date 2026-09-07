# General directory Pool validation — 2026-09-07

## Scope

- commit: `f9da731`
- local host: macOS arm64 with Docker Desktop
- Kubernetes: kind `v1.35.8`, two storage workers
- Pool paths: `/var/lib/shiftpv-directory-pool-a` and
  `/var/lib/shiftpv-directory-pool-b` inside the Kind workers' root filesystems

Both paths were checked with `mountpoint -q` and were not mount points. This
validation covers existing ordinary directories, not only separately mounted
filesystems.

## Executed checks

```bash
make verify

DIRECTORY_POOL_ONLY=1 \
  CLUSTER_NAME=shiftpv-directory-mobility-focused \
  ./test/e2e/kind/run.sh

CLUSTER_NAME=shiftpv-general-directory-full-3 \
  ./test/e2e/kind/run.sh

CLUSTER_NAME=shiftpv-general-directory-mobility-full \
  ./test/e2e/kind/mobility/run.sh
```

## Observed results

- `make verify` passed with race detection and 82.8% statement coverage, plus
  vet, builds, release checks, ShellCheck, Helm lint, and deterministic template checks.
- A missing Pool directory reported `Ready=False/PathMissing` and was not created
  by ShiftPV. Creating the directory changed it to
  `Accessible=True/DirectoryAccessible` and aggregate `Ready=True`.
- A PVC provisioned through `csi.shiftpv.io`, the Pod and node host path exposed
  identical payload data, and `Delete` cleanup removed the live volume directory,
  reservation, Volume state, and PV.
- Cordon moved an RWO Filesystem volume from worker A's ordinary directory to
  worker B's ordinary directory. The checksum and PVC/PV identity were preserved,
  owner authority moved to B, and the source was retained under the move journal.
- The full kind suite passed Pool physical/logical capacity admission, default and
  coexisting StorageClasses, controller/node replacement, Pod recreation, denied
  uninstall, emergency reinstall recovery, ENOSPC/read-only deletion recovery,
  and ENOSPC/read-only mobility recovery.
- The closed-loop mobility suite passed selector, affinity, taint, and PDB
  preflight; `Blocked/CopyFailed` owner recovery; Controller replacement during
  Copying, Promoting, and Committing; and post-commit destination recovery.

The first full run exposed that a retained terminal Move whose Volume and
reservation had both been deleted blocked later capacity admission. The final
commit ignores that audit-only record in both provisioning and mobility capacity
calculations while still rejecting a reservation-without-Volume partial state.
The two final full runs above passed with the terminal Move deliberately retained.

## Boundary

This is source-build evidence in isolated kind clusters. It does not prove that
controller `0.1.7`, node `0.1.3`, or chart `0.1.7` have been published, nor that a
specific Home cluster path and capacity limit are ready. Published-artifact and
Home deployment validation must occur after merge and release.
