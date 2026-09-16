package cleanupapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"

	"github.com/project-jelly/ShiftPV/src/kubernetes/volumeapi"
	"github.com/project-jelly/ShiftPV/src/volume"
)

var (
	ErrConflict = errors.New("cleanup journal state precondition failed")
	// ErrNoJournal reports that the exact parent carries no cleanup journal.
	ErrNoJournal = errors.New("cleanup journal not found")
)

const (
	PhasePending           = "Pending"
	PhaseRunning           = "Running"
	PhaseVerifying         = "Verifying"
	PhaseConfirmingAbsence = "ConfirmingAbsence"
	PhaseCompleted         = "Completed"
	PhaseNeedsReview       = "NeedsReview"
)

type CopyIdentity = volume.CopyIdentity

// Authority identifies the exact durable parent that owns a cleanup journal.
// The parent's UID, rather than its reusable name, is the authority boundary.
type Authority struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	UID  string `json:"uid"`
}

type Spec struct {
	OperationID string       `json:"operationID"`
	Target      CopyIdentity `json:"target"`
	Reason      string       `json:"reason"`
	Authority   Authority    `json:"authority"`
}

type Executor struct {
	JobName  string `json:"jobName"`
	JobUID   string `json:"jobUID"`
	PodUID   string `json:"podUID,omitempty"`
	NodeName string `json:"nodeName"`
}

type Receipt struct {
	OperationID        string `json:"operationID"`
	ExecutorUID        string `json:"executorUID"`
	ObservedAt         string `json:"observedAt"`
	Retired            bool   `json:"retired"`
	Purged             bool   `json:"purged"`
	LocalReceiptDigest string `json:"localReceiptDigest,omitempty"`
}

