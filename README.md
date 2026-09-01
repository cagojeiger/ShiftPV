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
- owner-node topology and publish validation
- Helm installation and recovery after Helm uninstall/reinstall
- isolated two-worker kind E2E validation

## Requirements

- Kubernetes 1.35 or newer
- Linux nodes with privileged DaemonSet and HostPath access
- a writable local filesystem or directory prepared at the same configured path
  on every participating node; the default is `/mnt/shiftpv`

Each participating node must use a distinct local path. ShiftPV does not create,
format, mount, inspect or repair filesystems.

## Limitations

- data remains on one owner node and is not copied to another node
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

Early `dev-v1` bootstrap. The CSI/Helm lifecycle has been validated in an
isolated Kubernetes 1.35.8 kind cluster with two distinct worker directories.

## License

Apache-2.0 — see [LICENSE](LICENSE).
