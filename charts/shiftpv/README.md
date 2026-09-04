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
helm repo add shiftpv https://cagojeiger.github.io/ShiftPV
helm repo update shiftpv
helm install shiftpv shiftpv/shiftpv \
  --namespace shiftpv-system --create-namespace
```

For repository development, replace `shiftpv/shiftpv` with
`./charts/shiftpv`. Chart versions and component image versions are independent:
this chart starts at `0.1.0` and its defaults select the separately released
controller and node images. Override those image values together only when
testing an unpublished build.

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

The Controller creates and reconciles the webhook TLS Secret, mobility
`MutatingWebhookConfiguration`, and lifecycle `ValidatingWebhookConfiguration`;
Helm renders only the stable webhook Service.
It checks once per minute, renews the 90-day serving certificate 30 days before
expiry, and rotates the ten-year CA one year before expiry. TLS handshakes read the
latest in-memory certificate, so renewal does not require a Pod restart. During CA
rotation the old and new CA are temporarily published together before trust is
converged to the new CA. If the Secret is lost, the Controller recovers the old
trust root from the current webhook configuration before switching certificates.
These periods are fixed product contracts rather than chart values.

### Recovery and CRD upgrades

The source-tree controller checks live Pod and ReplicaSet/StatefulSet template
constraints, destination taints/PV topology, and PDB allowance before locking or
first eviction. A known-ineligible workload is left running and reevaluated;
this does not reserve scheduler capacity or guarantee eventual scheduling.
See the [preflight contract](../../docs/spec/volume-mobility.md#non-disruptive-preflight).
The chart grants read-only `get` on ReplicaSets/StatefulSets and `list` on PDBs.
Use a matching new controller build and apply the CRD including `consumerUID`;
upgrade only with no active moves. Published older binaries do not gain this behavior
from chart/RBAC changes alone.

The source-tree controller supports explicit `ShiftPVMove.spec.recovery=ResumeOwner`
on a Blocked move. It verifies and reopens the current owner; it never rolls a
committed destination back to the stale source. See the
[recovery contract and operating procedure](../../docs/spec/volume-mobility.md#explicit-owner-recovery).
Use a matching controller/helper image when testing this source tree: changing a
chart or CRD alone does not add recovery to an older published binary.

Before upgrading an existing installation, apply the target chart's CRD schemas
without deleting the existing CRDs, then upgrade the controller. For a local chart:

```sh
helm show crds ./charts/shiftpv | \
  kubectl apply --server-side --field-manager=shiftpv-crds -f -
```

Helm's `crds/` installation path does not upgrade existing CRDs. Do not disable
mobility or downgrade to a controller without recovery support while recovery is
in progress; confirm `recoveryPhase=Recovered` and `activeMove` empty first.

Inspect mobility without starting from Controller logs:

```sh
kubectl get shiftpvmoves
kubectl get shiftpvmove <move-name> -o yaml
kubectl get events -n default \
  --field-selector involvedObject.kind=ShiftPVMove,involvedObject.name=<move-name>
