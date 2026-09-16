//go:build linux || darwin

package ownership

// characterizationGolden holds the observable behaviour recorded before
// PromoteIncoming and reclaimWithState were split into steps: the returned
// values, the exact error class and message, and the resulting on-disk state.
// Regenerate with UPDATE_CHARACTERIZATION=1 go test ./src/node/ownership/.
var characterizationGolden = map[string]string{
	"promote/authority-denied-after-receipt": `err=[unclassified] authority revoked
d 0700 .shiftpv
d 0700 .shiftpv/incoming
d 0700 .shiftpv/placements
d 0700 volumes/shiftpv-11111111111111111111111111111111
d 0755 volumes
f 0600 .shiftpv/copy-incoming-copy.json sha256:faabbc0d
f 0600 .shiftpv/copy-receipt-ede3162e146128559b02b9c32a94194a.json sha256:9f907f01
f 0600 .shiftpv/copy-serving-copy.json sha256:48e3d11f
f 0600 .shiftpv/identity.lock <empty>
f 0600 .shiftpv/lock-shiftpv-11111111111111111111111111111111 <empty>
f 0600 .shiftpv/placements/placement-incoming-copy.json sha256:73446d3b
f 0600 .shiftpv/placements/placement-serving-copy.json sha256:ef8eec3f
f 0600 .shiftpv/pool.json sha256:4590a53f
f 0600 .shiftpv/promotion-325b7c177419029161d3d0888a92880c.json sha256:e503de49
f 0600 .shiftpv/promotion-receipt-325b7c177419029161d3d0888a92880c.json sha256:639d88ac
f 0600 volumes/shiftpv-11111111111111111111111111111111/payload "preserve"`,
	"promote/authority-denied-before-open": `err=[unclassified] authority revoked
`,
	"promote/authority-denied-before-receipt": `err=[unclassified] authority revoked
d 0700 .shiftpv
d 0700 .shiftpv/incoming
d 0700 .shiftpv/placements
d 0700 volumes/shiftpv-11111111111111111111111111111111
d 0755 volumes
f 0600 .shiftpv/copy-incoming-copy.json sha256:faabbc0d
f 0600 .shiftpv/copy-receipt-ede3162e146128559b02b9c32a94194a.json sha256:9f907f01
f 0600 .shiftpv/copy-serving-copy.json sha256:48e3d11f
f 0600 .shiftpv/identity.lock <empty>
f 0600 .shiftpv/lock-shiftpv-11111111111111111111111111111111 <empty>
f 0600 .shiftpv/placements/placement-incoming-copy.json sha256:73446d3b
f 0600 .shiftpv/placements/placement-serving-copy.json sha256:ef8eec3f
f 0600 .shiftpv/pool.json sha256:4590a53f
f 0600 .shiftpv/promotion-325b7c177419029161d3d0888a92880c.json sha256:e503de49
f 0600 volumes/shiftpv-11111111111111111111111111111111/payload "preserve"`,
	"promote/invalid-incoming-identity": `err=[ErrIdentity] node storage identity mismatch
`,
	"promote/invalid-operation-id": `err=[ErrIdentity] node storage identity mismatch
`,
	"promote/mismatched-volume-uid": `err=[ErrIdentity] node storage identity mismatch
`,
	"promote/missing-incoming-copy-marker": `err=[fs.ErrNotExist] no such file or directory
d 0700 .shiftpv
d 0700 .shiftpv/incoming
d 0700 .shiftpv/placements
d 0755 volumes
d 0755 volumes/shiftpv-11111111111111111111111111111111
f 0600 .shiftpv/copy-serving-copy.json sha256:48e3d11f
f 0600 .shiftpv/identity.lock <empty>
f 0600 .shiftpv/lock-shiftpv-11111111111111111111111111111111 <empty>
f 0600 .shiftpv/placements/placement-serving-copy.json sha256:ef8eec3f
f 0600 .shiftpv/pool.json sha256:4590a53f`,
	"promote/missing-pool": `err=[fs.ErrNotExist] no such file or directory
`,
	"promote/nil-authority": `err=[ErrIdentity] node storage identity mismatch
`,
	"promote/receipt-identity-mismatch": `err=[ErrIdentity] node storage identity mismatch
d 0700 .shiftpv
d 0700 .shiftpv/incoming
d 0700 .shiftpv/placements
d 0700 volumes/shiftpv-11111111111111111111111111111111
d 0755 volumes
f 0600 .shiftpv/copy-receipt-ede3162e146128559b02b9c32a94194a.json sha256:9f907f01
f 0600 .shiftpv/copy-serving-copy.json sha256:48e3d11f
f 0600 .shiftpv/identity.lock <empty>
f 0600 .shiftpv/lock-shiftpv-11111111111111111111111111111111 <empty>
f 0600 .shiftpv/placements/placement-serving-copy.json sha256:ef8eec3f
f 0600 .shiftpv/pool.json sha256:4590a53f
f 0600 .shiftpv/promotion-325b7c177419029161d3d0888a92880c.json sha256:e503de49
f 0600 .shiftpv/promotion-receipt-325b7c177419029161d3d0888a92880c.json sha256:639d88ac
f 0600 volumes/shiftpv-11111111111111111111111111111111/payload "preserve"`,
	"promote/replay-after-success": `first=<nil> err=<nil>
d 0700 .shiftpv
d 0700 .shiftpv/incoming
d 0700 .shiftpv/placements
d 0700 volumes/shiftpv-11111111111111111111111111111111
d 0755 volumes
f 0600 .shiftpv/copy-receipt-ede3162e146128559b02b9c32a94194a.json sha256:9f907f01
f 0600 .shiftpv/copy-serving-copy.json sha256:48e3d11f
f 0600 .shiftpv/identity.lock <empty>
f 0600 .shiftpv/lock-shiftpv-11111111111111111111111111111111 <empty>
f 0600 .shiftpv/placements/placement-serving-copy.json sha256:ef8eec3f
f 0600 .shiftpv/pool.json sha256:4590a53f
f 0600 .shiftpv/promotion-325b7c177419029161d3d0888a92880c.json sha256:e503de49
f 0600 .shiftpv/promotion-receipt-325b7c177419029161d3d0888a92880c.json sha256:639d88ac
f 0600 volumes/shiftpv-11111111111111111111111111111111/payload "preserve"`,
	"promote/resume-after-rename": `err=<nil>
d 0700 .shiftpv
d 0700 .shiftpv/incoming
d 0700 .shiftpv/placements
d 0700 volumes/shiftpv-11111111111111111111111111111111
d 0755 volumes
f 0600 .shiftpv/copy-receipt-ede3162e146128559b02b9c32a94194a.json sha256:9f907f01
f 0600 .shiftpv/copy-serving-copy.json sha256:48e3d11f
f 0600 .shiftpv/identity.lock <empty>
f 0600 .shiftpv/lock-shiftpv-11111111111111111111111111111111 <empty>
f 0600 .shiftpv/placements/placement-serving-copy.json sha256:ef8eec3f
f 0600 .shiftpv/pool.json sha256:4590a53f
f 0600 .shiftpv/promotion-325b7c177419029161d3d0888a92880c.json sha256:e503de49
f 0600 .shiftpv/promotion-receipt-325b7c177419029161d3d0888a92880c.json sha256:639d88ac
f 0600 volumes/shiftpv-11111111111111111111111111111111/payload "preserve"`,
	"promote/same-copy-id": `err=[ErrIdentity] node storage identity mismatch
`,
	"promote/success": `err=<nil>
d 0700 .shiftpv
d 0700 .shiftpv/incoming
d 0700 .shiftpv/placements
d 0700 volumes/shiftpv-11111111111111111111111111111111
d 0755 volumes
f 0600 .shiftpv/copy-receipt-ede3162e146128559b02b9c32a94194a.json sha256:9f907f01
f 0600 .shiftpv/copy-serving-copy.json sha256:48e3d11f
f 0600 .shiftpv/identity.lock <empty>
f 0600 .shiftpv/lock-shiftpv-11111111111111111111111111111111 <empty>
f 0600 .shiftpv/placements/placement-serving-copy.json sha256:ef8eec3f
f 0600 .shiftpv/pool.json sha256:4590a53f
f 0600 .shiftpv/promotion-325b7c177419029161d3d0888a92880c.json sha256:e503de49
f 0600 .shiftpv/promotion-receipt-325b7c177419029161d3d0888a92880c.json sha256:639d88ac
f 0600 volumes/shiftpv-11111111111111111111111111111111/payload "preserve"`,
	"promote/target-already-occupied": `err=[ErrIdentity] node storage identity mismatch
d 0700 .shiftpv
d 0700 .shiftpv/incoming
d 0700 .shiftpv/incoming/incoming-copy
d 0700 .shiftpv/placements
d 0755 volumes
d 0755 volumes/shiftpv-11111111111111111111111111111111
f 0600 .shiftpv/copy-incoming-copy.json sha256:faabbc0d
f 0600 .shiftpv/copy-receipt-ede3162e146128559b02b9c32a94194a.json sha256:9f907f01
f 0600 .shiftpv/copy-serving-copy.json sha256:48e3d11f
f 0600 .shiftpv/identity.lock <empty>
f 0600 .shiftpv/incoming/incoming-copy/payload "preserve"
f 0600 .shiftpv/lock-shiftpv-11111111111111111111111111111111 <empty>
f 0600 .shiftpv/placements/placement-incoming-copy.json sha256:73446d3b
f 0600 .shiftpv/placements/placement-serving-copy.json sha256:ef8eec3f
f 0600 .shiftpv/pool.json sha256:4590a53f
f 0600 .shiftpv/promotion-325b7c177419029161d3d0888a92880c.json sha256:e503de49`,
	"promote/wrong-serving-role": `err=[ErrIdentity] node storage identity mismatch
`,
	"reclaim/authority-denied-before-effect": `receipt={operationID= copyID= role= device=unset inode=unset retired=false purged=false} digest=unset err=[unclassified] recheck cleanup authority before filesystem effect: authority revoked
d 0700 .shiftpv
d 0700 .shiftpv/incoming
d 0700 .shiftpv/placements
d 0755 volumes
d 0755 volumes/shiftpv-11111111111111111111111111111111
f 0600 .shiftpv/cleanup-ce1320a1b6ff2e4231a1f795bbbcea7b.json sha256:da5a5423
f 0600 .shiftpv/copy-copy-id.json sha256:b91ddfa2
f 0600 .shiftpv/identity.lock <empty>
f 0600 .shiftpv/lock-shiftpv-11111111111111111111111111111111 <empty>
f 0600 .shiftpv/placements/placement-copy-id.json sha256:0ad382b7
f 0600 .shiftpv/pool.json sha256:4590a53f
f 0600 volumes/shiftpv-11111111111111111111111111111111/data "preserve"`,
	"reclaim/authority-denied-first-check": `receipt={operationID= copyID= role= device=unset inode=unset retired=false purged=false} digest=unset err=[unclassified] authority revoked
d 0700 .shiftpv
d 0700 .shiftpv/incoming
d 0700 .shiftpv/placements
d 0755 volumes
d 0755 volumes/shiftpv-11111111111111111111111111111111
f 0600 .shiftpv/copy-copy-id.json sha256:b91ddfa2
f 0600 .shiftpv/identity.lock <empty>
f 0600 .shiftpv/lock-shiftpv-11111111111111111111111111111111 <empty>
f 0600 .shiftpv/placements/placement-copy-id.json sha256:0ad382b7
f 0600 .shiftpv/pool.json sha256:4590a53f`,
	"reclaim/cancelled-context": `receipt={operationID= copyID= role= device=unset inode=unset retired=false purged=false} digest=unset err=[context.Canceled] context canceled
d 0700 .shiftpv
d 0700 .shiftpv/incoming
d 0700 .shiftpv/placements
d 0755 volumes
d 0755 volumes/shiftpv-11111111111111111111111111111111
f 0600 .shiftpv/cleanup-ce1320a1b6ff2e4231a1f795bbbcea7b.json sha256:da5a5423
f 0600 .shiftpv/copy-copy-id.json sha256:b91ddfa2
f 0600 .shiftpv/identity.lock <empty>
f 0600 .shiftpv/lock-shiftpv-11111111111111111111111111111111 <empty>
f 0600 .shiftpv/placements/placement-copy-id.json sha256:0ad382b7
f 0600 .shiftpv/pool.json sha256:4590a53f`,
	"reclaim/incoming-success": `receipt={operationID=operation-a copyID=incoming-copy role=Incoming device=set inode=set retired=true purged=true} digest=set err=<nil>
d 0700 .shiftpv
d 0700 .shiftpv/incoming
d 0700 .shiftpv/placements
d 0700 .shiftpv/retired
f 0600 .shiftpv/cleanup-ce1320a1b6ff2e4231a1f795bbbcea7b.json sha256:e7ff01e7
f 0600 .shiftpv/copy-receipt-ede3162e146128559b02b9c32a94194a.json sha256:9f907f01
f 0600 .shiftpv/identity.lock <empty>
f 0600 .shiftpv/lock-shiftpv-11111111111111111111111111111111 <empty>
f 0600 .shiftpv/pool.json sha256:4590a53f
f 0600 .shiftpv/receipt-ce1320a1b6ff2e4231a1f795bbbcea7b.json sha256:55f21d49`,
	"reclaim/intent-placement-mismatch": `receipt={operationID= copyID= role= device=unset inode=unset retired=false purged=false} digest=unset err=[ErrIdentity] inspect local cleanup effect: node storage identity mismatch
d 0700 .shiftpv
d 0700 .shiftpv/incoming
d 0700 .shiftpv/placements
d 0755 volumes
d 0755 volumes/shiftpv-11111111111111111111111111111111
f 0600 .shiftpv/cleanup-ce1320a1b6ff2e4231a1f795bbbcea7b.json sha256:da5a5423
f 0600 .shiftpv/copy-copy-id.json sha256:b91ddfa2
f 0600 .shiftpv/identity.lock <empty>
f 0600 .shiftpv/lock-shiftpv-11111111111111111111111111111111 <empty>
f 0600 .shiftpv/placements/placement-copy-id.json sha256:0ad382b7
f 0600 .shiftpv/pool.json sha256:4590a53f`,
	"reclaim/invalid-operation-id": `receipt={operationID= copyID= role= device=unset inode=unset retired=false purged=false} digest=unset err=[ErrIdentity] node storage identity mismatch
`,
	"reclaim/invalid-target": `receipt={operationID= copyID= role= device=unset inode=unset retired=false purged=false} digest=unset err=[ErrIdentity] node storage identity mismatch
`,
	"reclaim/missing-copy-marker": `receipt={operationID= copyID= role= device=unset inode=unset retired=false purged=false} digest=unset err=[fs.ErrNotExist] verify copy identity: no such file or directory
d 0700 .shiftpv
d 0700 .shiftpv/incoming
d 0700 .shiftpv/placements
d 0755 volumes
d 0755 volumes/shiftpv-11111111111111111111111111111111
f 0600 .shiftpv/identity.lock <empty>
f 0600 .shiftpv/lock-shiftpv-11111111111111111111111111111111 <empty>
f 0600 .shiftpv/placements/placement-copy-id.json sha256:0ad382b7
f 0600 .shiftpv/pool.json sha256:4590a53f`,
	"reclaim/missing-placement": `receipt={operationID= copyID= role= device=unset inode=unset retired=false purged=false} digest=unset err=[ErrIdentity,fs.ErrNotExist] verify copy placement: no such file or directory; node storage identity mismatch
d 0700 .shiftpv
d 0700 .shiftpv/incoming
d 0700 .shiftpv/placements
d 0755 volumes
d 0755 volumes/shiftpv-11111111111111111111111111111111
f 0600 .shiftpv/copy-copy-id.json sha256:b91ddfa2
f 0600 .shiftpv/identity.lock <empty>
f 0600 .shiftpv/lock-shiftpv-11111111111111111111111111111111 <empty>
f 0600 .shiftpv/pool.json sha256:4590a53f`,
	"reclaim/missing-pool": `receipt={operationID= copyID= role= device=unset inode=unset retired=false purged=false} digest=unset err=[fs.ErrNotExist] no such file or directory
`,
	"reclaim/nil-authority": `receipt={operationID= copyID= role= device=unset inode=unset retired=false purged=false} digest=unset err=[ErrIdentity] node storage identity mismatch
`,
	"reclaim/preflight-error": `receipt={operationID= copyID= role= device=unset inode=unset retired=false purged=false} digest=unset err=[unix.ENOSYS] verify safe purge support: function not implemented
d 0700 .shiftpv
d 0700 .shiftpv/incoming
d 0700 .shiftpv/placements
d 0755 volumes
d 0755 volumes/shiftpv-11111111111111111111111111111111
f 0600 .shiftpv/cleanup-ce1320a1b6ff2e4231a1f795bbbcea7b.json sha256:da5a5423
f 0600 .shiftpv/copy-copy-id.json sha256:b91ddfa2
f 0600 .shiftpv/identity.lock <empty>
f 0600 .shiftpv/lock-shiftpv-11111111111111111111111111111111 <empty>
f 0600 .shiftpv/placements/placement-copy-id.json sha256:0ad382b7
f 0600 .shiftpv/pool.json sha256:4590a53f
f 0600 volumes/shiftpv-11111111111111111111111111111111/data "preserve"`,
	"reclaim/purge-error": `receipt={operationID= copyID= role= device=unset inode=unset retired=false purged=false} digest=unset err=[unclassified] purge retired target: purge interrupted
d 0700 .shiftpv
d 0700 .shiftpv/incoming
d 0700 .shiftpv/placements
d 0700 .shiftpv/retired
d 0755 .shiftpv/retired/copy-id
d 0755 volumes
f 0600 .shiftpv/cleanup-ce1320a1b6ff2e4231a1f795bbbcea7b.json sha256:da5a5423
f 0600 .shiftpv/copy-copy-id.json sha256:b91ddfa2
f 0600 .shiftpv/identity.lock <empty>
f 0600 .shiftpv/lock-shiftpv-11111111111111111111111111111111 <empty>
f 0600 .shiftpv/placements/placement-copy-id.json sha256:0ad382b7
f 0600 .shiftpv/pool.json sha256:4590a53f
f 0600 .shiftpv/retired/copy-id/data "remove"`,
	"reclaim/receipt-target-mismatch": `receipt={operationID= copyID= role= device=unset inode=unset retired=false purged=false} digest=unset err=[ErrIdentity] node storage identity mismatch
d 0700 .shiftpv
d 0700 .shiftpv/incoming
d 0700 .shiftpv/placements
d 0700 .shiftpv/retired
d 0755 volumes
f 0600 .shiftpv/cleanup-ce1320a1b6ff2e4231a1f795bbbcea7b.json sha256:da5a5423
f 0600 .shiftpv/identity.lock <empty>
f 0600 .shiftpv/lock-shiftpv-11111111111111111111111111111111 <empty>
f 0600 .shiftpv/pool.json sha256:4590a53f
f 0600 .shiftpv/receipt-ce1320a1b6ff2e4231a1f795bbbcea7b.json sha256:1a56c201`,
	"reclaim/replay-after-receipt": `stable=true receipt={operationID=operation-a copyID=copy-id role=Serving device=set inode=set retired=true purged=true} digest=set err=<nil>
d 0700 .shiftpv
d 0700 .shiftpv/incoming
d 0700 .shiftpv/placements
d 0700 .shiftpv/retired
d 0755 volumes
f 0600 .shiftpv/cleanup-ce1320a1b6ff2e4231a1f795bbbcea7b.json sha256:da5a5423
f 0600 .shiftpv/identity.lock <empty>
f 0600 .shiftpv/lock-shiftpv-11111111111111111111111111111111 <empty>
f 0600 .shiftpv/pool.json sha256:4590a53f
f 0600 .shiftpv/receipt-ce1320a1b6ff2e4231a1f795bbbcea7b.json sha256:1a56c201`,
	"reclaim/resume-after-retire": `interrupted=[unclassified] purge retired target: purge interrupted effectStarted=[true true] receipt={operationID=operation-a copyID=copy-id role=Serving device=set inode=set retired=true purged=true} digest=set err=<nil>
d 0700 .shiftpv
d 0700 .shiftpv/incoming
d 0700 .shiftpv/placements
d 0700 .shiftpv/retired
d 0755 volumes
f 0600 .shiftpv/cleanup-ce1320a1b6ff2e4231a1f795bbbcea7b.json sha256:da5a5423
f 0600 .shiftpv/identity.lock <empty>
f 0600 .shiftpv/lock-shiftpv-11111111111111111111111111111111 <empty>
f 0600 .shiftpv/pool.json sha256:4590a53f
f 0600 .shiftpv/receipt-ce1320a1b6ff2e4231a1f795bbbcea7b.json sha256:1a56c201`,
	"reclaim/retired-success": `receipt={operationID=operation-a copyID=copy-id role=Retired device=set inode=set retired=true purged=true} digest=set err=<nil>
d 0700 .shiftpv
d 0700 .shiftpv/placements
d 0700 .shiftpv/retired
f 0600 .shiftpv/cleanup-ce1320a1b6ff2e4231a1f795bbbcea7b.json sha256:db5fd977
f 0600 .shiftpv/identity.lock <empty>
f 0600 .shiftpv/lock-shiftpv-11111111111111111111111111111111 <empty>
f 0600 .shiftpv/pool.json sha256:4590a53f
f 0600 .shiftpv/receipt-ce1320a1b6ff2e4231a1f795bbbcea7b.json sha256:db8aa7a9`,
	"reclaim/serving-success": `receipt={operationID=operation-a copyID=copy-id role=Serving device=set inode=set retired=true purged=true} digest=set err=<nil>
d 0700 .shiftpv
d 0700 .shiftpv/incoming
d 0700 .shiftpv/placements
d 0700 .shiftpv/retired
d 0755 volumes
f 0600 .shiftpv/cleanup-ce1320a1b6ff2e4231a1f795bbbcea7b.json sha256:da5a5423
f 0600 .shiftpv/identity.lock <empty>
f 0600 .shiftpv/lock-shiftpv-11111111111111111111111111111111 <empty>
f 0600 .shiftpv/pool.json sha256:4590a53f
f 0600 .shiftpv/receipt-ce1320a1b6ff2e4231a1f795bbbcea7b.json sha256:1a56c201`,
}
