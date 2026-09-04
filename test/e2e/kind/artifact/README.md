# Public artifact smoke test

This isolated Kind test installs the published chart package and released images without
building product code from the checkout. `versions.env` pins the chart package SHA-256,
controller/node multi-platform manifest digests and Kind node image digest. Update the lock
only after those immutable public artifacts are available.

Run from the repository root:

```bash
./test/e2e/kind/artifact/run.sh
```

The test downloads the requested chart through `helm repo add`, rejects a package hash
mismatch, installs the pinned images, registers the two mounted test pools and verifies a
real PVC/Pod mount, CSI identity, volume owner and data checksum. It deliberately remains a
small publication smoke test; source-level fault, recovery and mobility behavior belongs to
the other Kind suites.

GitHub runs this through the separate `Public Artifact Smoke` workflow on demand and daily.
It is intentionally not a pull-request merge gate because public availability can fail without
any relationship to the proposed source change. Lock validation remains part of `make verify`.

The cluster and temporary host data are removed on exit. Use `KEEP_CLUSTER=1` only for local
failure diagnosis. `ARTIFACT_LOCK_FILE` may select another validated lock for a candidate
artifact, but mutable image tags and malformed/unpinned values are rejected.