// AbsenceProof records the generation fence and the later exact negative
// observation used to close cleanup. ConfirmedAt is diagnostic only; the
// generation fence, validity, completeness, and exact absence are authority.
type AbsenceProof struct {
	RequestID          string `json:"requestID"`
	PoolName           string `json:"poolName"`
	PoolUID            string `json:"poolUID"`
	RequiredGeneration int64  `json:"requiredGeneration"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	Valid              bool   `json:"valid,omitempty"`
	Complete           bool   `json:"complete,omitempty"`
	Absent             bool   `json:"absent,omitempty"`
	ConfirmedAt        string `json:"confirmedAt,omitempty"`
}

type Status struct {
	Phase              string        `json:"phase,omitempty"`
	ObservedGeneration int64         `json:"observedGeneration,omitempty"`
	Reason             string        `json:"reason,omitempty"`
	Message            string        `json:"message,omitempty"`
	LastTransitionTime string        `json:"lastTransitionTime,omitempty"`
	Executor           *Executor     `json:"executor,omitempty"`
	Receipt            *Receipt      `json:"receipt,omitempty"`
	AbsenceProof       *AbsenceProof `json:"absenceProof,omitempty"`
	SettledAt          string        `json:"settledAt,omitempty"`
}

// Cleanup is a synthesized view of a journal embedded at status.cleanup on an
// exact ShiftPVVolume or ShiftPVMove. UID is the parent's identity; Name is only
// a deterministic executor name component.
type Cleanup struct {
	Name   string
	UID    string
	Spec   Spec
	Status Status
}

type journal struct {
	Spec   Spec   `json:"spec"`
	Status Status `json:"status"`
}

type Store struct {
	Client dynamic.Interface
	Now    func() time.Time
}

func cleanupName(target CopyIdentity) string {
	encoded, _ := json.Marshal(target)
	sum := sha256.Sum256(encoded)
	return "shiftpv-cleanup-" + hex.EncodeToString(sum[:16])
}

func (a Authority) Validate() error {
	if !volume.ValidObjectName(a.Name) || !volume.ValidIdentityToken(a.UID) {
		return fmt.Errorf("invalid cleanup parent identity")
	}
	switch a.Kind {
	case "ShiftPVVolume", "ShiftPVMove":
		return nil
	default:
		return fmt.Errorf("invalid cleanup authority %q", a.Kind)
	}
}

func (s Spec) Validate() error {
	if !volume.ValidIdentityToken(s.OperationID) || s.Target.Validate() != nil {
		return fmt.Errorf("invalid cleanup identity")
	}
	if err := s.Authority.Validate(); err != nil {
		return err
	}
	switch s.Reason {
	case "VolumeDelete":
		if s.Authority.Kind != "ShiftPVVolume" || s.Authority.Name != s.Target.VolumeID || s.Authority.UID != s.Target.VolumeUID {
			return fmt.Errorf("volume cleanup authority does not match the target")
		}
	case "MoveSource", "MoveRollback":
		if s.Authority.Kind != "ShiftPVMove" {
			return fmt.Errorf("move cleanup requires ShiftPVMove authority")
		}
	default:
		return fmt.Errorf("invalid cleanup reason %q", s.Reason)
	}
	return nil
}

func (s *Store) Ensure(ctx context.Context, spec Spec) (Cleanup, error) {
	if err := s.validate(); err != nil {
		return Cleanup{}, err
	}
	if err := spec.Validate(); err != nil {
		return Cleanup{}, err
	}
	var result Cleanup
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		resource, finalizer, err := s.parentResource(spec.Authority)
		if err != nil {
			return err
		}
		object, err := resource.Get(ctx, spec.Authority.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := validateParent(object, spec.Authority, finalizer); err != nil {
			return err
		}
		current, found, err := journalFromParent(object)
		if err != nil {
			return err
		}
		if found {
			if !reflect.DeepEqual(current.Spec, spec) {
				return fmt.Errorf("%w: parent cleanup slot is bound to another intent", ErrConflict)
			}
			result = current
			return nil
		}
		status := Status{
			Phase:              PhasePending,
			ObservedGeneration: object.GetGeneration(),
			LastTransitionTime: s.now().Format(time.RFC3339Nano),
		}
		if err := setJournal(object, journal{Spec: spec, Status: status}); err != nil {
			return err
		}
		updated, err := resource.UpdateStatus(ctx, object, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		result, _, err = journalFromParent(updated)
		return err
	})
	if err != nil {
		return Cleanup{}, fmt.Errorf("ensure parent cleanup journal: %w", err)
	}
	return result, nil
}

// Get reads status.cleanup from the exact parent identity. A terminating
// parent remains usable while its ShiftPV protection finalizer is present.
func (s *Store) Get(ctx context.Context, authority Authority) (Cleanup, error) {
	if err := s.validate(); err != nil {
		return Cleanup{}, err
	}
	resource, finalizer, err := s.parentResource(authority)
	if err != nil {
		return Cleanup{}, err
	}
	object, err := resource.Get(ctx, authority.Name, metav1.GetOptions{})
	if err != nil {
		return Cleanup{}, err
	}
	if err := validateParent(object, authority, finalizer); err != nil {
		return Cleanup{}, err
	}
	cleanup, found, err := journalFromParent(object)
	if err != nil {
		return Cleanup{}, err
	}
	if !found {
		return Cleanup{}, fmt.Errorf("%w: %s %q", ErrNoJournal, authority.Kind, authority.Name)
	}
	return cleanup, nil
}

func (s *Store) List(ctx context.Context) ([]Cleanup, error) {
	return s.list(ctx, "")
}

func (s *Store) ListForVolume(ctx context.Context, volumeID string) ([]Cleanup, error) {
	if err := volume.ValidateID(volumeID); err != nil {
		return nil, err
	}
	return s.list(ctx, volumeID)
}

func (s *Store) list(ctx context.Context, volumeID string) ([]Cleanup, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	result := make([]Cleanup, 0)
	for _, resource := range []schema.GroupVersionResource{volumeapi.VolumeResource, volumeapi.MoveResource} {
		objects, err := s.Client.Resource(resource).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for index := range objects.Items {
			cleanup, found, decodeErr := journalFromParent(&objects.Items[index])
			if decodeErr != nil {
				return nil, decodeErr
			}
			if found && (volumeID == "" || cleanup.Spec.Target.VolumeID == volumeID) {
				result = append(result, cleanup)
			}
		}
	}
	return result, nil
}

func (s *Store) UpdateStatus(ctx context.Context, expected Cleanup, next Status) error {
	if err := s.validate(); err != nil {
		return err
	}
	if expected.UID == "" || expected.Name != cleanupName(expected.Spec.Target) || expected.UID != expected.Spec.Authority.UID || expected.Spec.Validate() != nil {
		return ErrConflict
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		resource, finalizer, err := s.parentResource(expected.Spec.Authority)
		if err != nil {
			return err
		}
		object, err := resource.Get(ctx, expected.Spec.Authority.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := validateParent(object, expected.Spec.Authority, finalizer); err != nil {
			return err
		}
		current, found, err := journalFromParent(object)
		if err != nil {
			return err
		}
		if !found || current.UID != expected.UID || current.Name != expected.Name || !reflect.DeepEqual(current.Spec, expected.Spec) {
			return ErrConflict
		}
		if next.ObservedGeneration == 0 {
			next.ObservedGeneration = object.GetGeneration()
		}
		if next.LastTransitionTime == "" {
			if next.Phase == current.Status.Phase && current.Status.LastTransitionTime != "" {
				next.LastTransitionTime = current.Status.LastTransitionTime
			} else {
				next.LastTransitionTime = s.now().Format(time.RFC3339Nano)
			}
		}
		if reflect.DeepEqual(current.Status, next) {
			return nil
		}
		if err := validateTransition(current, next); err != nil {
			return err
		}
		if err := setJournal(object, journal{Spec: current.Spec, Status: next}); err != nil {
			return err
		}
		_, err = resource.UpdateStatus(ctx, object, metav1.UpdateOptions{})
		return err
	})
}

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

func (s *Store) parentResource(authority Authority) (dynamic.ResourceInterface, string, error) {
	if err := authority.Validate(); err != nil {
		return nil, "", err
	}
	switch authority.Kind {
	case "ShiftPVVolume":
		return s.Client.Resource(volumeapi.VolumeResource), volumeapi.VolumeProtectionFinalizer, nil
	case "ShiftPVMove":
		return s.Client.Resource(volumeapi.MoveResource), volumeapi.MoveProtectionFinalizer, nil
	default:
		return nil, "", fmt.Errorf("invalid cleanup authority %q", authority.Kind)
	}
}

func validateParent(object *unstructured.Unstructured, authority Authority, finalizer string) error {
	if object == nil || string(object.GetUID()) != authority.UID || object.GetName() != authority.Name {
		return fmt.Errorf("%w: cleanup parent identity changed", ErrConflict)
	}
	if hasFinalizer(object, finalizer) {
		return nil
	}
	return fmt.Errorf("%w: cleanup parent lacks %q", ErrConflict, finalizer)
}

func hasFinalizer(object *unstructured.Unstructured, finalizer string) bool {
	if object == nil {
		return false
	}
	for _, current := range object.GetFinalizers() {
		if current == finalizer {
			return true
		}
	}
	return false
}

func journalFromParent(object *unstructured.Unstructured) (Cleanup, bool, error) {
	value, found, err := unstructured.NestedMap(object.Object, "status", "cleanup")
	if err != nil || !found {
		return Cleanup{}, found, err
	}
	var stored journal
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(value, &stored); err != nil {
		return Cleanup{}, true, fmt.Errorf("decode cleanup journal on %s %q: %w", object.GetKind(), object.GetName(), err)
	}
	if err := stored.Spec.Validate(); err != nil {
		return Cleanup{}, true, err
	}
	if stored.Spec.Authority.Kind != object.GetKind() || stored.Spec.Authority.Name != object.GetName() || stored.Spec.Authority.UID != string(object.GetUID()) {
		return Cleanup{}, true, fmt.Errorf("%w: embedded cleanup authority does not match its parent", ErrConflict)
	}
	return Cleanup{
		Name: cleanupName(stored.Spec.Target), UID: string(object.GetUID()),
		Spec: stored.Spec, Status: stored.Status,
	}, true, nil
}

func setJournal(object *unstructured.Unstructured, stored journal) error {
	encoded, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&stored)
	if err != nil {
		return err
	}
	return unstructured.SetNestedMap(object.Object, encoded, "status", "cleanup")
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Store) validate() error {
	if s == nil || s.Client == nil {
		return fmt.Errorf("Kubernetes dynamic client is required")
	}
	return nil
}
