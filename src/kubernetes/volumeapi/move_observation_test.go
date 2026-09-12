package volumeapi

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cagojeiger/ShiftPV/src/volume"
)

func TestTerminalMoveInventoryPending(t *testing.T) {
	target := volume.CopyIdentity{
		InstallationID: "installation", PoolName: "pool", PoolUID: "pool-uid", VolumeID: "shiftpv-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		VolumeUID: "volume-uid", CopyID: "incoming", NodeName: "node", Role: volume.RoleIncoming,
	}
	transitionedAt := time.Unix(10, 0).UTC()
	move := Move{Status: MoveStatus{
		Phase: "Succeeded", LastTransitionTime: transitionedAt.Format(time.RFC3339Nano), IncomingCopy: &target,
	}}
	pool := Pool{
		Name: target.PoolName, UID: target.PoolUID, NodeName: target.NodeName,
		Status: PoolStatus{Inventory: &PoolInventory{Valid: true, ObservedAt: metav1.NewTime(transitionedAt.Add(time.Second))}},
	}

	if TerminalMoveInventoryPending(move, target, pool) {
		t.Fatal("fresh post-terminal inventory remained fenced")
	}
	pool.Status.Inventory.ObservedAt = metav1.NewTime(transitionedAt)
	if !TerminalMoveInventoryPending(move, target, pool) {
		t.Fatal("equal-time inventory crossed the terminal Move fence")
	}
	move.Status.Phase, move.Status.RecoveryPhase = "Blocked", "Recovered"
	if !TerminalMoveInventoryPending(move, target, pool) {
		t.Fatal("recovered Move did not retain the observation fence")
	}
	move.Status.LastTransitionTime = "invalid"
	if !TerminalMoveInventoryPending(move, target, pool) {
		t.Fatal("invalid terminal timestamp did not fail closed")
	}
	move.Status.LastTransitionTime = transitionedAt.Format(time.RFC3339Nano)
	pool.Status.Inventory.Valid = false
	if !TerminalMoveInventoryPending(move, target, pool) {
		t.Fatal("invalid inventory did not fail closed")
	}
	pool.Status.Inventory.Valid = true
	pool.UID = "replacement-pool"
	if !TerminalMoveInventoryPending(move, target, pool) {
		t.Fatal("replacement Pool identity did not fail closed")
	}
	pool.UID = target.PoolUID
	unrelated := target
	unrelated.CopyID = "other-copy"
	if TerminalMoveInventoryPending(move, unrelated, pool) {
		t.Fatal("terminal Move fenced an unrelated copy")
	}
	move.Status.Phase, move.Status.RecoveryPhase = "Copying", ""
	if TerminalMoveInventoryPending(move, target, pool) {
		t.Fatal("non-terminal Move used the terminal observation fence")
	}
}
