package controller

import (
	"context"

	"k8s.io/klog/v2"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/mobility/fsm"
)

func (r *Reconciler) reconcileMove(ctx context.Context, move volumeapi.Move) error {
	previous := move.Status
	if move.Status.Phase == "" {
		move.Status.Phase = string(fsm.PhasePending)
		move.Status.Message = mobilityMessage(fsm.PhasePending, "")
		return r.persistMoveStatus(ctx, &move, previous)
	}
	observed, err := r.observe(ctx, move)
	if err != nil {
		return r.recordMoveError(ctx, &move, previous, "ObservationFailed", "failed to observe Kubernetes state", err)
	}
	if pendingMoveObsoleted(move, observed) {
		klog.Infof("deleting unstarted ShiftPVMove %s because source node %s is schedulable", move.Name, move.Spec.SourceNode)
		return r.Repository.DeleteMove(ctx, move.Name, move.UID)
	}
	decision, err := fsm.Decide(fsm.Phase(move.Status.Phase), observed.FSM)
	if err != nil {
		return r.recordMoveError(ctx, &move, previous, "ActionFailed", "failed to decide the next mobility action", err)
	}
	if err := r.execute(ctx, &move, observed, decision); err != nil {
		return r.recordMoveError(ctx, &move, previous, "ActionFailed", "failed to execute the current mobility action", err)
	}
	move.Status.Phase = string(decision.Next)
	move.Status.Reason = decision.Reason
	move.Status.Message = mobilityMessage(decision.Next, decision.Reason)
	return r.persistMoveStatus(ctx, &move, previous)
}

func pendingMoveObsoleted(move volumeapi.Move, observed observation) bool {
	return move.Status.Phase == string(fsm.PhasePending) &&
		observed.FSM.SourceHealthy && !observed.SourceCordoned &&
		observed.Volume.Phase == volumeapi.PhaseReady && observed.Volume.ActiveMove == "" &&
		observed.Volume.OwnerNode == move.Spec.SourceNode
}
