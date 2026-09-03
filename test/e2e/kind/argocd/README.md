# Isolated Argo CD uninstall E2E

This suite creates a dedicated `shiftpv-argocd-e2e` kind cluster and uses a
cluster-local Helm repository so Argo CD renders the current checkout rather
than a remote branch. It installs pinned Argo CD 3.5.2 and validates the
fail-closed hook plus authoritative lifecycle-admission contract:

1. An Application with no dependent ShiftPV storage is deleted successfully.
2. An Application with a mounted ShiftPV volume remains in deletion while the
   guard Job fails and lifecycle admission rejects protected resource deletion.
3. The Controller, Node Plugin, StorageClass, and mounted checksum remain intact
   after that denial.
4. Removing the PVC/PV/Volume blockers lets Argo CD retry, grant the bounded
   uninstall permit, remove lifecycle validation, and complete the same
   Application deletion.

Run from the repository root:

```bash
./test/e2e/kind/argocd/run.sh
```

The suite has its own cluster name, temporary kubeconfig, worker directory, and
image tag. It can therefore run alongside the base and mobility kind suites.
Set `KEEP_CLUSTER=1` only for failure diagnosis.