```

`Reason`, `message`, `lastTransitionTime`, and `lastProgressTime` distinguish an
automatically retried wait from a terminal `Blocked` move and show the next safe
operator action. Events are emitted only after a phase, reason, or recovery status
change; CR status remains the current source of truth. These timestamps do not
trigger timeout rollback. Cluster-scoped Move events are stored in the `default`
namespace by the Kubernetes Event API.

Changing `mobility.enabled` from `true` to `false` stops the mobility reconciler
but keeps the webhook Service, TLS Secret, HTTPS endpoint, and webhook
configuration. The Controller makes that configuration inert with a match
condition that is always false and changes `failurePolicy` to `Ignore`. This
avoids a Service/configuration deletion-order dependency in Helm and Argo CD.
Re-enabling mobility restores `failurePolicy=Fail` and removes the disabling
condition. The Controller refuses to update same-named certificate resources
without ShiftPV managed labels and expected owner references.

The CSI driver name, topology key, `WaitForFirstConsumer`, `Retain`, RWO
filesystem support, and disabled expansion are fixed product contracts.

Key configurable values:

| Value | Purpose |
|-------|---------|
| `controller.image.*` | Controller and CSI provisioner-facing binary image |
| `node.image.*` | Node Plugin binary image |
| `mobility.helperImage` | rsync-capable helper image; the Controller image satisfies this contract |
| `mobility.enabled`, `mobility.interval`, `mobility.webhookPort` | cordon reconciler and admission policy; the HTTPS endpoint remains available while disabled |
| `node.kubeletRootDir` | kubelet state root, normally `/var/lib/kubelet` |
| `node.nodeSelector`, `node.tolerations` | participating node selection |
| `helperPod.image`, `helperPod.timeout`, `helperPod.resources` | node-local directory helper |
| `lifecycle.uninstallMode` | uninstall owner: `helm` (default, fail fast) or `argocd` (wait and retry) |
| `storageClass.create`, `storageClass.name`, `storageClass.defaultClass` | StorageClass publication and explicit default-class opt-in |
| `controller.resources`, `node.resources`, `sidecars.*.resources` | workload resources |

The fixed `csi.shiftpv.io` name and cluster-scoped resources allow one ShiftPV
Helm release per cluster.

## Uninstall and recovery

The chart runs a fail-closed pre-delete Job and lifecycle validation webhook.
Keep the default `lifecycle.uninstallMode=helm` for Helm-owned releases. Set
`lifecycle.uninstallMode=argocd` in an Argo CD Application so a blocked
PreDelete Job remains active and retries bounded quiesce attempts until storage
dependencies are removed. A normal `helm uninstall`, Argo CD Application deletion, or direct Kubernetes
deletion of protected ShiftPV components is denied
while any ShiftPV PV, PVC using the configured StorageClass, `ShiftPVVolume`, or
non-terminal `ShiftPVMove` exists. Kubernetes API inspection errors also deny
the operation. The webhook protects the labeled CSI Deployment, DaemonSet,
Service, ServiceAccounts, RBAC, StorageClass, and CSIDriver even if an Argo CD
PreDelete hook creation race advances resource pruning. A denial leaves the
release and CSI workloads in place; inspect it with
`kubectl -n <namespace> logs job/<release>-uninstall-guard`.

The guard first creates a five-minute `quiescing` state owned by the current
`CSIDriver` UID. The Controller then rejects new CSI `CreateVolume` calls and
waits for already-running provisioning calls to drain before acknowledging that
specific attempt. Certificate reconciliation is fenced by the same state, so it
cannot recreate lifecycle validation during teardown. Only after acknowledgement
does the guard inspect dependencies, remove lifecycle validation, and change the
state to `granted`. This keeps the remaining Helm or Argo CD deletion independent
of Service and RBAC deletion order. A rapid reinstall gets a new `CSIDriver` UID,
so it cannot inherit the old state.

If inspection or teardown preparation fails, the guard cancels its attempt and
the Controller resumes provisioning and lifecycle validation reconciliation.
Argo CD mode waits five seconds and begins a fresh bounded attempt; Helm mode
returns the failure immediately.
Both `quiescing` and `granted` states expire after five minutes. The lifecycle
webhook itself is read-only: a direct or dry-run DELETE cannot mint permission,
and a direct DELETE is denied even with no storage dependency unless the guard
has completed the quiesce protocol.

For a normal decommission, stop workloads, let Moves become `Succeeded` or
`Blocked`, and explicitly remove the PVC/PV and ShiftPVVolume metadata before
uninstalling. The `Retain` policy means metadata cleanup does not automatically
remove host data.

For emergency recovery, an operator can explicitly remove lifecycle validation
and then bypass the Helm hook with:

```sh
kubectl get validatingwebhookconfiguration \
  -l app.kubernetes.io/managed-by=shiftpv-controller,app.kubernetes.io/component=lifecycle-admission
kubectl delete validatingwebhookconfiguration <name-from-above>
helm uninstall <release> --namespace <namespace> --no-hooks
```

This removes the driver workloads, RBAC, `CSIDriver`, chart-created StorageClass,
and the remaining Controller-managed mobility webhook and TLS Secret through
owner garbage collection. It does not delete CRDs, user PVCs/PVs,
Pool/Volume/Move CRs, dynamically-created reservation ConfigMaps, or data
directories. Existing Pods must not be treated as safely mounted while the node
plugin is absent.

In `argocd` mode the Job is an Argo CD 3.3+ `PreDelete` hook for whole-Application
deletion, while the lifecycle validation webhook is the authoritative deletion barrier.
Argo CD does not run `PreDelete` during ordinary sync pruning. Direct
`kubectl delete` bypasses Helm/Argo hooks but still reaches lifecycle admission
for protected resources. Manage ShiftPV as a dedicated Application and use
Application deletion as its normal removal path.

Reinstall the release into the same namespace, restore the same Pool CRs and
mount paths, wait for controller and node Pods to become ready, and then restart
the user workload. `Retain` also means deleting a PVC leaves its PV and data for
manual recovery instead of calling CSI `DeleteVolume` automatically.
