# Isolated kind E2E

This test creates a dedicated Kubernetes 1.35.8 cluster with one control-plane
node and two workers. Each worker mounts a different temporary host directory at
`/mnt/shiftpv`; a shared directory would not prove node-local behavior.

Requirements:

- a healthy Docker-compatible engine
- kind 0.33.0 or newer
- kubectl and Helm 3
- enough space for the pinned kind node and CSI sidecar images

Run from the repository root:

```bash
./test/e2e/kind/run.sh
```

The script builds and loads `shiftpv:dev` and installs the Helm chart with
ShiftPV marked as the default StorageClass. It creates a PVC without
`storageClassName`, verifies Kubernetes defaults it to `shiftpv`, provisions it
through `csi.shiftpv.io`, and starts a Pod that writes through the mounted RWO
filesystem volume. It force-replaces the controller Pod and the node plugin Pod
on the volume owner node, verifies both UIDs change without losing the mounted
data, then verifies checksum retention after ordinary Pod recreation. It stops
the workload, uninstalls Helm, verifies that the
PVC/PV/reservation/host file remain, reinstalls the same release, and verifies
the checksum again. Finally, it installs an unrelated default StorageClass,
reinstalls ShiftPV with `defaultClass=false`, verifies an implicit PVC keeps the
existing default, and provisions an explicitly selected ShiftPV PVC and Pod.

The cluster and its temporary host directories are removed on exit. Set
`KEEP_CLUSTER=1` only while diagnosing a failure.
