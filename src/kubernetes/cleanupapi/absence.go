package cleanupapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"

	"github.com/project-jelly/ShiftPV/src/kubernetes/volumeapi"
)

// ReconcileAbsence advances a receipt-bearing journal through a causal Pool
// scan fence. The first call after Verifying bumps spec.scanEpoch and records
// the generation returned by the API. Later calls complete only when the exact
// current Pool generation has a valid, complete, non-truncated inventory whose
// per-copy evidence is internally consistent and shows the target neither
// present nor published. A failed or ambiguous fence write is safe to retry: an
// unrecorded bump is followed by another bump.
func (s *Store) ReconcileAbsence(ctx context.Context, expected Cleanup) (Cleanup, bool, error) {
	current, err := s.Get(ctx, expected.Spec.Authority)
	if err != nil {
		return Cleanup{}, false, err
	}
	if current.UID != expected.UID || current.Name != expected.Name || !reflect.DeepEqual(current.Spec, expected.Spec) {
		return Cleanup{}, false, ErrConflict
	}
	switch current.Status.Phase {
	case PhaseCompleted:
		if !validPurgedReceipt(current, current.Status) || !validAbsenceFence(current.Spec.Target, current.Status.AbsenceProof) ||
			!confirmedAbsence(current.Status.AbsenceProof) || current.Status.SettledAt == "" {
			return Cleanup{}, false, fmt.Errorf("%w: completed cleanup proof is invalid", ErrConflict)
		}
		return current, true, nil
	case PhaseVerifying:
		if !validPurgedReceipt(current, current.Status) {
			return Cleanup{}, false, fmt.Errorf("%w: absence scan requires a purged API receipt", ErrConflict)
		}
		generation, err := s.requestPoolScan(ctx, current.Spec.Target)
		if err != nil {
			return Cleanup{}, false, err
		}
		next := current.Status
		next.Phase = PhaseConfirmingAbsence
		next.AbsenceProof = &AbsenceProof{
			RequestID:          absenceRequestID(current, generation),
			PoolName:           current.Spec.Target.PoolName,
			PoolUID:            current.Spec.Target.PoolUID,
			RequiredGeneration: generation,
		}
		if err := s.UpdateStatus(ctx, current, next); err != nil {
			return Cleanup{}, false, err
		}
		current, err = s.Get(ctx, current.Spec.Authority)
		return current, false, err
	case PhaseConfirmingAbsence:
		if !validPurgedReceipt(current, current.Status) {
			return Cleanup{}, false, fmt.Errorf("%w: absence confirmation requires a purged API receipt", ErrConflict)
		}
	default:
		return Cleanup{}, false, fmt.Errorf("%w: cleanup is phase=%q", ErrConflict, current.Status.Phase)
	}

	pool, err := s.Client.Resource(volumeapi.PoolResource).Get(ctx, current.Spec.Target.PoolName, metav1.GetOptions{})
	if err != nil {
		return Cleanup{}, false, err
	}
	if string(pool.GetUID()) != current.Spec.Target.PoolUID || !hasFinalizer(pool, volumeapi.PoolProtectionFinalizer) {
		return Cleanup{}, false, fmt.Errorf("%w: cleanup Pool identity or protection changed", ErrConflict)
	}
	proof := current.Status.AbsenceProof
	if !validAbsenceFence(current.Spec.Target, proof) {
		return Cleanup{}, false, fmt.Errorf("%w: cleanup absence fence is invalid", ErrConflict)
	}
	observedGeneration, _, err := unstructured.NestedInt64(pool.Object, "status", "observedGeneration")
	if err != nil {
		return Cleanup{}, false, err
	}
	if observedGeneration != pool.GetGeneration() || observedGeneration < proof.RequiredGeneration {
		return current, false, nil
	}
	inventory, found, err := volumeapi.PoolInventoryFrom(pool)
	if err != nil {
		return Cleanup{}, false, err
	}
	if !found || !inventory.Valid || inventory.Truncated || inventory.Message != "" {
		return current, false, nil
	}
	poolNode, found, err := unstructured.NestedString(pool.Object, "spec", "nodeName")
	if err != nil {
		return Cleanup{}, false, err
	}
	if !found || !inventoryProvesAbsence(inventory, pool.GetName(), string(pool.GetUID()), poolNode, current.Spec.Target) {
		return current, false, nil
	}

	next := current.Status
	next.Phase = PhaseCompleted
	next.AbsenceProof = &AbsenceProof{
		RequestID:          proof.RequestID,
		PoolName:           proof.PoolName,
		PoolUID:            proof.PoolUID,
		RequiredGeneration: proof.RequiredGeneration,
		ObservedGeneration: observedGeneration,
		Valid:              true,
		Complete:           true,
		Absent:             true,
		ConfirmedAt:        s.now().Format(time.RFC3339Nano),
	}
	next.SettledAt = s.now().Format(time.RFC3339Nano)
	if err := s.UpdateStatus(ctx, current, next); err != nil {
		return Cleanup{}, false, err
	}
	current, err = s.Get(ctx, current.Spec.Authority)
	if err != nil {
		return Cleanup{}, false, err
	}
	return current, current.Status.Phase == PhaseCompleted, nil
}

