package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
)

const (
	recoveryQuiescing  = "Quiescing"
	recoveryVerifying  = "Verifying"
	recoveryRetiring   = "Retiring"
	recoveryResuming   = "Resuming"
	recoveryCompleting = "Completing"
	recoveryRecovered  = "Recovered"
)

// Recovery never changes owner. Its journal is separate from the failed Move FSM.
func (r *Reconciler) reconcileRecovery(ctx context.Context, move volumeapi.Move) error {
	state, err := r.Repository.Get(ctx, move.Spec.VolumeID)
	if err != nil {
		return r.recoveryError(ctx, move, "ObservationFailed", err)
	}
	// The final CAS may have succeeded immediately before a controller restart.
	if move.Status.RecoveryPhase == recoveryCompleting && state.Phase == volumeapi.PhaseReady && state.ActiveMove == "" && state.OwnerNode == move.Status.RecoveryOwner {
		return r.recoveryAdvance(ctx, move, recoveryRecovered)
	}
	if state.ActiveMove != move.Name || (state.OwnerNode != move.Spec.SourceNode && state.OwnerNode != move.Status.DestinationNode) || state.OwnerNode == "" {
		return r.recoveryError(ctx, move, "AuthorityMismatch", fmt.Errorf("expected this move and source or committed destination owner"))
	}
	if move.Status.RecoveryOwner != "" && state.OwnerNode != move.Status.RecoveryOwner {
		return r.recoveryError(ctx, move, "AuthorityMismatch", fmt.Errorf("owner changed since recovery request"))
	}
	if move.Status.DestinationNode == "" && (move.Status.CopyJobName != "" || move.Status.PromotionJobName != "") {
		return r.recoveryError(ctx, move, "DestinationUnknown", fmt.Errorf("disk work was recorded without a destination; operator investigation required"))
	}
	canBeReady := move.Status.RecoveryPhase == recoveryResuming || move.Status.RecoveryPhase == recoveryCompleting
	if state.Phase != volumeapi.PhaseBlocked && !(canBeReady && state.Phase == volumeapi.PhaseReady) {
		return r.recoveryError(ctx, move, "StateMismatch", fmt.Errorf("cannot recover volume in phase %q", state.Phase))
	}
	for _, published := range state.PublishedNodes {
		if published != state.OwnerNode {
			return r.recoveryError(ctx, move, "ForeignPublication", fmt.Errorf("node %q is still published; confirm unpublish before recovery", published))
		}
	}
	if err := r.recoveryNodes(ctx, move, state.OwnerNode); err != nil {
		return r.recoveryError(ctx, move, "NodeNotReady", err)
	}
	claim, err := r.recoveryClaim(ctx, move)
	if err != nil {
		return r.recoveryError(ctx, move, "BindingMismatch", err)
	}
	if move.Status.RecoveryPhase == "" {
		move.Status.RecoveryOwner = state.OwnerNode
		return r.recoveryAdvance(ctx, move, recoveryQuiescing)
	}
	err = r.recoveryStep(ctx, move, state, claim)
	if err != nil {
		return r.recoveryError(ctx, move, "RecoveryStepFailed", err)
	}
	return nil
}

func (r *Reconciler) recoveryStep(ctx context.Context, move volumeapi.Move, state volumeapi.State, claim *corev1.PersistentVolumeClaim) error {
	switch move.Status.RecoveryPhase {
	case recoveryQuiescing:
		done, err := r.quiesceMove(ctx, move)
		if err != nil || !done {
			return err
		}
		return r.recoveryAdvance(ctx, move, recoveryVerifying)
	case recoveryVerifying:
		done, err := r.recoveryJob(ctx, move, true)
		if err != nil || !done {
			return err
		}
		return r.recoveryAdvance(ctx, move, recoveryRetiring)
	case recoveryRetiring:
		done, err := r.recoveryJob(ctx, move, false)
		if err != nil || !done {
			return err
		}
		return r.recoveryAdvance(ctx, move, recoveryResuming)
	case recoveryResuming:
		if state.Phase == volumeapi.PhaseBlocked {
			// Keep the lock until the workload has published on the same owner.
			next := state
			next.Phase = volumeapi.PhaseReady
			return r.Repository.CompareAndSetState(ctx, move.Spec.VolumeID, volumeapi.PhaseBlocked, move.Name, state.OwnerNode, next)
		}
		placed, err := r.recoverPlacement(ctx, move, claim)
		if err != nil || !placed || !contains(state.PublishedNodes, state.OwnerNode) {
			return err
		}
		return r.recoveryAdvance(ctx, move, recoveryCompleting)
	case recoveryCompleting:
		done, err := r.removeRecoveryJobs(ctx, move)
		if err != nil || !done {
			return err
		}
		next := state
		next.ActiveMove = ""
		if err := r.Repository.CompareAndSetState(ctx, move.Spec.VolumeID, volumeapi.PhaseReady, move.Name, state.OwnerNode, next); err != nil {
			return err
		}
		return r.recoveryAdvance(ctx, move, recoveryRecovered)
	default:
		return fmt.Errorf("unknown recovery phase %q", move.Status.RecoveryPhase)
	}
}

