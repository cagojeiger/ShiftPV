package volumeapi

import (
	"time"

	"github.com/cagojeiger/ShiftPV/src/volume"
)

// PoolReadyForActiveMoveRepairAt admits only the two filesystem states that a
// bound Move can repair after a helper stops between a directory rename/create
// and its placement marker. It is intentionally narrower than ordinary Pool
// readiness and must not be used for provisioning or destination discovery.
func PoolReadyForActiveMoveRepairAt(pool Pool, move Move, state State, now time.Time, staleAfter time.Duration) bool {
	if ready, _ := pool.ReadyAt(now, staleAfter); !ready {
		return false
	}
	inventory := pool.Status.Inventory
	if inventory == nil || inventory.Valid || inventory.Truncated || inventory.Message != "CopyObservationProblem" ||
		inventory.ObservedAt.IsZero() || now.Before(inventory.ObservedAt.Time) || now.Sub(inventory.ObservedAt.Time) > staleAfter {
		return false
	}
	if move.Name == "" || !volume.ValidIdentityToken(move.UID) || move.Status.DestinationNode != pool.NodeName ||
		move.Status.DestinationPoolUID != pool.UID || state.UID == "" || state.Phase != PhaseMoving ||
		state.ActiveMove != move.Name || state.OwnerNode != move.Spec.SourceNode || state.CurrentCopy == nil ||
		move.Status.SourceCopy == nil || state.CurrentCopy.Validate() != nil || state.CurrentCopy.Role != volume.RoleServing ||
		state.CurrentCopy.VolumeID != move.Spec.VolumeID || state.CurrentCopy.VolumeUID != state.UID ||
		state.CurrentCopy.NodeName != move.Spec.SourceNode || *state.CurrentCopy != *move.Status.SourceCopy {
		return false
	}
	incoming, destination := move.Status.IncomingCopy, move.Status.DestinationCopy
	if incoming == nil || destination == nil || incoming.Validate() != nil || destination.Validate() != nil ||
		incoming.Role != volume.RoleIncoming || destination.Role != volume.RoleServing ||
		incoming.InstallationID != destination.InstallationID || incoming.PoolName != pool.Name || destination.PoolName != pool.Name ||
		incoming.PoolUID != pool.UID || destination.PoolUID != pool.UID || incoming.NodeName != pool.NodeName || destination.NodeName != pool.NodeName ||
		incoming.VolumeID != move.Spec.VolumeID || destination.VolumeID != move.Spec.VolumeID ||
		incoming.VolumeUID != state.UID || destination.VolumeUID != state.UID ||
		incoming.CopyID != "move-"+move.UID+"-incoming" || destination.CopyID != "move-"+move.UID+"-serving" ||
		move.Status.CopyOperationID != "copy-"+move.UID || move.Status.PromotionOperationID != "promote-"+move.UID {
		return false
	}
	expectedMarker := ""
	switch move.Status.Phase {
	case "Copying":
		expectedMarker = "path:.shiftpv/incoming/" + incoming.CopyID
	case "Promoting":
		expectedMarker = "path:volumes/" + destination.VolumeID
	default:
		return false
	}
	matched := 0
	for _, observed := range inventory.Copies {
		if observed.Problem == "" {
			continue
		}
		if observed.Marker != expectedMarker || observed.Identity != nil || !observed.Present || observed.Published || observed.Problem != "UnrecordedPath" {
			return false
		}
		matched++
	}
	return matched == 1
}
