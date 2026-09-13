package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/cleanupapi"
	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
)

func (r *Reconciler) cleanupState(ctx context.Context, move volumeapi.Move) (complete, failed bool, err error) {
	spec, err := moveCleanupSpec(move)
	if err != nil {
		return false, false, nil
	}
	request, err := r.Cleanups.Get(ctx, spec.Authority)
	if apierrors.IsNotFound(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if request.Spec != spec {
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
	if move == nil || move.Status.SourceCopy == nil || move.Status.DestinationCopy == nil || move.UID == "" {
		return fmt.Errorf("move cleanup contract is incomplete")
	}
	spec, err := moveCleanupSpec(*move)
	if err != nil {
		return err
	}
	return r.ensureCleanupSpec(ctx, move, spec)
}

func (r *Reconciler) ensureCleanupSpec(ctx context.Context, move *volumeapi.Move, spec cleanupapi.Spec) error {
	if r.Cleanups == nil || r.CleanupOperator == nil || move == nil || move.UID == "" ||
		spec.Authority != (cleanupapi.Authority{Kind: "ShiftPVMove", Name: move.Name, UID: move.UID}) ||
		spec.Target.VolumeID != move.Spec.VolumeID || spec.Validate() != nil {
		return fmt.Errorf("move cleanup contract is incomplete")
	}
	request, err := r.Cleanups.Ensure(ctx, spec)
	if err != nil {
		return err
	}
	if request.Status.Phase == cleanupapi.PhaseCompleted {
		return nil
	}
	if request.Status.Phase != cleanupapi.PhaseVerifying && request.Status.Phase != cleanupapi.PhaseConfirmingAbsence {
		request, err = r.CleanupOperator.Reclaim(ctx, request, r.Cleanups)
		if err != nil {
			return err
		}
	}
	if request.Status.Phase == cleanupapi.PhaseCompleted {
		return nil
	}
	if (request.Status.Phase != cleanupapi.PhaseVerifying && request.Status.Phase != cleanupapi.PhaseConfirmingAbsence) ||
		request.Status.Receipt == nil || request.Status.Executor == nil ||
		request.Status.Receipt.OperationID != spec.OperationID || request.Status.Receipt.ExecutorUID != request.Status.Executor.JobUID ||
		!request.Status.Receipt.Retired || !request.Status.Receipt.Purged {
		return fmt.Errorf("cleanup receipt is incomplete")
	}
	request, complete, err := r.Cleanups.ReconcileAbsence(ctx, request)
	if err != nil || complete {
		return err
	}
	if request.Status.Phase == cleanupapi.PhaseConfirmingAbsence {
		_, _, err = r.Cleanups.ReconcileAbsence(ctx, request)
	}
	return err
}

func moveCleanupSpec(move volumeapi.Move) (cleanupapi.Spec, error) {
	if move.Name == "" || move.UID == "" || move.Status.SourceCopy == nil {
		return cleanupapi.Spec{}, fmt.Errorf("move cleanup identity is incomplete")
	}
	spec := cleanupapi.Spec{
		OperationID: "cleanup-" + move.UID,
		Target:      *move.Status.SourceCopy,
		Reason:      "MoveSource",
		Authority:   cleanupapi.Authority{Kind: "ShiftPVMove", Name: move.Name, UID: move.UID},
	}
	if err := spec.Validate(); err != nil {
		return cleanupapi.Spec{}, err
	}
	return spec, nil
}
