# Argo CD uninstall guard kind validation — 2026-09-03

## Environment

- kind cluster: `shiftpv-argocd-e2e`
- Kubernetes: `v1.35.8`
- Argo CD: `v3.5.2`, upstream install manifest SHA-256 verified
- chart source: cluster-local Helm repository built from the current checkout
- isolation: dedicated kubeconfig, worker host directory and
  `shiftpv-argocd-e2e:dev` image tag

## Command

```bash
./test/e2e/kind/argocd/run.sh
```

## Observed result

1. Argo CD synced the ShiftPV Helm chart and reported the Application Healthy.
2. With no PV/PVC/Volume/Move dependency, the `PreDelete` Job created a bounded
   quiesce attempt. The Controller acknowledged it after provisioning drained,
   then the Job removed lifecycle validation, granted the teardown, and the
   Application, StorageClass and chart resources were deleted.
3. After reinstalling, a real RWO PVC was provisioned and mounted on the worker.
4. Deleting the Application created a long-running `shiftpv-uninstall-guard` Job.
   Even when concurrent Argo CD reconciliation advanced resource pruning,
   lifecycle admission rejected protected deletes. The Application remained
   pending; Controller, Node Plugin, StorageClass, and lifecycle validation
   remained present. Failed attempts did not leave a `granted` uninstall state;
   the same Job created fresh bounded `quiescing` attempts while waiting.
5. The mounted payload SHA-256 before and after denial was identical:
   `81db0b52d390ac140bd43d6d2c77a40d293f37e0cc3cdc36f75ee76ed9c52394`.
6. After deleting the Pod, PVC, retained PV and `ShiftPVVolume`, the running Job
   completed the same pending deletion and removed the Application and chart resources.
7. The `Retain` host payload remained after metadata and Application deletion.

The final suite completed with `ShiftPV Argo CD uninstall guard E2E passed`.
Its kind cluster was removed and its retained diagnostic directory was moved to
the user's Trash after verification.
