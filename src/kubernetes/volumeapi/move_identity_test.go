package volumeapi

import (
	"testing"

	"github.com/cagojeiger/ShiftPV/src/volume"
)

func TestMoveTransactionNamesAreDerivedFromTheMoveUID(t *testing.T) {
	incoming, serving := MoveCopyIDs("move-uid")
	if incoming != "move-move-uid-incoming" || serving != "move-move-uid-serving" {
		t.Fatalf("copy IDs = %q, %q", incoming, serving)
	}
	for name, got := range map[string]string{
		"copy-move-uid":     MoveCopyOperationID("move-uid"),
		"promote-move-uid":  MovePromotionOperationID("move-uid"),
		"cleanup-move-uid":  MoveCleanupOperationID("move-uid"),
		"rollback-move-uid": MoveRollbackOperationID("move-uid"),
	} {
		if got != name {
			t.Fatalf("operation ID = %q, want %q", got, name)
		}
	}
}

func moveTransactionFixture() (Move, MoveDestinationAnchor) {
	volumeID := "shiftpv-0123456789abcdef0123456789abcdef"
	anchor := MoveDestinationAnchor{
		InstallationID: "installation", PoolName: "destination-pool", PoolUID: "destination-pool-uid",
		NodeName: "destination", VolumeUID: "volume-uid",
	}
	incoming, destination := MoveTransactionCopies("move-uid", volumeID, anchor)
	move := Move{
		Name: "move-test", UID: "move-uid", Spec: MoveSpec{VolumeID: volumeID, SourceNode: "source"},
		Status: MoveStatus{
			DestinationNode: anchor.NodeName, DestinationPoolUID: anchor.PoolUID,
			IncomingCopy: &incoming, DestinationCopy: &destination,
			CopyOperationID: MoveCopyOperationID("move-uid"), PromotionOperationID: MovePromotionOperationID("move-uid"),
		},
	}
	return move, anchor
}

func TestMoveTransactionCopiesGeneratesTheExactAdmittedIdentities(t *testing.T) {
	move, anchor := moveTransactionFixture()
	incoming, destination := *move.Status.IncomingCopy, *move.Status.DestinationCopy
	if incoming.Validate() != nil || destination.Validate() != nil {
		t.Fatalf("generated identities are invalid: %#v %#v", incoming, destination)
	}
	if incoming.Role != volume.RoleIncoming || destination.Role != volume.RoleServing {
		t.Fatalf("roles = %q, %q", incoming.Role, destination.Role)
	}
	wantIncoming, wantServing := MoveCopyIDs(move.UID)
	if incoming.CopyID != wantIncoming || destination.CopyID != wantServing {
		t.Fatalf("copy IDs = %q, %q", incoming.CopyID, destination.CopyID)
	}
	if incoming.PoolName != anchor.PoolName || incoming.PoolUID != anchor.PoolUID || incoming.NodeName != anchor.NodeName ||
		incoming.InstallationID != anchor.InstallationID || incoming.VolumeUID != anchor.VolumeUID || incoming.VolumeID != move.Spec.VolumeID {
		t.Fatalf("incoming copy is not bound to the anchor: %#v", incoming)
	}
	// Only the copy ID and the role may differ between the two copies.
	incoming.CopyID, incoming.Role = destination.CopyID, destination.Role
	if incoming != destination {
		t.Fatalf("transaction copies differ beyond copy ID and role: %#v %#v", incoming, destination)
	}
	if err := ValidateMoveTransactionIdentities(move, anchor); err != nil {
		t.Fatalf("generated identities were rejected: %v", err)
	}
}

