package controller

import (
	"context"
	"errors"
	"fmt"

	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/mobility/fsm"
)

func (r *Reconciler) execute(ctx context.Context, move *volumeapi.Move, observed observation, decision fsm.Decision) error {
	switch decision.Action {
	case fsm.ActionWait:
		return nil
	case fsm.ActionLockVolume:
		return r.lockVolume(ctx, move, observed)
	case fsm.ActionEvictConsumer:
		return r.evictConsumer(ctx, move, observed)
	case fsm.ActionReleasePlacement:
		return r.releasePlacement(ctx, move, observed)
	case fsm.ActionEnsureCopy:
		return r.ensureCopy(ctx, move, observed)
	case fsm.ActionEnsurePromotion:
		return r.ensurePromotion(ctx, move, observed)
	case fsm.ActionCommitOwner:
		return r.commitOwner(ctx, move, observed)
	case fsm.ActionEnsureCleanup:
		return r.ensureCleanup(ctx, move, observed)
	case fsm.ActionMarkSucceeded:
		return r.markSucceeded(ctx, move, observed)
	case fsm.ActionMarkBlocked:
		return r.markBlocked(ctx, move, observed, decision.Reason)
	default:
		return fmt.Errorf("unsupported mobility action %q", decision.Action)
	}
}

func (r *Reconciler) lockVolume(ctx context.Context, move *volumeapi.Move, observed observation) error {
	if observed.PV == nil || observed.Claim == nil || observed.Consumer == nil {
		return fmt.Errorf("mobility binding or consumer disappeared before volume lock")
	}
	if observed.Volume.OwnerNode != move.Spec.SourceNode {
		return fmt.Errorf("volume owner %q does not match move source %q", observed.Volume.OwnerNode, move.Spec.SourceNode)
	}
	if !observed.FSM.VolumeLocked {
		next := volumeapi.State{
			Phase:          volumeapi.PhaseMoving,
			OwnerNode:      move.Spec.SourceNode,
			ActiveMove:     move.Name,
			PublishedNodes: append([]string(nil), observed.Volume.PublishedNodes...),
		}
		err := r.Repository.CompareAndSetState(ctx, move.Spec.VolumeID, volumeapi.PhaseReady, "", move.Spec.SourceNode, next)
		if err != nil && !errors.Is(err, volumeapi.ErrStateConflict) {
			return err
		}
		if err != nil {
			current, getErr := r.Repository.Get(ctx, move.Spec.VolumeID)
			if getErr != nil {
				return getErr
			}
			if current.Phase != volumeapi.PhaseMoving || current.ActiveMove != move.Name || current.OwnerNode != move.Spec.SourceNode {
				return err
			}
		}
	}
	move.Status.PersistentVolumeName = observed.PV.Name
	move.Status.ClaimNamespace = observed.Claim.Namespace
	move.Status.ClaimName = observed.Claim.Name
	move.Status.ConsumerName = observed.Consumer.Name
	move.Status.CandidateNodes = append([]string(nil), observed.CandidateNodes...)
	return nil
}

func (r *Reconciler) evictConsumer(ctx context.Context, move *volumeapi.Move, observed observation) error {
	if observed.Consumer == nil {
		move.Status.EvictionRequested = true
		return nil
	}
	err := r.Client.PolicyV1().Evictions(observed.Consumer.Namespace).Evict(ctx, &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{Name: observed.Consumer.Name, Namespace: observed.Consumer.Namespace},
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("evict consumer Pod: %w", err)
	}
	move.Status.EvictionRequested = true
	return nil
}

func (r *Reconciler) releasePlacement(ctx context.Context, move *volumeapi.Move, observed observation) error {
	if observed.Replacement == nil {
		return fmt.Errorf("replacement Pod is not observed")
	}
	name := observed.Replacement.Name
	namespace := observed.Replacement.Namespace
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		pod, err := r.Client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		gates := pod.Spec.SchedulingGates[:0]
		for _, gate := range pod.Spec.SchedulingGates {
			if gate.Name != placementHoldName {
				gates = append(gates, gate)
			}
		}
		if len(gates) == len(pod.Spec.SchedulingGates) {
			return nil
		}
		pod.Spec.SchedulingGates = gates
		_, err = r.Client.CoreV1().Pods(namespace).Update(ctx, pod, metav1.UpdateOptions{})
		return err
	}); err != nil {
		return fmt.Errorf("release replacement Pod placement hold: %w", err)
	}
	move.Status.ReplacementName = name
	return nil
}

