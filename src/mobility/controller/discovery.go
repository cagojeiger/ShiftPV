package controller

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/mobility/fsm"
)

func (r *Reconciler) discoverMoves(ctx context.Context) (discoveryErr error) {
	deferred := make(map[string]int)
	if r.ObserveDiscovery != nil {
		defer func() { r.ObserveDiscovery(deferred, discoveryErr) }()
	}
	volumes, err := r.Repository.ListVolumes(ctx)
	if err != nil {
		return err
	}
	moves, err := r.Repository.ListMoves(ctx)
	if err != nil {
		return err
	}
	activeByVolume := make(map[string]bool)
	for _, move := range moves {
		if move.Status.RecoveryPhase == recoveryRecovered {
			continue
		}
		if move.Spec.Recovery == "ResumeOwner" && move.Status.Phase == string(fsm.PhaseBlocked) {
			// The final Volume CAS can precede recovery status persistence. Do not
			// discover a new transaction across that crash boundary on either owner.
			activeByVolume[move.Spec.VolumeID] = true
			continue
		}
		state, exists := volumes[move.Spec.VolumeID]
		if move.Status.Phase != string(fsm.PhaseSucceeded) &&
			(move.Status.Phase != string(fsm.PhaseBlocked) || (exists && state.OwnerNode == move.Spec.SourceNode)) {
			activeByVolume[move.Spec.VolumeID] = true
		}
	}
	for volumeID, state := range volumes {
		if state.Phase != volumeapi.PhaseReady || state.ActiveMove != "" || activeByVolume[volumeID] {
			continue
		}
		node, err := r.Client.CoreV1().Nodes().Get(ctx, state.OwnerNode, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("read owner Node %q: %w", state.OwnerNode, err)
		}
		if !node.Spec.Unschedulable || !nodeReady(node) {
			continue
		}
		candidate := volumeapi.Move{Spec: volumeapi.MoveSpec{VolumeID: volumeID, SourceNode: state.OwnerNode}}
		observed, err := r.observe(ctx, candidate)
		if err != nil {
			return fmt.Errorf("preflight volume %s: %w", volumeID, err)
		}
		if !observed.FSM.PreconditionsValid {
			deferred[observed.FSM.UnsafeReason]++
			klog.V(2).Infof("deferred ShiftPV mobility for volume %s: %s", volumeID, observed.FSM.UnsafeReason)
			continue
		}
		move, err := r.Repository.CreateMove(ctx, moveGenerateName(volumeID), volumeapi.MoveSpec{VolumeID: volumeID, SourceNode: state.OwnerNode})
		if err != nil {
			return err
		}
		previous := move.Status
		move.Status.Phase = string(fsm.PhasePending)
		move.Status.Message = mobilityMessage(fsm.PhasePending, "")
		if err := r.persistMoveStatus(ctx, &move, previous); err != nil {
			return err
		}
		activeByVolume[volumeID] = true
		klog.Infof("created ShiftPVMove %s for cordoned node %s volume %s", move.Name, state.OwnerNode, volumeID)
	}
	return nil
}

func moveGenerateName(volumeID string) string {
	return "move-" + strings.TrimPrefix(volumeID, "shiftpv-") + "-"
}
