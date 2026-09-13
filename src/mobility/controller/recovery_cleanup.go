package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/cleanupapi"
	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/volume"
)

const recoveryCapacitySettled = "RecoverySettled"

var errRecoveryCleanupNeedsReview = errors.New("recovery cleanup needs review")

// settleRecoveryArtifacts is the only recovery step allowed to release a
// Move's temporary capacity hold. Source-owner recovery rolls back the one
// exact destination transaction artifact, while destination-owner recovery
// finishes the ordinary retained-source cleanup. A durable Retiring phase and
// a later Pool inventory make the already-absent case safe without inventing a
// destructive cleanup receipt.
func (r *Reconciler) settleRecoveryArtifacts(ctx context.Context, move *volumeapi.Move, state volumeapi.State) (bool, error) {
	if move.Status.CapacityReason == recoveryCapacitySettled {
		if move.Status.CapacityApproved {
			return false, needsRecoveryCleanupReview("capacity hold is marked settled but remains approved")
		}
		return true, nil
	}
	switch state.OwnerNode {
	case move.Spec.SourceNode:
		if state.Phase != volumeapi.PhaseBlocked || state.ActiveMove != move.Name || state.CurrentCopy == nil ||
			state.CurrentCopy.Validate() != nil || state.CurrentCopy.Role != volume.RoleServing ||
			state.CurrentCopy.NodeName != move.Spec.SourceNode || state.CurrentCopy.VolumeID != move.Spec.VolumeID || state.CurrentCopy.VolumeUID != state.UID {
			return false, needsRecoveryCleanupReview("precommit source authority is incomplete")
		}
		if noDestinationEffectIntent(*move) {
			if move.Status.CapacityApproved {
				return false, needsRecoveryCleanupReview("approved destination hold has no exact artifact identities")
			}
			break
		}
		if move.Status.SourceCopy == nil || *state.CurrentCopy != *move.Status.SourceCopy {
			return false, needsRecoveryCleanupReview("recorded source identity differs from current authority")
		}
		target, present, err := r.rollbackArtifact(ctx, *move)
		if err != nil {
			return false, err
		}
		if present {
			spec := cleanupapi.Spec{
				OperationID: "rollback-" + move.UID,
				Target:      target,
				Reason:      "MoveRollback",
				Authority:   cleanupapi.Authority{Kind: "ShiftPVMove", Name: move.Name, UID: move.UID},
			}
			complete, err := r.ensureRecoveryCleanup(ctx, *move, spec)
			if err != nil || !complete {
				return false, err
			}
		}
	case move.Status.DestinationNode:
		if state.Phase != volumeapi.PhaseReady || state.ActiveMove != move.Name || move.Status.DestinationCopy == nil ||
			state.CurrentCopy == nil || *state.CurrentCopy != *move.Status.DestinationCopy || state.UID != move.Status.DestinationCopy.VolumeUID ||
			!contains(state.PublishedNodes, move.Status.DestinationNode) || contains(state.PublishedNodes, move.Spec.SourceNode) {
			return false, fmt.Errorf("waiting for exact destination publication before source cleanup")
		}
		destinationPool, err := r.Repository.ReadyPoolForNode(ctx, move.Status.DestinationNode)
		if err != nil || !volumeapi.PoolHasPublishedCopy(destinationPool, move.Status.DestinationCopy) {
			return false, fmt.Errorf("waiting for exact destination scanner publication before source cleanup: %w", err)
		}
		spec, err := moveCleanupSpec(*move)
		if err != nil {
			return false, needsRecoveryCleanupReview("postcommit cleanup identity is incomplete: %v", err)
		}
		complete, err := r.ensureRecoveryCleanup(ctx, *move, spec)
		if err != nil || !complete {
			return false, err
		}
	default:
		return false, needsRecoveryCleanupReview("authoritative owner is neither the source nor recorded destination")
	}

	previous := move.Status
	move.Status.CapacityApproved = false
	move.Status.CapacityReason = recoveryCapacitySettled
	if err := r.persistMoveStatus(ctx, move, previous); err != nil {
		return false, err
	}
	// Force a read-back boundary between releasing the hold and opening the
	// Volume mount guard. This also makes controller restarts harmless here.
	return false, nil
}

