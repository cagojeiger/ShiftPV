# kind UltraQA Validation — 2026-09-03

## Scope and environment

- uncommitted worktree on `codex/automatic-volume-mobility` at base commit
  `17e0ce02285b84494f86fc8a33f472fbd37337f9`
- kind `v0.33.0`, Kubernetes node `v1.35.8`
- kubectl client `v1.36.1`, Helm `v3.15.2`
- Docker Desktop `29.4.0` on `linux/arm64`
- one control-plane and two workers with distinct mounted Pool paths

## Defects found and repaired

1. Restarting the owner-node plugin while a workload was mounted left a nested kubelet mount in
   the private `/host` mount namespace. A later `NodeUnpublishVolume` unmounted the real target,
   but target-directory removal failed with `EBUSY`. The Node DaemonSet now mounts `/host` with
   `HostToContainer` propagation so host-side unmount changes reach the restarted plugin.
2. Concurrent kind E2E runs shared and changed the process-global kubectl context. This produced
   a false cross-cluster `PVC NotFound` failure. Both kind harnesses now use a per-run kubeconfig.

## Passing evidence

- `make verify`: exit 0, race coverage `82.1%`, vet/build/release resolver/shellcheck/actionlint,
  Helm lint and render checks passed.
- basic kind E2E: exit 0 after Controller and owner Node Plugin forced replacement, ordinary
  unpublish/publish, Helm uninstall/reinstall retention, existing default StorageClass
  coexistence, ENOSPC recovery and read-only DeleteVolume recovery.
- mobility kind E2E: exit 0. The source-only workload reached
  `Blocked/UnsupportedSchedulingConstraint` without changing authority. The normal move survived
  Controller replacement at `Copying`, `Promoting` and `Committing`, then reached `Succeeded`
  with the same PVC, PV, volume handle and payload checksum.
- admission outage probe: scaling the Controller to zero rejected a Pod CREATE in an opted-in
  namespace and left no Pod. Restoring the Controller admitted the same request and the Pod
  reached `Succeeded`.
- FSM graph test: every transition target is a known phase, terminal phases self-loop, and every
  non-terminal phase can reach a terminal phase.

The validation clusters were deleted. The retained diagnostic directory from the mobility run
was moved to the user's Trash after its cluster was removed.