func (r *Reconciler) recoveryNodes(ctx context.Context, move volumeapi.Move, owner string) error {
	for _, name := range []string{move.Spec.SourceNode, move.Status.DestinationNode} {
		if name == "" {
			continue
		}
		node, err := r.Client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !nodeReady(node) || node.DeletionTimestamp != nil {
			return fmt.Errorf("node %q must be Ready", name)
		}
		if name == owner && node.Spec.Unschedulable {
			return fmt.Errorf("uncordon current owner %q before requesting service recovery", name)
		}
		if _, err := r.poolMountPath(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) recoveryClaim(ctx context.Context, move volumeapi.Move) (*corev1.PersistentVolumeClaim, error) {
	if move.Status.PersistentVolumeName == "" || move.Status.ClaimNamespace == "" || move.Status.ClaimName == "" {
		return nil, fmt.Errorf("move has no recorded binding; do not patch Volume status")
	}
	pv, err := r.Client.CoreV1().PersistentVolumes().Get(ctx, move.Status.PersistentVolumeName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	claim, err := r.Client.CoreV1().PersistentVolumeClaims(move.Status.ClaimNamespace).Get(ctx, move.Status.ClaimName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	ref := pv.Spec.ClaimRef
	if pv.DeletionTimestamp != nil || claim.DeletionTimestamp != nil || pv.Spec.CSI == nil || pv.Spec.CSI.Driver != "csi.shiftpv.io" || pv.Spec.CSI.VolumeHandle != move.Spec.VolumeID || ref == nil || ref.Namespace != claim.Namespace || ref.Name != claim.Name || ref.UID == "" || ref.UID != claim.UID || claim.Spec.VolumeName != pv.Name {
		return nil, fmt.Errorf("PV/PVC binding or volume handle changed")
	}
	namespace, err := r.Client.CoreV1().Namespaces().Get(ctx, claim.Namespace, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if namespace.Labels[admissionNamespaceLabel] != "enabled" {
		return nil, fmt.Errorf("workload namespace must keep ShiftPV admission enabled")
	}
	return claim, nil
}

// Do not edit workload templates, bypass PDBs, or re-add a removed scheduling gate.
func (r *Reconciler) recoverPlacement(ctx context.Context, move volumeapi.Move, claim *corev1.PersistentVolumeClaim) (bool, error) {
	pods, err := r.Client.CoreV1().Pods(claim.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err
	}
	placed := false
	for i := range pods.Items {
		pod := &pods.Items[i]
		if terminalPod(pod) || !podUsesClaim(pod, claim.Name) {
			continue
		}
		if pod.DeletionTimestamp != nil {
			return false, nil
		}
		if pod.Spec.NodeName == move.Status.RecoveryOwner && !hasPlacementHold(pod) {
			placed = true
			continue
		}
		if metav1.GetControllerOf(pod) == nil {
			return false, fmt.Errorf("bare consumer %q must be handled by its operator", pod.Name)
		}
		uid := pod.UID
		err := r.Client.PolicyV1().Evictions(pod.Namespace).Evict(ctx, &policyv1.Eviction{
			ObjectMeta:    metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
			DeleteOptions: &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}},
		})
		if apierrors.IsNotFound(err) {
			err = nil
		}
		return false, err
	}
	return placed, nil
}

func (r *Reconciler) recoveryAdvance(ctx context.Context, move volumeapi.Move, phase string) error {
	previous := move.Status
	move.Status.RecoveryPhase = phase
	move.Status.RecoveryReason = ""
	move.Status.RecoveryMessage = ""
	move.Status.Message = recoveryPhaseMessage(phase, move.Status.RecoveryOwner)
	return r.persistMoveStatus(ctx, &move, previous)
}

func (r *Reconciler) recoveryError(ctx context.Context, move volumeapi.Move, reason string, err error) error {
	previous := move.Status
	move.Status.RecoveryReason = reason
	move.Status.RecoveryMessage = err.Error()
	move.Status.Message = recoveryErrorMessage(reason, err)
	if statusErr := r.persistMoveStatus(ctx, &move, previous); statusErr != nil {
		return fmt.Errorf("%w (record recovery error: %v)", err, statusErr)
	}
	return err
}
