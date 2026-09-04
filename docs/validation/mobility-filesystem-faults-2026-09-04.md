# Mobility destination filesystem faults — 2026-09-04

## Scope

This is the second bounded SP-STAB-005 fault-validation slice. It exercises the
destination filesystem during a real mobility transaction: ENOSPC while rsync
copies, partial staging left by that failure, and a read-only remount after a
verified copy but before promotion. It does not claim node-container restart,
PVC/Pod mutation, destination disappearance or certificate-rollover coverage.

## Safety contract

- Copy failure must leave the source owner authoritative and close the Move and
  Volume at `Blocked/CopyFailed`.
- Partial staging, including an empty or truncated marker file, must never be
  treated as a verified copy. Only a marker equal to the current Move name is
  valid for promotion.
- A read-only destination after copy completion must fail promotion before the
  owner CAS, leaving the source authoritative at `Blocked/PromotionFailed`.
- After the filesystem prerequisite is restored, explicit `ResumeOwner` must
  verify the source final directory, quarantine destination staging, preserve
  the same PVC/PV/volume handle and payload, and reopen the same source owner.

## Focused Kind result

The source-based test used Kubernetes 1.35.8 in a dedicated three-node Kind
cluster. Each worker had a different host directory and node-local mount path.
The destination path was overlaid with a bounded tmpfs so the test exercised
real kernel ENOSPC and read-only behavior rather than a fake client error.

```bash
MOBILITY_FILESYSTEM_FAULTS_ONLY=1 \
  CLUSTER_NAME=shiftpv-mobility-fs-focused \
  ./test/e2e/kind/run.sh
```

Observed result:

- rsync against a full 1 MiB tmpfs failed and left partial incoming state;
- the marker content was not a valid Move ID, and no destination final existed;
- the Move reached `Blocked/CopyFailed` while Volume owner and source payload
  remained unchanged;
- after freeing space, `ResumeOwner` reached `Recovered` and renamed incoming
  staging under `.shiftpv/aborted`;
- a second Move completed its copy Job, then the destination was remounted
  read-only;
- promotion reached `Blocked/PromotionFailed` before owner commit and created
  no destination final;
- after remounting read-write, `ResumeOwner` reached `Recovered`, quarantined
  verified staging, and preserved the same source placement and checksum;
- the focused runner printed both mobility recovery pass messages and exited 0;
- the runner deleted the cluster and its temporary working directory.

An initial harness assertion assumed a failed copy could not create the marker
path. Actual ENOSPC behavior can create a zero-length marker before `printf`
returns an I/O error. The product already validates exact marker content before
promotion; the assertion was corrected to test that contract. A later harness
race observed the durable `copyJobName` before the Job create in the same
reconcile; it was corrected to wait for both status identity and Job existence
before pausing the Controller. Neither finding required a product-code change.

## Full-suite integration result

The focused result was followed by a fresh full source E2E run using cluster
`shiftpv-fs-full-e2e`. Default StorageClass provisioning, Controller and Node
Plugin replacement, Helm uninstall refusal, emergency uninstall and retained
data reattachment, coexistence with another default StorageClass, provisioning
ENOSPC recovery and DeleteVolume read-only recovery all passed before the new
mobility cases ran. The mobility ENOSPC and read-only recovery cases both passed
in the same run. The original suite payload finished with SHA-256
`8c328652070131b5d4cdd29e7cc17f8eef6e9fbfa576425c9f71450cc2537f95`, the runner
printed `ShiftPV kind e2e passed`, exited 0 and removed the cluster.

Current branch status is established by rerunning the commands above and
`make verify`; this evidence file is a dated snapshot rather than a CI result.
