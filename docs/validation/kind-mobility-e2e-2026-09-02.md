# kind Mobility E2E Validation — 2026-09-02

## Environment

- kind `v0.33.0`
- Kubernetes node `v1.35.8`
- node image digest `sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0`
- one control-plane and two workers with distinct temporary host pool directories and
  different in-node mount paths (`/mnt/shiftpv`, `/srv/shiftpv-b`)
- kubectl client `v1.36.1`
- Helm `v3.15.2`
- Docker Desktop engine `29.4.0` on linux/arm64

## Command

```bash
./test/e2e/kind/mobility/run.sh
```

## Proven behavior

1. The product Helm chart installed the CRDs, admission webhook, reconciler, CSI Controller and
   two Node Plugins.
2. A source-only hostname selector reached
   `Blocked/UnsupportedSchedulingConstraint`; the source remained owner, the source payload
   remained present, and no destination final directory was promoted.
3. A separate WFFC workload automatically created a Move after its healthy owner was cordoned.
4. The Controller Pod was force-deleted during `Copying`, during `Promoting`, and in the
   exact `Committing` crash window after owner CAS changed the Volume owner but before the
   Move phase advanced. Every replacement Controller Pod continued the same Move transaction.
5. The Move reached `Succeeded`, the replacement workload ran on the other worker, and PVC UID,
   PV, CSI volume handle and payload SHA-256 remained unchanged.
6. The destination final payload existed and the source payload was moved to the recoverable
   retired path.

The final automated run reported:

```text
ShiftPV blocked mobility E2E passed:
volume=shiftpv-0a6611fc07121e464e5cde40081edf49
move=move-0a6611fc07121e464e5cde40081edf49-bfppj
reason=UnsupportedSchedulingConstraint

controller restart injected at Copying:
0458c922-0ed6-4564-b881-f8786da0f7fb -> c2e6f814-779d-4d7d-8d7a-b3abc99a9b2e
controller restart injected at Promoting:
c2e6f814-779d-4d7d-8d7a-b3abc99a9b2e -> 02f7b93e-c811-4b5c-9711-acb3769b20d5
controller restart injected at Committing:
02f7b93e-c811-4b5c-9711-acb3769b20d5 -> 4cd4dc12-a76c-4f93-bd9d-20c6a7c942ef

ShiftPV closed-loop mobility E2E passed
volume=shiftpv-e6766d2e4fed65301a4d8278e99daa37
pv=pvc-653ebf2b-d481-4c33-9326-b73bd0b6c93c
move=move-e6766d2e4fed65301a4d8278e99daa37-7gfpf
source=shiftpv-mobility-final5-worker2
destination=shiftpv-mobility-final5-worker
checksum=bab0b221a3ebcc85f45d270c9fa83c26280f28fd07941f194f1ac3ca452ce655
```

The script deleted the kind cluster and temporary pool directories after validation.