// rollbackArtifact returns the only exact destination transaction artifact
// visible in an inventory collected strictly after entry into Retiring. A
// valid, complete inventory containing neither identity proves that no cleanup
// effect is required. Any conflicting or problem observation stays untouched.
func (r *Reconciler) rollbackArtifact(ctx context.Context, move volumeapi.Move) (volume.CopyIdentity, bool, error) {
	if err := validRollbackIntent(move); err != nil {
		return volume.CopyIdentity{}, false, needsRecoveryCleanupReview("destination cleanup intent is incomplete: %v", err)
	}

	transitionedAt, err := time.Parse(time.RFC3339Nano, move.Status.LastTransitionTime)
	if err != nil {
		return volume.CopyIdentity{}, false, needsRecoveryCleanupReview("Retiring transition time is invalid")
	}
	pools, err := r.Repository.Pools(ctx)
	if err != nil {
		return volume.CopyIdentity{}, false, err
	}
	var destination *volumeapi.Pool
	for index := range pools {
		pool := &pools[index]
		if pool.NodeName != move.Status.DestinationNode {
			continue
		}
		if destination != nil {
			return volume.CopyIdentity{}, false, needsRecoveryCleanupReview("multiple destination Pools are registered")
		}
		destination = pool
	}
	if destination == nil || destination.Name != move.Status.IncomingCopy.PoolName || destination.UID != move.Status.DestinationPoolUID ||
		destination.UID != move.Status.IncomingCopy.PoolUID || destination.UID != move.Status.DestinationCopy.PoolUID {
		return volume.CopyIdentity{}, false, needsRecoveryCleanupReview("destination Pool identity changed")
	}
	if !contains(destination.Finalizers, cleanupapi.PoolProtectionFinalizer) {
		return volume.CopyIdentity{}, false, needsRecoveryCleanupReview("destination Pool lacks lifecycle protection")
	}

	now := r.now()
	staleAfter := r.PoolReadinessStaleAfter
	if staleAfter <= 0 {
		staleAfter = volumeapi.DefaultPoolReadinessStaleAfter
	}
	if ready, reason := destination.CleanupReadyAt(now, staleAfter); !ready {
		return volume.CopyIdentity{}, false, fmt.Errorf("destination Pool is not cleanup-ready: %s", reason)
	}
	inventory := destination.Status.Inventory
	if inventory == nil || !inventory.Valid || inventory.Truncated || inventory.Message != "" ||
		inventory.ObservedAt.IsZero() || !inventory.ObservedAt.Time.After(transitionedAt) || now.Before(inventory.ObservedAt.Time) || now.Sub(inventory.ObservedAt.Time) > staleAfter {
		return volume.CopyIdentity{}, false, fmt.Errorf("waiting for a fresh complete destination inventory collected after Retiring")
	}

	incomingPresent, destinationPresent := false, false
	for _, observed := range inventory.Copies {
		if observed.Problem != "" {
			return volume.CopyIdentity{}, false, needsRecoveryCleanupReview("destination inventory contains an ambiguous observation")
		}
		if !observed.Present {
			continue
		}
		if observed.Identity == nil {
			return volume.CopyIdentity{}, false, needsRecoveryCleanupReview("destination inventory contains an unidentified artifact")
		}
		identity := *observed.Identity
		if identity.Validate() != nil || identity.PoolName != destination.Name || identity.PoolUID != destination.UID || identity.NodeName != destination.NodeName {
			return volume.CopyIdentity{}, false, needsRecoveryCleanupReview("destination inventory contains an invalid copy identity")
		}
		if observed.Published {
			return volume.CopyIdentity{}, false, needsRecoveryCleanupReview("destination inventory contains a published artifact")
		}
		switch identity {
		case *move.Status.IncomingCopy:
			incomingPresent = true
		case *move.Status.DestinationCopy:
			destinationPresent = true
		default:
			if identity.VolumeID == move.Spec.VolumeID || identity.VolumeUID == move.Status.SourceCopy.VolumeUID ||
				(identity.Role == volume.RoleIncoming && identity.CopyID == move.Status.IncomingCopy.CopyID) {
				return volume.CopyIdentity{}, false, needsRecoveryCleanupReview("destination inventory contains a conflicting volume artifact")
			}
		}
	}
	if incomingPresent && destinationPresent {
		return volume.CopyIdentity{}, false, needsRecoveryCleanupReview("both incoming and promoted destination artifacts are present")
	}
	if incomingPresent {
		return *move.Status.IncomingCopy, true, nil
	}
	if destinationPresent {
		return *move.Status.DestinationCopy, true, nil
	}
	return volume.CopyIdentity{}, false, nil
}

