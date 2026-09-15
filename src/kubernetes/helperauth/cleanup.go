package helperauth

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/cleanupapi"
	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
)

// CleanupOptions carries the exact parent intent and executor binding a cleanup helper rechecks.
type CleanupOptions struct {
	Authority cleanupapi.Authority
	Approved  cleanupapi.Cleanup
	JobName   string
	JobUID    string
	Pod       *corev1.Pod
}

// CleanupAuthority rechecks the durable parent intent and executor binding before every destructive step.
func CleanupAuthority(cleanups *cleanupapi.Store, registry *volumeapi.Registry, options CleanupOptions) func(context.Context, bool) error {
	approved := options.Approved
	return func(checkCtx context.Context, _ bool) error {
		current, err := cleanups.Get(checkCtx, options.Authority)
		if err != nil {
			return fmt.Errorf("read cleanup intent: %w", err)
		}
		if current.UID != approved.UID || current.Spec != approved.Spec || current.Status.Phase != cleanupapi.PhaseRunning ||
			!MatchesCleanupExecutor(current.Status.Executor, options.JobName, options.JobUID, options.Pod) {
			return fmt.Errorf("cleanup intent changed: %w", cleanupapi.ErrConflict)
		}
		installationID, err := registry.InstallationID(checkCtx)
		if err != nil {
			return fmt.Errorf("read installation identity: %w", err)
		}
		if installationID != approved.Spec.Target.InstallationID {
			return fmt.Errorf("installation authority changed: %w", volumeapi.ErrStateConflict)
		}
		pool, err := registry.PoolForIdentity(checkCtx, approved.Spec.Target.PoolName, approved.Spec.Target.PoolUID, approved.Spec.Target.NodeName)
		if err != nil {
			return fmt.Errorf("read cleanup target Pool: %w", err)
		}
		if pool.Name != approved.Spec.Target.PoolName || pool.UID != approved.Spec.Target.PoolUID ||
			!slices.Contains(pool.Finalizers, volumeapi.PoolProtectionFinalizer) {
			return fmt.Errorf("Pool authority changed: %w", volumeapi.ErrStateConflict)
		}
		return VerifyCleanupAuthority(checkCtx, registry, approved)
	}
}

// MatchesCleanupExecutor reports whether the durable executor is the exact running Pod.
func MatchesCleanupExecutor(executor *cleanupapi.Executor, jobName, jobUID string, pod *corev1.Pod) bool {
	return executor != nil && pod != nil && pod.UID != "" && *executor == (cleanupapi.Executor{
		JobName: jobName, JobUID: jobUID, PodUID: string(pod.UID), NodeName: pod.Spec.NodeName,
	})
}

// OwnedByCleanupParent reports whether the references name the exact cleanup parent as controller.
func OwnedByCleanupParent(references []metav1.OwnerReference, authority cleanupapi.Authority) bool {
	for _, owner := range references {
		if owner.APIVersion == "shiftpv.io/v1alpha1" && owner.Controller != nil && *owner.Controller &&
			owner.Kind == authority.Kind && owner.Name == authority.Name && string(owner.UID) == authority.UID {
			return true
		}
	}
	return false
}

// VerifyCleanupAuthority rechecks the parent durable state that authorizes the exact cleanup.
func VerifyCleanupAuthority(ctx context.Context, registry *volumeapi.Registry, cleanup cleanupapi.Cleanup) error {
	switch cleanup.Spec.Authority.Kind {
	case "ShiftPVVolume":
		state, err := registry.Get(ctx, cleanup.Spec.Authority.Name)
		if err != nil {
			return fmt.Errorf("read volume cleanup state: %w", err)
		}
		if state.UID != cleanup.Spec.Authority.UID || state.CurrentCopy == nil || *state.CurrentCopy != cleanup.Spec.Target ||
			state.Phase != volumeapi.PhaseDeleting || state.DeletionOperationID != cleanup.Spec.OperationID || state.ActiveMove != "" || len(state.PublishedNodes) != 0 {
			return fmt.Errorf("volume cleanup authority changed: %w", volumeapi.ErrStateConflict)
		}
		return nil
	case "ShiftPVMove":
		return verifyMoveCleanupAuthority(ctx, registry, cleanup)
	default:
		return fmt.Errorf("cleanup authority %q is not implemented", cleanup.Spec.Authority.Kind)
	}
}

