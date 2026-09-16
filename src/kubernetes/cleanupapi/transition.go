package cleanupapi

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"time"

	"github.com/project-jelly/ShiftPV/src/volume"
)

func validateTransition(current Cleanup, next Status) error {
	phase := current.Status.Phase
	if phase == "" {
		phase = PhasePending
	}
	allowed := map[string]map[string]bool{
		PhasePending:           {PhasePending: true, PhaseRunning: true, PhaseNeedsReview: true},
		PhaseRunning:           {PhaseRunning: true, PhaseVerifying: true, PhaseNeedsReview: true},
		PhaseVerifying:         {PhaseVerifying: true, PhaseConfirmingAbsence: true, PhaseNeedsReview: true},
		PhaseConfirmingAbsence: {PhaseConfirmingAbsence: true, PhaseCompleted: true, PhaseNeedsReview: true},
		PhaseCompleted:         {PhaseCompleted: true},
		PhaseNeedsReview:       {PhaseNeedsReview: true},
	}
	if !allowed[phase][next.Phase] {
		return fmt.Errorf("%w: phase %s cannot transition to %s", ErrConflict, phase, next.Phase)
	}
	if current.Status.Executor != nil && !executorTransitionAllowed(phase, next.Phase, current.Status.Executor, next.Executor, current.Status.Receipt, next.Receipt) {
		return fmt.Errorf("%w: executor identity is immutable", ErrConflict)
	}
	if next.Phase == PhaseRunning && next.Executor == nil {
		return fmt.Errorf("%w: Running requires an executor", ErrConflict)
	}
	if current.Status.Receipt != nil && !reflect.DeepEqual(current.Status.Receipt, next.Receipt) {
		return fmt.Errorf("%w: receipt is immutable", ErrConflict)
	}
	if next.Receipt != nil {
		if next.Executor == nil || next.Receipt.OperationID != current.Spec.OperationID || next.Receipt.ExecutorUID != next.Executor.JobUID {
			return fmt.Errorf("%w: receipt identity does not match intent and executor", ErrConflict)
		}
		if _, err := time.Parse(time.RFC3339Nano, next.Receipt.ObservedAt); err != nil {
			return fmt.Errorf("%w: invalid receipt time", ErrConflict)
		}
	}
	if next.Phase == PhaseVerifying {
		if !validPurgedReceipt(current, next) || next.AbsenceProof != nil {
			return fmt.Errorf("%w: Verifying requires a Pod-bound executor and only the API receipt", ErrConflict)
		}
	}
	if current.Status.AbsenceProof != nil {
		if next.AbsenceProof == nil || !sameAbsenceFence(current.Status.AbsenceProof, next.AbsenceProof) {
			return fmt.Errorf("%w: absence fence identity is immutable", ErrConflict)
		}
		if current.Status.AbsenceProof.ConfirmedAt != "" && !reflect.DeepEqual(current.Status.AbsenceProof, next.AbsenceProof) {
			return fmt.Errorf("%w: confirmed absence proof is immutable", ErrConflict)
		}
	}
	if next.Phase == PhaseConfirmingAbsence || next.Phase == PhaseCompleted {
		if !validPurgedReceipt(current, next) || !validAbsenceFence(current.Spec.Target, next.AbsenceProof) {
			return fmt.Errorf("%w: cleanup confirmation requires a post-receipt absence fence", ErrConflict)
		}
	}
	if next.Phase == PhaseCompleted {
		if !confirmedAbsence(next.AbsenceProof) || next.SettledAt == "" {
			return fmt.Errorf("%w: Completed requires a purged receipt and later complete exact absence proof", ErrConflict)
		}
		if _, err := time.Parse(time.RFC3339Nano, next.SettledAt); err != nil {
			return fmt.Errorf("%w: invalid settlement time", ErrConflict)
		}
	}
	return nil
}

func validPurgedReceipt(cleanup Cleanup, status Status) bool {
	if status.Executor == nil || status.Receipt == nil || !volume.ValidObjectName(status.Executor.JobName) ||
		!volume.ValidIdentityToken(status.Executor.JobUID) || !volume.ValidIdentityToken(status.Executor.PodUID) ||
		status.Executor.NodeName != cleanup.Spec.Target.NodeName ||
		status.Receipt.OperationID != cleanup.Spec.OperationID || status.Receipt.ExecutorUID != status.Executor.JobUID ||
		!status.Receipt.Retired || !status.Receipt.Purged || !validSHA256Digest(status.Receipt.LocalReceiptDigest) {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, status.Receipt.ObservedAt)
	return err == nil
}

func validSHA256Digest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

func executorTransitionAllowed(currentPhase, nextPhase string, current, next *Executor, currentReceipt, nextReceipt *Receipt) bool {
	if reflect.DeepEqual(current, next) {
		return true
	}
	// A Job retry gets a new Pod UID. Before any API receipt exists, the exact
	// parent may rebind execution to that new Pod while retaining the same Job
	// UID, operation, and node. The node-local per-volume lock and operation
	// intent serialize an old Pod with its retry and make the effect idempotent.
	// Once a receipt exists, the complete executor identity is immutable.
	return currentPhase == PhaseRunning && nextPhase == PhaseRunning && currentReceipt == nil && nextReceipt == nil && current != nil && next != nil &&
		volume.ValidIdentityToken(next.PodUID) && current.JobName == next.JobName && current.JobUID == next.JobUID && current.NodeName == next.NodeName
}

func sameAbsenceFence(current, next *AbsenceProof) bool {
	return current != nil && next != nil && current.RequestID == next.RequestID && current.PoolName == next.PoolName &&
		current.PoolUID == next.PoolUID && current.RequiredGeneration == next.RequiredGeneration
}

func validAbsenceFence(target CopyIdentity, proof *AbsenceProof) bool {
	return proof != nil && volume.ValidIdentityToken(proof.RequestID) && proof.PoolName == target.PoolName &&
		proof.PoolUID == target.PoolUID && proof.RequiredGeneration > 0
}

func confirmedAbsence(proof *AbsenceProof) bool {
	if proof == nil || !proof.Valid || !proof.Complete || !proof.Absent || proof.ObservedGeneration < proof.RequiredGeneration || proof.ConfirmedAt == "" {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, proof.ConfirmedAt)
	return err == nil
}