func noDestinationEffectIntent(move volumeapi.Move) bool {
	return move.Status.IncomingCopy == nil && move.Status.DestinationCopy == nil && move.Status.CopyJobName == "" &&
		move.Status.PromotionJobName == "" && move.Status.CopyOperationID == "" && move.Status.PromotionOperationID == ""
}

func validRollbackIntent(move volumeapi.Move) error {
	incoming, destination, source := move.Status.IncomingCopy, move.Status.DestinationCopy, move.Status.SourceCopy
	if move.Name == "" || !volume.ValidIdentityToken(move.UID) || source == nil || incoming == nil || destination == nil ||
		source.Validate() != nil || incoming.Validate() != nil || destination.Validate() != nil ||
		source.Role != volume.RoleServing || source.NodeName != move.Spec.SourceNode || source.VolumeID != move.Spec.VolumeID ||
		!move.Status.CapacityApproved || move.Status.SourceBytes <= 0 ||
		move.Status.DestinationNode == "" || move.Status.DestinationNode == move.Spec.SourceNode || move.Status.DestinationPoolUID == "" ||
		incoming.Role != volume.RoleIncoming || destination.Role != volume.RoleServing ||
		incoming.InstallationID != source.InstallationID || destination.InstallationID != source.InstallationID ||
		incoming.PoolName != destination.PoolName || incoming.PoolUID != destination.PoolUID || incoming.PoolUID != move.Status.DestinationPoolUID ||
		incoming.VolumeID != move.Spec.VolumeID || destination.VolumeID != move.Spec.VolumeID ||
		incoming.VolumeUID != source.VolumeUID || destination.VolumeUID != source.VolumeUID ||
		incoming.NodeName != move.Status.DestinationNode || destination.NodeName != move.Status.DestinationNode ||
		incoming.CopyID != "move-"+move.UID+"-incoming" || destination.CopyID != "move-"+move.UID+"-serving" ||
		move.Status.CopyOperationID != "copy-"+move.UID || move.Status.PromotionOperationID != "promote-"+move.UID ||
		move.Status.CopyJobName != namesFor(move.Name).CopyJob ||
		(move.Status.PromotionJobName != "" && move.Status.PromotionJobName != namesFor(move.Name).PromotionJob) {
		return fmt.Errorf("exact Move transaction identities are required")
	}
	return nil
}

func needsRecoveryCleanupReview(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", errRecoveryCleanupNeedsReview, fmt.Sprintf(format, arguments...))
}

func (r *Reconciler) ensureRecoveryCleanup(ctx context.Context, move volumeapi.Move, spec cleanupapi.Spec) (bool, error) {
	if r.Cleanups == nil {
		return false, fmt.Errorf("recovery cleanup store is not configured")
	}
	err := r.ensureCleanupSpec(ctx, &move, spec)
	current, getErr := r.Cleanups.Get(ctx, spec.Authority)
	if getErr == nil && current.Status.Phase == cleanupapi.PhaseNeedsReview {
		return false, needsRecoveryCleanupReview("cleanup journal entered NeedsReview: %s: %s", current.Status.Reason, current.Status.Message)
	}
	if err != nil {
		if errors.Is(err, cleanupapi.ErrConflict) {
			return false, needsRecoveryCleanupReview("cleanup journal conflicts with the approved recovery target: %v", err)
		}
		return false, err
	}
	if getErr != nil {
		return false, getErr
	}
	return current.Status.Phase == cleanupapi.PhaseCompleted, nil
}
