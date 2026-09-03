# ShiftPV

ShiftPV is a CSI driver for directory-backed local volumes on filesystems that
an operator has already prepared on Kubernetes nodes.

## Current scope

ShiftPV dynamically provisions RWO Filesystem volumes through a standard
StorageClass and bind-mounts each volume from its owner node into a Pod.

```text
StorageClass
    ↓
PVC → ShiftPV Controller → owner node directory
    ↓
Pod → ShiftPV Node Plugin → bind mount
```

The current implementation provides:

- CSI Identity, Controller and Node services
- `Retain` and `WaitForFirstConsumer` StorageClass
- optional cluster-default StorageClass annotation
- deterministic volume IDs and namespace-scoped reservation ConfigMaps
- explicit node Pool registration with per-node mount paths and dynamic owner publish guard
- automatic healthy-node cordon cold migration with Placement Hold, authenticated rsync,
  dynamic owner CAS and restart-safe reconciliation
- fail-closed Helm/Argo CD Application uninstall guard and explicit recovery bypass
- isolated Helm, mobility, and Argo CD kind E2E validation

## Requirements

- Kubernetes 1.35 or newer
- Linux nodes with privileged DaemonSet and HostPath access
- Argo CD 3.3 or newer when Application deletion must run the uninstall guard
- a writable local filesystem prepared on every participating node; each node's
  absolute mount path is declared by its `ShiftPVPool`

Each participating node must use a distinct local path. ShiftPV does not create,
format, mount, inspect or repair filesystems.

Register every participating node explicitly with a `ShiftPVPool` CR after
installing the chart.

Automatic mobility applies only to workload namespaces explicitly labeled
`shiftpv.io/admission=enabled`.

## Limitations

- data has exactly one authoritative owner; automatic movement is planned cold migration from
  a healthy cordoned node, not failover from an unavailable node
- automatic mobility supports one controller-owned consumer and one RWO Filesystem ShiftPV PVC;
  `Blocked` is terminal and waiting phases have no deadline
- no replication, HA, failover, backup or snapshot
- RWO Filesystem only; no RWX or raw block
- no volume expansion
- requested capacity is recorded but not enforced as a write limit
- deleting a PVC leaves its `Retain` PV and data for manual recovery

## Documentation

- [Documentation map](docs/README.md)
- [Architecture decisions](docs/adr/README.md)
- [Current contracts](docs/spec/README.md)
- [Development and CI checks](docs/development/testing.md)
- [Runtime validation](docs/validation/README.md)

## Status

Early `dev-v1` bootstrap. The CSI lifecycle and Helm/Argo CD removal guards have
been validated in isolated Kubernetes 1.35.8 kind clusters.

## License

Apache-2.0 — see [LICENSE](LICENSE).
