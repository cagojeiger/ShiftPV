# ADR 0008: Explicit recovery of the current owner

Status: Accepted

## Context

`Blocked` closes new CSI mounts, but removing a cordon does not reopen them. Patching
Volume status alone can leave an old copy, promotion or cleanup process running.
An owner commit may already have admitted writes on the destination, so restoring
the source merely because a Move failed is unsafe.

## Decision

An operator requests `ShiftPVMove.spec.recovery=ResumeOwner`. This is a one-way,
idempotent request on a Blocked transaction, not a new migration or an owner override.
The existing singleton controller reconciles a separately persisted recovery phase.
The normal Move phase remains Blocked as the historical migration result.

Recovery snapshots the authoritative Volume owner, requires that owner Ready and
uncordoned, and never changes it. It stops and observes termination of the old
helper Jobs/Pods without force deletion, verifies the current owner's final directory,
and retires non-authoritative artifacts to move-specific recoverable paths. A
destination owner additionally requires the promotion marker for this Move. Source
and any selected destination must remain healthy while their processes/data are
examined. API errors, foreign published nodes, binding changes, unexpected paths,
or failed recovery Jobs keep the mount guard closed.

Only then does a CAS reopen Ready on the same owner. The activeMove lock remains
until an owner consumer publishes and recovery resources are removed. Gated or
misplaced controller-owned Pods are evicted using UID preconditions and the PDB;
their workload controller creates replacements which admission pins to the owner.
ShiftPV does not scale/edit the Deployment/StatefulSet or bypass Argo CD ownership.

## Consequences

- Recovery is explicitly requested, restartable and owner-preserving, not failover.
- Unavailable nodes, unknown authority and failed verification need operator repair;
  they never authorize stale-source promotion or force unmount.
- A recovered Move is terminal and excluded from discovery's active history.
- Retired artifacts are preserved, not backups and not automatically deleted.
- CRD schema must be upgraded explicitly before this controller/chart upgrade:
  Helm does not upgrade existing CRDs in `crds/`.
- The supported controller remains a single Recreate replica. Concurrent externally
  started controllers, force-deleted helper Pods, manual status edits, and direct
  mutation/replacement of registered pool storage are outside this safety contract.

## Validation

Unit tests cover both owners, unknown/mismatched authority, binding identity,
foreign writers, unavailable nodes, helper termination, failed checks, API errors,
duplicate requests and restart boundaries. Isolated Kind additionally verified
source resumption, destination-only post-commit resumption, unchanged PVC/PV and
payload, and safe interruption. These cases passed in the
[2026-09-03 validation](../validation/blocked-owner-recovery-2026-09-03.md).
No Home cordon or release publication was performed.