func TestValidateMoveTransactionIdentitiesNamesTheFailingTerm(t *testing.T) {
	copyTerms := map[string]func(*volume.CopyIdentity){
		"InstallationID": func(c *volume.CopyIdentity) { c.InstallationID = "other-installation" },
		"PoolName":       func(c *volume.CopyIdentity) { c.PoolName = "other-pool" },
		"PoolUID":        func(c *volume.CopyIdentity) { c.PoolUID = "other-pool-uid" },
		"VolumeID":       func(c *volume.CopyIdentity) { c.VolumeID = "shiftpv-ffffffffffffffffffffffffffffffff" },
		"VolumeUID":      func(c *volume.CopyIdentity) { c.VolumeUID = "other-volume-uid" },
		"CopyID":         func(c *volume.CopyIdentity) { c.CopyID = "move-other-uid-incoming" },
		"NodeName":       func(c *volume.CopyIdentity) { c.NodeName = "other-node" },
	}
	for term, mutate := range copyTerms {
		t.Run("IncomingCopy."+term, func(t *testing.T) {
			move, anchor := moveTransactionFixture()
			changed := *move.Status.IncomingCopy
			mutate(&changed)
			move.Status.IncomingCopy = &changed
			requireMoveIdentityTerm(t, ValidateMoveTransactionIdentities(move, anchor), "IncomingCopy."+term)
		})
		t.Run("DestinationCopy."+term, func(t *testing.T) {
			move, anchor := moveTransactionFixture()
			changed := *move.Status.DestinationCopy
			mutate(&changed)
			move.Status.DestinationCopy = &changed
			requireMoveIdentityTerm(t, ValidateMoveTransactionIdentities(move, anchor), "DestinationCopy."+term)
		})
	}

	for term, mutate := range map[string]func(*Move){
		"UID":             func(m *Move) { m.UID = "move/uid" },
		"IncomingCopy":    func(m *Move) { m.Status.IncomingCopy = nil },
		"DestinationCopy": func(m *Move) { m.Status.DestinationCopy = nil },
		"IncomingCopy.Role": func(m *Move) {
			changed := *m.Status.IncomingCopy
			changed.Role = volume.RoleServing
			m.Status.IncomingCopy = &changed
		},
		"DestinationCopy.Role": func(m *Move) {
			changed := *m.Status.DestinationCopy
			changed.Role = volume.RoleRetired
			m.Status.DestinationCopy = &changed
		},
		"CopyOperationID":      func(m *Move) { m.Status.CopyOperationID = "copy-other" },
		"PromotionOperationID": func(m *Move) { m.Status.PromotionOperationID = "" },
	} {
		t.Run(term, func(t *testing.T) {
			move, anchor := moveTransactionFixture()
			mutate(&move)
			requireMoveIdentityTerm(t, ValidateMoveTransactionIdentities(move, anchor), term)
		})
	}

	// A Move UID change renames both copies and both operation IDs at once, so
	// the recheck must fail on the first renamed term rather than accept the set.
	move, anchor := moveTransactionFixture()
	move.UID = "replacement-move-uid"
	requireMoveIdentityTerm(t, ValidateMoveTransactionIdentities(move, anchor), "IncomingCopy.CopyID")
}

func TestValidateMoveTransactionIdentitiesRejectsInvalidCopies(t *testing.T) {
	move, anchor := moveTransactionFixture()
	changed := *move.Status.IncomingCopy
	changed.PoolUID = ""
	move.Status.IncomingCopy = &changed
	anchor.PoolUID = ""
	requireMoveIdentityTerm(t, ValidateMoveTransactionIdentities(move, anchor), "IncomingCopy")

	move, anchor = moveTransactionFixture()
	changed = *move.Status.DestinationCopy
	changed.PoolUID = ""
	move.Status.DestinationCopy = &changed
	requireMoveIdentityTerm(t, ValidateMoveTransactionIdentities(move, anchor), "DestinationCopy")
}

func requireMoveIdentityTerm(t *testing.T, err error, term string) {
	t.Helper()
	if err == nil {
		t.Fatalf("contradicted %s was accepted", term)
	}
	if want := MoveIdentityMismatch(term).Error(); err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}
