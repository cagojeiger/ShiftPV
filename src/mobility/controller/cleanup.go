package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/cleanupapi"
	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
)

func (r *Reconciler) cleanupState(ctx context.Context, move volumeapi.Move) (complete, failed bool, err error) {
	if move.Status.CleanupName == "" {
		return false, false, nil
	}
	request, err := r.Cleanups.Get(ctx, move.Status.CleanupName)
	if err != nil {
		return false, false, err
	}
	if request.Spec.Authority.Kind != "ShiftPVMove" || request.Spec.Authority.Name != move.Name || request.Spec.Authority.UID != move.UID || move.Status.SourceCopy == nil || request.Spec.Target != *move.Status.SourceCopy {
		return false, false, fmt.Errorf("cleanup request belongs to a different Move")
	}
	return request.Status.Phase == cleanupapi.PhaseCompleted, request.Status.Phase == cleanupapi.PhaseNeedsReview, nil
}

func (r *Reconciler) acknowledgeCleanup(ctx context.Context, move volumeapi.Move) error {
	complete, failed, err := r.cleanupState(ctx, move)
	if err != nil {
		return err
	}
	if !complete || failed {
		return fmt.Errorf("cleanup success is not confirmed")
	}
	return nil
}

func (r *Reconciler) ensureCleanupContract(ctx context.Context, move *volumeapi.Move) error {
	if r.Cleanups == nil || r.CleanupOperator == nil || move.Status.SourceCopy == nil || move.Status.DestinationCopy == nil || move.UID == "" {
		return fmt.Errorf("move cleanup contract is incomplete")
	}
	spec := cleanupapi.Spec{
		OperationID: "cleanup-" + move.UID,
		Target:      *move.Status.SourceCopy,
		Reason:      "MoveSource",
		Approved:    true,
		Authority:   cleanupapi.Authority{Kind: "ShiftPVMove", Name: move.Name, UID: move.UID},
	}
	request, err := r.Cleanups.Ensure(ctx, spec)
	if err != nil {
		return err
	}
	if move.Status.CleanupName != "" && move.Status.CleanupName != request.Name {
		return fmt.Errorf("move cleanup identity changed")
	}
	if move.Status.CleanupName == "" || move.Status.CleanupJobName == "" {
		previous := move.Status
		move.Status.CleanupName = request.Name
		move.Status.CleanupJobName = request.Name + "-effect"
		if err := r.persistMoveStatus(ctx, move, previous); err != nil {
			return err
		}
	}
	if request.Status.Phase == cleanupapi.PhaseCompleted {
		return nil
	}
	request, err = r.CleanupOperator.Reclaim(ctx, request, r.Cleanups)
	if err != nil {
		return err
	}
	if request.Status.Phase == cleanupapi.PhaseCompleted {
		return nil
	}
	if request.Status.Phase != cleanupapi.PhaseVerifying || request.Status.Receipt == nil || request.Status.Executor == nil ||
		request.Status.Receipt.OperationID != spec.OperationID || request.Status.Receipt.ExecutorUID != request.Status.Executor.JobUID ||
		!request.Status.Receipt.Retired || !request.Status.Receipt.Purged {
		return fmt.Errorf("cleanup receipt is incomplete")
	}
	settled := time.Now().UTC().Format(time.RFC3339Nano)
	return r.Cleanups.UpdateStatus(ctx, request.Name, request.UID, cleanupapi.Status{
		Phase: cleanupapi.PhaseCompleted, Executor: request.Status.Executor, Receipt: request.Status.Receipt, SettledAt: settled,
	})
}
