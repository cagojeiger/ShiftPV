package volumeapi

import (
	"time"

	"github.com/cagojeiger/ShiftPV/src/volume"
)

// TerminalMoveInventoryPending preserves a copy until the exact Pool reports
// an inventory collected after the Move reached its terminal state.
func TerminalMoveInventoryPending(move Move, target volume.CopyIdentity, pool Pool) bool {
	if move.Status.Phase != "Succeeded" && move.Status.RecoveryPhase != "Recovered" {
		return false
	}
	referenced := false
	for _, candidate := range []*volume.CopyIdentity{move.Status.SourceCopy, move.Status.IncomingCopy, move.Status.DestinationCopy} {
		if candidate != nil && *candidate == target {
			referenced = true
			break
		}
	}
	if !referenced {
		return false
	}
	if pool.Name != target.PoolName || pool.UID != target.PoolUID || pool.NodeName != target.NodeName {
		return true
	}
	transitionedAt, err := time.Parse(time.RFC3339Nano, move.Status.LastTransitionTime)
	if err != nil || pool.Status.Inventory == nil || !pool.Status.Inventory.Valid {
		return true
	}
	return !pool.Status.Inventory.ObservedAt.Time.After(transitionedAt)
}