func (r *Reconciler) ensureCopy(ctx context.Context, move *volumeapi.Move, observed observation) error {
	if observed.DestinationNode == "" {
		return fmt.Errorf("destination node is not observed")
	}
	move.Status.DestinationNode = observed.DestinationNode
	move.Status.ReplacementName = observed.Replacement.Name
	move.Status.CopyJobName = observed.Names.CopyJob
	return r.ensureCopyResources(ctx, *move, observed.Names)
}

func (r *Reconciler) ensurePromotion(ctx context.Context, move *volumeapi.Move, observed observation) error {
	if move.Status.DestinationNode == "" {
		move.Status.DestinationNode = observed.DestinationNode
	}
	move.Status.PromotionJobName = observed.Names.PromotionJob
	return r.ensurePromotionJob(ctx, *move, observed.Names)
}

func (r *Reconciler) commitOwner(ctx context.Context, move *volumeapi.Move, observed observation) error {
	destination := move.Status.DestinationNode
	if destination == "" {
		destination = observed.DestinationNode
	}
	if destination == "" {
		return fmt.Errorf("destination node is empty")
	}
	if observed.FSM.OwnerCommitted {
		return nil
	}
	next := volumeapi.State{Phase: volumeapi.PhaseReady, OwnerNode: destination, ActiveMove: move.Name, PublishedNodes: append([]string(nil), observed.Volume.PublishedNodes...)}
	err := r.Repository.CompareAndSetState(ctx, move.Spec.VolumeID, volumeapi.PhaseMoving, move.Name, move.Spec.SourceNode, next)
	if err != nil && !errors.Is(err, volumeapi.ErrStateConflict) {
		return err
	}
	if err != nil {
		current, getErr := r.Repository.Get(ctx, move.Spec.VolumeID)
		if getErr != nil {
			return getErr
		}
		if current.Phase != volumeapi.PhaseReady || current.OwnerNode != destination || current.ActiveMove != move.Name {
			return err
		}
	}
	return nil
}

func (r *Reconciler) ensureCleanup(ctx context.Context, move *volumeapi.Move, observed observation) error {
	move.Status.CleanupJobName = observed.Names.CleanupJob
	return r.ensureCleanupJob(ctx, *move, observed.Names)
}

func (r *Reconciler) markSucceeded(ctx context.Context, move *volumeapi.Move, observed observation) error {
	if err := r.deleteTransferResources(ctx, observed.Names); err != nil {
		return err
	}
	if observed.Volume.Phase == volumeapi.PhaseReady && observed.Volume.ActiveMove == "" {
		return nil
	}
	if observed.Volume.Phase != volumeapi.PhaseReady || observed.Volume.ActiveMove != move.Name {
		return fmt.Errorf("cannot complete volume in phase %q with active move %q", observed.Volume.Phase, observed.Volume.ActiveMove)
	}
	next := observed.Volume
	next.ActiveMove = ""
	return r.Repository.CompareAndSetState(ctx, move.Spec.VolumeID, volumeapi.PhaseReady, move.Name, observed.Volume.OwnerNode, next)
}

func (r *Reconciler) markBlocked(ctx context.Context, move *volumeapi.Move, observed observation, reason string) error {
	move.Status.Reason = reason
	move.Status.Message = "mobility stopped without automatic rollback"
	if observed.Volume.Phase == volumeapi.PhaseBlocked && observed.Volume.ActiveMove == move.Name {
		return nil
	}
	if observed.Volume.Phase == volumeapi.PhaseReady && observed.Volume.ActiveMove == "" && fsm.Phase(move.Status.Phase) == fsm.PhasePending {
		return nil
	}
	if (observed.Volume.Phase != volumeapi.PhaseReady || observed.Volume.ActiveMove != move.Name) &&
		(observed.Volume.Phase != volumeapi.PhaseMoving || observed.Volume.ActiveMove != move.Name) {
		return fmt.Errorf("cannot block volume in phase %q with active move %q", observed.Volume.Phase, observed.Volume.ActiveMove)
	}
	next := observed.Volume
	next.Phase = volumeapi.PhaseBlocked
	next.ActiveMove = move.Name
	err := r.Repository.CompareAndSetState(ctx, move.Spec.VolumeID, observed.Volume.Phase, observed.Volume.ActiveMove, observed.Volume.OwnerNode, next)
	if err == nil || !errors.Is(err, volumeapi.ErrStateConflict) {
		return err
	}
	current, getErr := r.Repository.Get(ctx, move.Spec.VolumeID)
	if getErr != nil {
		return getErr
	}
	if current.Phase == volumeapi.PhaseBlocked && current.ActiveMove == move.Name {
		return nil
	}
	return err
}
