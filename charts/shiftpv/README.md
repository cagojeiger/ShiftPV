# ShiftPV Helm chart

The chart installs one CSI controller Deployment, one CSI node DaemonSet, the
required provisioner/registrar/liveness sidecars, `CSIDriver`, RBAC, and a
chart-created StorageClass. The StorageClass is not the cluster default unless
explicitly enabled.

Each selected node must already have a writable filesystem mounted at
`node.poolRoot`. The chart never creates, formats, mounts, or repairs that
filesystem. Initial V1 records PVC capacity but does not enforce a hard write
limit.

```bash
helm install shiftpv ./charts/shiftpv \
  --namespace shiftpv-system --create-namespace
```

The CSI driver name, topology key, `WaitForFirstConsumer`, `Retain`, RWO
filesystem support, and disabled expansion are fixed product contracts.

Key configurable values:

| Value | Purpose |
|-------|---------|
| `image.repository`, `image.tag`, `image.pullPolicy` | ShiftPV image |
| `node.poolRoot` | pre-mounted host pool path on every participating node |
| `node.kubeletRootDir` | kubelet state root, normally `/var/lib/kubelet` |
| `node.nodeSelector`, `node.tolerations` | participating node selection |
| `helperPod.image`, `helperPod.timeout`, `helperPod.resources` | node-local directory helper |
| `storageClass.create`, `storageClass.name`, `storageClass.defaultClass` | StorageClass publication and explicit default-class opt-in |
| `controller.resources`, `node.resources`, `sidecars.*.resources` | workload resources |

The fixed `csi.shiftpv.io` name and cluster-scoped resources allow one ShiftPV
Helm release per cluster.

## Uninstall and recovery

Stop volume-using workloads before uninstalling. `helm uninstall` removes the
driver workloads, RBAC, `CSIDriver`, and chart-created `StorageClass`; it does
not delete user PVCs/PVs, dynamically-created reservation ConfigMaps, or data
directories. Existing Pods must not be treated as safely mounted while the node
plugin is absent.

Reinstall the release into the same namespace with the same `node.poolRoot`,
wait for controller and node Pods to become ready, and then restart the user
workload. `Retain` also means deleting a PVC leaves its PV and data for manual
recovery instead of calling CSI `DeleteVolume` automatically.
