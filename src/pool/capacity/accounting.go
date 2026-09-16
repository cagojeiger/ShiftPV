package capacity

import (
	"fmt"
	"math"

	"github.com/project-jelly/ShiftPV/src/kubernetes/volumeapi"
)

// ReservedBytes counts current Volume owner capacity and active Move temporary
// holds. It intentionally over-counts retained postcommit source copies until
// the owning Move settles cleanup evidence.
func ReservedBytes(volumes map[string]volumeapi.State, moves []volumeapi.Move, nodeName string) (int64, error) {
	if nodeName == "" {
		return 0, fmt.Errorf("node name is required")
	}
	var total int64
	for volumeID, state := range volumes {
		if state.OwnerNode == "" {
			return 0, fmt.Errorf("volume %q has no current owner", volumeID)
		}
		if state.CapacityBytes <= 0 {
			return 0, fmt.Errorf("volume %q has invalid capacity", volumeID)
		}
		if state.OwnerNode != nodeName {
			continue
		}
		next, err := add(total, state.CapacityBytes)
		if err != nil {
			return 0, err
		}
		total = next
	}
	for _, move := range moves {
		if volumeapi.MoveCleanupSettled(move) {
			continue
		}
		state, exists := volumes[move.Spec.VolumeID]
		if !exists {
			if move.Status.CapacityApproved {
				return 0, fmt.Errorf("move %q has no volume state", move.Name)
			}
			continue
		}
		if state.ActiveMove != move.Name {
			if move.Status.CapacityApproved {
				return 0, fmt.Errorf("move %q is not the active Move for volume %q", move.Name, move.Spec.VolumeID)
			}
			continue
		}
		if state.CapacityBytes <= 0 {
			return 0, fmt.Errorf("move %q has invalid volume capacity", move.Name)
		}
		if move.Status.CapacityApproved && move.Status.DestinationNode == nodeName && state.OwnerNode != nodeName {
			next, err := add(total, state.CapacityBytes)
			if err != nil {
				return 0, err
			}
			total = next
		}
		if move.Status.CapacityApproved && move.Status.DestinationNode == state.OwnerNode &&
			move.Spec.SourceNode == nodeName && move.Spec.SourceNode != state.OwnerNode {
			next, err := add(total, state.CapacityBytes)
			if err != nil {
				return 0, err
			}
			total = next
		}
	}
	return total, nil
}

func add(left, right int64) (int64, error) {
	if right <= 0 {
		return 0, fmt.Errorf("capacity must be positive")
	}
	if left > math.MaxInt64-right {
		return 0, fmt.Errorf("Pool reservation total overflows int64")
	}
	return left + right, nil
}
