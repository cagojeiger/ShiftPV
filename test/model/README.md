# ShiftPV 0.4 design preflight

이 디렉터리는 0.4 구현 전에 상태 계약과 외부 전제를 빠르게 반증하기 위한 검사다. 통과 결과는
설계의 내부 일관성과 필요한 primitive의 사용 가능성을 뜻하며, Controller 구현이나 운영 내구성을
증명하지 않는다.

## Run

```bash
make v04-model
make v04-kubernetes-primitives
make v04-filesystem-primitives
```

| 검사 | 확인하는 것 | 확인하지 않는 것 |
|---|---|---|
| `v04-model` | Move와 Volume 삭제의 안전 invariant, 재시작 후 수렴, commit 경계, 보수적 용량 hold | Kubernetes API와 실제 filesystem effect |
| `v04-kubernetes-primitives` | CR generation/status 분리, deletionTimestamp 뒤 status journal, finalizer와 owner-referenced Job 수명주기 | ShiftPV CRD와 Controller 구현 |
| `v04-filesystem-primitives` | 대상 runtime의 rsync daemon이 hardlink, symlink, FIFO, UID/GID, mode, sparse file, ACL, xattr와 checksum 검증을 지원 | 전원 차단 뒤 durability, 실제 node filesystem 조합, 성능 |

모델은 stale, invalid, incomplete observation을 재시도 가능한 증거 부족으로 취급한다. Exact identity가
충돌하거나 삭제 intent 없이 경로가 사라지면 `NeedsReview`에 해당하는 격리 상태로 남기고 destructive
transition을 허용하지 않는다.

모든 production 보증은 같은 계약을 실제 CRD, Controller, node-local lock, helper effect에 구현한 뒤
unit, Linux integration, isolated Kind fault injection, real-node unclean OS reboot와 soak gate를 통과해야 성립한다.