func inventoryProvesAbsence(inventory volumeapi.PoolInventory, poolName, poolUID, nodeName string, target CopyIdentity) bool {
	for _, observed := range inventory.Copies {
		if observed.Marker == "" || observed.Problem != "" || observed.Identity == nil || observed.Identity.Validate() != nil {
			return false
		}
		identity := *observed.Identity
		if identity.InstallationID != target.InstallationID || identity.PoolName != poolName || identity.PoolUID != poolUID || identity.NodeName != nodeName ||
			observed.Marker != "placement-"+identity.CopyID+".json" || observed.Published && !observed.Present {
			return false
		}
		if reflect.DeepEqual(identity, target) && (observed.Present || observed.Published) {
			return false
		}
	}
	return true
}

func (s *Store) requestPoolScan(ctx context.Context, target CopyIdentity) (int64, error) {
	var generation int64
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		resource := s.Client.Resource(volumeapi.PoolResource)
		pool, err := resource.Get(ctx, target.PoolName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if string(pool.GetUID()) != target.PoolUID || !hasFinalizer(pool, volumeapi.PoolProtectionFinalizer) {
			return fmt.Errorf("%w: cleanup Pool identity or protection changed", ErrConflict)
		}
		nodeName, _, err := unstructured.NestedString(pool.Object, "spec", "nodeName")
		if err != nil || nodeName != target.NodeName {
			return fmt.Errorf("%w: cleanup Pool node changed", ErrConflict)
		}
		epoch, found, err := unstructured.NestedInt64(pool.Object, "spec", "scanEpoch")
		if err != nil {
			return err
		}
		if !found {
			epoch = 0
		}
		if epoch == int64(^uint64(0)>>1) {
			return fmt.Errorf("%w: Pool scan epoch is exhausted", ErrConflict)
		}
		if err := unstructured.SetNestedField(pool.Object, epoch+1, "spec", "scanEpoch"); err != nil {
			return err
		}
		updated, err := resource.Update(ctx, pool, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		if updated.GetGeneration() <= 0 {
			return fmt.Errorf("%w: Pool scan request returned no generation", ErrConflict)
		}
		generation = updated.GetGeneration()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("request post-receipt Pool scan: %w", err)
	}
	return generation, nil
}

func absenceRequestID(cleanup Cleanup, generation int64) string {
	encoded := cleanup.Spec.Authority.UID + "\x00" + cleanup.Spec.OperationID + "\x00" + fmt.Sprint(generation)
	sum := sha256.Sum256([]byte(encoded))
	return "scan-" + hex.EncodeToString(sum[:16])
}