func verifyMoveCleanupAuthority(ctx context.Context, registry *volumeapi.Registry, cleanup cleanupapi.Cleanup) error {
	move, err := registry.GetMove(ctx, cleanup.Spec.Authority.Name)
	if err != nil {
		return fmt.Errorf("read move cleanup Move: %w", err)
	}
	if move.UID != cleanup.Spec.Authority.UID || move.Spec.VolumeID != cleanup.Spec.Target.VolumeID ||
		move.Status.SourceCopy == nil || move.Status.SourceCopy.NodeName != move.Spec.SourceNode {
		return fmt.Errorf("move cleanup authority changed: %w", volumeapi.ErrStateConflict)
	}
	state, err := registry.Get(ctx, move.Spec.VolumeID)
	if err != nil {
		return fmt.Errorf("read move cleanup volume state: %w", err)
	}
	if state.UID != cleanup.Spec.Target.VolumeUID || state.ActiveMove != move.Name || state.CurrentCopy == nil {
		return fmt.Errorf("move cleanup volume authority changed: %w", volumeapi.ErrStateConflict)
	}
	switch cleanup.Spec.Reason {
	case "MoveSource":
		if cleanup.Spec.OperationID != "cleanup-"+move.UID || *move.Status.SourceCopy != cleanup.Spec.Target || move.Status.DestinationCopy == nil {
			return fmt.Errorf("move source cleanup identity changed: %w", volumeapi.ErrStateConflict)
		}
		normal := (move.Status.Phase == "WaitingForDestinationPublish" || move.Status.Phase == "CleaningSource") &&
			state.Phase == volumeapi.PhaseReady && state.OwnerNode == move.Status.DestinationNode &&
			*state.CurrentCopy == *move.Status.DestinationCopy && slices.Contains(state.PublishedNodes, move.Status.DestinationNode) &&
			!slices.Contains(state.PublishedNodes, move.Spec.SourceNode)
		recovery := move.Status.Phase == "Blocked" && move.Spec.Recovery == "ResumeOwner" && move.Status.RecoveryPhase == "Retiring" &&
			move.Status.RecoveryOwner == move.Status.DestinationNode && state.Phase == volumeapi.PhaseReady &&
			state.OwnerNode == move.Status.DestinationNode && *state.CurrentCopy == *move.Status.DestinationCopy &&
			slices.Contains(state.PublishedNodes, move.Status.DestinationNode) && !slices.Contains(state.PublishedNodes, move.Spec.SourceNode)
		if !normal && !recovery {
			return fmt.Errorf("move source cleanup authority changed: %w", volumeapi.ErrStateConflict)
		}
		destinationPool, err := registry.ReadyPoolForNode(ctx, move.Status.DestinationNode)
		if err != nil {
			return fmt.Errorf("destination Pool publication proof unavailable: %w", err)
		}
		if !volumeapi.PoolHasPublishedCopy(destinationPool, move.Status.DestinationCopy) {
			return fmt.Errorf("move source cleanup publication proof changed: %w", volumeapi.ErrStateConflict)
		}
		return nil
	case "MoveRollback":
		targetIsDestination := (move.Status.IncomingCopy != nil && *move.Status.IncomingCopy == cleanup.Spec.Target) ||
			(move.Status.DestinationCopy != nil && *move.Status.DestinationCopy == cleanup.Spec.Target)
		if cleanup.Spec.OperationID != "rollback-"+move.UID || !targetIsDestination ||
			cleanup.Spec.Target.NodeName != move.Status.DestinationNode || cleanup.Spec.Target.PoolUID != move.Status.DestinationPoolUID ||
			move.Status.Phase != "Blocked" || move.Spec.Recovery != "ResumeOwner" || move.Status.RecoveryPhase != "Retiring" ||
			move.Status.RecoveryOwner != move.Spec.SourceNode || state.Phase != volumeapi.PhaseBlocked || state.OwnerNode != move.Spec.SourceNode ||
			*state.CurrentCopy != *move.Status.SourceCopy || slices.Contains(state.PublishedNodes, cleanup.Spec.Target.NodeName) {
			return fmt.Errorf("move rollback cleanup authority changed: %w", volumeapi.ErrStateConflict)
		}
		return nil
	default:
		return fmt.Errorf("move cleanup reason %q is not implemented", cleanup.Spec.Reason)
	}
}
