package volumeapi

import (
	"fmt"

	"github.com/cagojeiger/ShiftPV/src/volume"
)

// One Move owns exactly one transaction: one incoming copy, one destination
// serving copy, and one operation ID per step. Every name is derived from the
// Move UID here so the generator and every later authority recheck agree on the
// same exact strings.

// MoveCopyIDs returns the exact incoming and destination serving copy IDs the
// Move with moveUID owns.
func MoveCopyIDs(moveUID string) (incoming, serving string) {
	return "move-" + moveUID + "-incoming", "move-" + moveUID + "-serving"
}

// MoveCopyOperationID is the exact copy operation the Move authorizes.
func MoveCopyOperationID(moveUID string) string { return "copy-" + moveUID }

// MovePromotionOperationID is the exact promotion operation the Move authorizes.
func MovePromotionOperationID(moveUID string) string { return "promote-" + moveUID }

// MoveCleanupOperationID is the exact postcommit source cleanup operation the
// Move authorizes.
func MoveCleanupOperationID(moveUID string) string { return "cleanup-" + moveUID }

// MoveRollbackOperationID is the exact precommit rollback cleanup operation the
// Move authorizes.
func MoveRollbackOperationID(moveUID string) string { return "rollback-" + moveUID }

// MoveDestinationAnchor is the exact destination binding a Move's transaction
// copies are derived from and rechecked against. Every field is pinned: a
// caller that cannot independently observe one of them passes the Move's own
// incoming value, which reduces that field to an agreement check between the
// two transaction copies.
type MoveDestinationAnchor struct {
	InstallationID string
	PoolName       string
	PoolUID        string
	NodeName       string
	VolumeUID      string
}

// MoveTransactionCopies builds the exact incoming and destination serving copy
// identities the Move with moveUID must carry for volumeID under anchor. It is
// the only generator of those identities; validators compare against it rather
// than restating the shape.
func MoveTransactionCopies(moveUID, volumeID string, anchor MoveDestinationAnchor) (incoming, destination volume.CopyIdentity) {
	base := volume.CopyIdentity{
		InstallationID: anchor.InstallationID, PoolName: anchor.PoolName, PoolUID: anchor.PoolUID,
		VolumeID: volumeID, VolumeUID: anchor.VolumeUID, NodeName: anchor.NodeName,
	}
	incoming, destination = base, base
	incoming.Role, destination.Role = volume.RoleIncoming, volume.RoleServing
	incoming.CopyID, destination.CopyID = MoveCopyIDs(moveUID)
	return incoming, destination
}

// ValidateMoveTransactionIdentities rechecks that a Move still carries the exact
// transaction identities MoveTransactionCopies derives for anchor, plus the two
// operation IDs bound to the same UID. It names the failing term and reads no
// Pool, Volume, or Job state; callers own the rest of their authority checks.
func ValidateMoveTransactionIdentities(move Move, anchor MoveDestinationAnchor) error {
	if !volume.ValidIdentityToken(move.UID) {
		return MoveIdentityMismatch("UID")
	}
	incoming, destination := move.Status.IncomingCopy, move.Status.DestinationCopy
	if incoming == nil || incoming.Validate() != nil {
		return MoveIdentityMismatch("IncomingCopy")
	}
	if destination == nil || destination.Validate() != nil {
		return MoveIdentityMismatch("DestinationCopy")
	}
	expectedIncoming, expectedDestination := MoveTransactionCopies(move.UID, move.Spec.VolumeID, anchor)
	if err := matchMoveCopy("IncomingCopy", *incoming, expectedIncoming); err != nil {
		return err
	}
	if err := matchMoveCopy("DestinationCopy", *destination, expectedDestination); err != nil {
		return err
	}
	if move.Status.CopyOperationID != MoveCopyOperationID(move.UID) {
		return MoveIdentityMismatch("CopyOperationID")
	}
	if move.Status.PromotionOperationID != MovePromotionOperationID(move.UID) {
		return MoveIdentityMismatch("PromotionOperationID")
	}
	return nil
}

// MoveIdentityMismatch names the exact Move transaction term that contradicts
// the transaction the Move committed to.
func MoveIdentityMismatch(term string) error {
	return fmt.Errorf("Move %s does not match the exact transaction identity", term)
}

func matchMoveCopy(name string, observed, expected volume.CopyIdentity) error {
	for _, term := range []struct{ field, observed, expected string }{
		{"InstallationID", observed.InstallationID, expected.InstallationID},
		{"PoolName", observed.PoolName, expected.PoolName},
		{"PoolUID", observed.PoolUID, expected.PoolUID},
		{"VolumeID", observed.VolumeID, expected.VolumeID},
		{"VolumeUID", observed.VolumeUID, expected.VolumeUID},
		{"CopyID", observed.CopyID, expected.CopyID},
		{"NodeName", observed.NodeName, expected.NodeName},
		{"Role", observed.Role, expected.Role},
	} {
		if term.observed != term.expected {
			return MoveIdentityMismatch(name + "." + term.field)
		}
	}
	// Whole-value comparison keeps the check exact even if CopyIdentity gains a
	// field that the named terms above do not yet list.
	if observed != expected {
		return MoveIdentityMismatch(name)
	}
	return nil
}
