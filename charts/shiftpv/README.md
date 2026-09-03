# ShiftPV Helm chart

The chart installs one CSI controller Deployment, one CSI node DaemonSet, the
required provisioner/registrar/liveness sidecars, `CSIDriver`, RBAC, and a
chart-created StorageClass. With `mobility.enabled=true`, the same Controller
Pod also runs the automatic cordon reconciler and HTTPS admission webhook. The
StorageClass is not the cluster default unless explicitly enabled.

Each selected node must already have a writable filesystem mounted at the path
declared by that node's `ShiftPVPool.spec.mountPath`. Paths may differ by node.
The chart never creates, formats, mounts, or repairs those filesystems. Initial
V1 records PVC capacity but does not enforce a hard write limit.

```bash
helm install shiftpv ./charts/shiftpv \
  --namespace shiftpv-system --create-namespace
```

After installation, explicitly register one `ShiftPVPool` for every participating
node before provisioning volumes. `spec.mountPath` is the runtime authority used
by provisioning helpers, mobility Jobs, and the node plugin on that node.

```yaml
apiVersion: shiftpv.io/v1alpha1
kind: ShiftPVPool
metadata:
  name: storage-worker-a
spec:
  nodeName: worker-a
  mountPath: /mnt/shiftpv
```

Pool CRs are cluster operating state; the Helm release does not create or own
them. Because the privileged Node Plugin resolves these paths through a host-root
mount, permission to create or change Pool CRs is security-sensitive and must be
restricted to cluster storage operators. Root (`/`) is rejected.

Automatic mobility is opt-in per workload namespace. Label only namespaces whose
controller-owned, single-PVC workloads follow the current mobility contract.

```bash
kubectl label namespace my-workload shiftpv.io/admission=enabled
```

The placement webhook is registered only for labeled namespaces and uses
`failurePolicy=Fail`. In those namespaces it pins a bound ShiftPV volume to its
dynamic owner or applies a Placement Hold while moving. Volumes provisioned outside
the opt-in boundary receive owner-only PV topology and do not depend on the
webhook. The Controller Deployment is fixed to one replica with `Recreate`
strategy so two mobility reconcilers do not overlap.

The CSI driver name, topology key, `WaitForFirstConsumer`, `Retain`, RWO
filesystem support, and disabled expansion are fixed product contracts.

Key configurable values:

| Value | Purpose |
|-------|---------|
| `controller.image.*` | Controller and CSI provisioner-facing binary image |
| `node.image.*` | Node Plugin binary image |
| `mobility.helperImage` | rsync-capable helper image; the Controller image satisfies this contract |
| `mobility.enabled`, `mobility.interval`, `mobility.webhookPort` | cordon reconciler and admission endpoint |
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
not delete CRDs, user PVCs/PVs, Pool/Volume/Move CRs, dynamically-created
reservation ConfigMaps, or data directories. It does remove the webhook
configuration and TLS Secret. Existing Pods must not be treated as safely mounted
while the node plugin is absent, and an active Move must reach `Succeeded` or
`Blocked` before uninstall.

Reinstall the release into the same namespace, restore the same Pool CRs and
mount paths, wait for controller and node Pods to become ready, and then restart
the user workload. `Retain` also means deleting a PVC leaves its PV and data for
manual recovery instead of calling CSI `DeleteVolume` automatically.
