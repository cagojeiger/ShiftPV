package cleanupcontroller

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/cleanupapi"
	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/pool/capacity"
	"github.com/cagojeiger/ShiftPV/src/volume"
)

type Operator interface {
	Reclaim(context.Context, cleanupapi.Cleanup, *cleanupapi.Store) (cleanupapi.Cleanup, error)
}

type Inventory interface {
	ListPools(context.Context) ([]volumeapi.Pool, error)
	ListVolumes(context.Context) (map[string]volumeapi.State, error)
	ListMoves(context.Context) ([]volumeapi.Move, error)
}

type Reconciler struct {
	Store     *cleanupapi.Store
	Operator  Operator
	Client    kubernetes.Interface
	Namespace string
	Interval  time.Duration
	Now       func() time.Time
	Inventory Inventory
}

func (r *Reconciler) Run(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	for {
		if err := r.ReconcileAll(ctx); err != nil && !errors.Is(err, context.Canceled) {
			klog.Errorf("reconcile cleanup contracts: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (r *Reconciler) ReconcileAll(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	var snapshot *orphanSnapshot
	if r.Inventory != nil {
		observed, err := r.orphanSnapshot(ctx)
		if err != nil {
			return err
		}
		snapshot = &observed
		if err := r.discover(ctx, observed); err != nil {
			return err
		}
	}
	requests, err := r.Store.List(ctx)
	if err != nil {
		return err
	}
	var reconcileErrors []error
	for _, request := range requests {
		if request.Status.Phase == cleanupapi.PhaseCompleted {
			continue
		}
		if !request.Spec.Approved {
			if request.Status.Phase != cleanupapi.PhaseNeedsReview {
				err := r.review(ctx, request, "ApprovalRequired", "data is preserved; approve only after restoring a valid cleanup authority")
				if err != nil {
					reconcileErrors = append(reconcileErrors, fmt.Errorf("preserve unapproved cleanup %s: %w", request.Name, err))
				}
			}
			continue
		}
		if request.Spec.Reason == "OrphanReclaim" && (request.Status.Phase == "" || request.Status.Phase == cleanupapi.PhasePending || request.Status.Phase == cleanupapi.PhaseNeedsReview) {
			if request.Status.Phase == cleanupapi.PhaseNeedsReview && request.Status.Executor != nil {
				// A bound executor requires explicit operator recovery. Its immutable
				// identity and review reason remain the evidence for that action.
				continue
			}
			if snapshot == nil {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("re-evaluate approved orphan %s: orphan inventory is not configured", request.Name))
				continue
			}
			ready, reason, message := snapshot.classify(request.Spec.Target, request.Spec.ReservationUID, r.now())
			if !ready {
				if err := r.review(ctx, request, reason, message); err != nil {
					reconcileErrors = append(reconcileErrors, fmt.Errorf("preserve approved orphan %s: %w", request.Name, err))
				}
				continue
			}
			if request.Status.Phase == cleanupapi.PhaseNeedsReview {
				if err := r.Store.UpdateStatus(ctx, request.Name, request.UID, cleanupapi.Status{Phase: cleanupapi.PhasePending, Reason: "ApprovalAccepted", Message: "the exact orphan is unreferenced and ready for cleanup"}); err != nil {
					reconcileErrors = append(reconcileErrors, fmt.Errorf("resume approved orphan %s: %w", request.Name, err))
					continue
				}
				request, err = r.Store.Get(ctx, request.Name)
				if err != nil {
					reconcileErrors = append(reconcileErrors, fmt.Errorf("read resumed orphan %s: %w", request.Name, err))
					continue
				}
			}
		}
		if request.Status.Phase == cleanupapi.PhaseNeedsReview {
			continue
		}
		if err := r.Reconcile(ctx, request); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("cleanup %s: %w", request.Name, err))
		}
	}
	return errors.Join(reconcileErrors...)
}

type orphanSnapshot struct {
	pools        []volumeapi.Pool
	volumes      map[string]volumeapi.State
	moves        []volumeapi.Move
	volumesByPV  map[string]struct{}
	reservations map[string]corev1.ConfigMap
}

func (r *Reconciler) orphanSnapshot(ctx context.Context) (orphanSnapshot, error) {
	if r.Inventory == nil || r.Client == nil || r.Namespace == "" {
		return orphanSnapshot{}, fmt.Errorf("orphan inventory is not configured")
	}
	pools, err := r.Inventory.ListPools(ctx)
	if err != nil {
		return orphanSnapshot{}, err
	}
	volumes, err := r.Inventory.ListVolumes(ctx)
	if err != nil {
		return orphanSnapshot{}, err
	}
	moves, err := r.Inventory.ListMoves(ctx)
	if err != nil {
		return orphanSnapshot{}, err
	}
	persistentVolumes, err := r.Client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return orphanSnapshot{}, err
	}
	reservations, err := r.Client.CoreV1().ConfigMaps(r.Namespace).List(ctx, metav1.ListOptions{LabelSelector: capacity.ReservationSelector})
	if err != nil {
		return orphanSnapshot{}, err
	}
	snapshot := orphanSnapshot{
		pools: pools, volumes: volumes, moves: moves,
		volumesByPV: make(map[string]struct{}), reservations: make(map[string]corev1.ConfigMap),
	}
	for _, persistentVolume := range persistentVolumes.Items {
		if persistentVolume.Spec.CSI != nil && persistentVolume.Spec.CSI.Driver == "csi.shiftpv.io" {
			snapshot.volumesByPV[persistentVolume.Spec.CSI.VolumeHandle] = struct{}{}
		}
	}
	for _, reservation := range reservations.Items {
		snapshot.reservations[reservation.Name] = reservation
	}
	return snapshot, nil
}

func (s orphanSnapshot) classify(target volume.CopyIdentity, reservationUID string, now time.Time) (bool, string, string) {
	authority := volumeapi.ClassifyCopyAuthority(s.volumes, target)
	if authority == volumeapi.CopyAuthorityCurrent || authority == volumeapi.CopyAuthorityUncertain {
		return false, "VolumeAuthorityPresent", "data is preserved while a ShiftPVVolume incarnation can still own this copy; restore or retire that lifecycle first"
	}
	if _, exists := s.volumesByPV[target.VolumeID]; exists && authority != volumeapi.CopyAuthoritySuperseded {
		return false, "PersistentVolumePresent", "data is preserved while a PersistentVolume still references this volume; remove or restore the PV authority and wait for re-evaluation"
	}
	for _, move := range s.moves {
		if move.Status.Phase == "Succeeded" || move.Status.RecoveryPhase == "Recovered" {
			continue
		}
		for _, candidate := range []*volume.CopyIdentity{move.Status.SourceCopy, move.Status.IncomingCopy, move.Status.DestinationCopy} {
			if candidate != nil && *candidate == target {
				return false, "MoveAuthorityPresent", "data is preserved while a non-succeeded ShiftPVMove references this copy; resolve the Move and wait for re-evaluation"
			}
		}
	}
	var pool *volumeapi.Pool
	for index := range s.pools {
		candidate := &s.pools[index]
		if candidate.Name == target.PoolName && candidate.UID == target.PoolUID && candidate.NodeName == target.NodeName {
			pool = candidate
			break
		}
	}
	if pool == nil {
		return false, "PoolIdentityUnavailable", "data is preserved until the exact Pool incarnation is registered again"
	}
	if ready, reason := pool.ReadyAt(now, volumeapi.DefaultPoolReadinessStaleAfter); !ready {
		return false, "PoolUnavailable", "data is preserved until the exact Pool is ready again: " + reason
	}
	if pool.Status.Inventory == nil || !pool.Status.Inventory.Valid {
		return false, "ObservationUnavailable", "data is preserved until node inventory succeeds and the exact copy can be re-evaluated"
	}
	if pool.Status.Inventory.ObservedAt.IsZero() || now.Before(pool.Status.Inventory.ObservedAt.Time) || now.Sub(pool.Status.Inventory.ObservedAt.Time) > volumeapi.DefaultPoolReadinessStaleAfter {
		return false, "ObservationStale", "data is preserved until the exact Pool inventory is observed again"
	}
	observed := false
	for _, candidate := range pool.Status.Inventory.Copies {
		if candidate.Identity == nil || *candidate.Identity != target {
			continue
		}
		observed = true
		if candidate.Problem != "" {
			return false, "CopyIdentityProblem", "data is preserved until the node observation is clean: " + candidate.Problem
		}
		if !candidate.Present {
			return false, "CopyNotObserved", "absence is not deletion evidence; restore the original filesystem or resolve the stale review request"
		}
		if candidate.Published {
			return false, "CopyMounted", "data is preserved while kubelet has a published mount reference; unpublish the workload and wait for re-evaluation"
		}
	}
	if !observed {
		if pool.Status.Inventory.Truncated {
			return false, "ObservationTruncated", "data is preserved because the bounded inventory did not reach this copy"
		}
		return false, "CopyNotObserved", "absence is not deletion evidence; restore the original filesystem or resolve the stale review request"
	}
	reservation, reservationExists := s.reservations[target.VolumeID]
	if authority == volumeapi.CopyAuthoritySuperseded {
		if reservationUID != "" {
			return false, "LiveReservationOwnershipConflict", "data is preserved because cleanup cannot own the active volume reservation"
		}
		if !reservationExists {
			return false, "LiveReservationMissing", "data is preserved until the active volume capacity reservation is restored"
		}
		if reservation.Labels["app.kubernetes.io/name"] != "shiftpv" || reservation.Labels["app.kubernetes.io/component"] != "volume-reservation" ||
			reservation.Data["volumeID"] != target.VolumeID || reservation.Data["volumeUID"] != target.VolumeUID {
			return false, "LiveReservationIdentityChanged", "data is preserved because the active volume capacity reservation identity is invalid"
		}
	} else if reservationUID == "" {
		if reservationExists {
			return false, "ReservationIdentityUnknown", "data is preserved because a reservation exists without the cleanup contract owning its UID"
		}
	} else if reservationExists && (string(reservation.UID) != reservationUID || reservation.Data["volumeID"] != target.VolumeID || reservation.Data["volumeUID"] != target.VolumeUID) {
		return false, "ReservationIdentityChanged", "data is preserved because the capacity reservation belongs to another identity"
	}
	return true, "OrphanReady", "the exact copy is present, unmounted, unreferenced, and safe for an explicitly approved cleanup"
}

func (r *Reconciler) review(ctx context.Context, request cleanupapi.Cleanup, reason, message string) error {
	if request.Status.Phase == cleanupapi.PhaseNeedsReview && request.Status.Reason == reason && request.Status.Message == message {
		return nil
	}
	return r.Store.UpdateStatus(ctx, request.Name, request.UID, cleanupapi.Status{
		Phase: cleanupapi.PhaseNeedsReview, Reason: reason, Message: message,
		Executor: request.Status.Executor, Receipt: request.Status.Receipt, SettledAt: request.Status.SettledAt,
	})
}

// Discover records exact but unreferenced copies as review-only obligations.
// It never grants deletion authority from absence or age alone.
func (r *Reconciler) Discover(ctx context.Context) error {
	if r.Inventory == nil {
		return nil
	}
	snapshot, err := r.orphanSnapshot(ctx)
	if err != nil {
		return err
	}
	return r.discover(ctx, snapshot)
}

func (r *Reconciler) discover(ctx context.Context, snapshot orphanSnapshot) error {
	requests, err := r.Store.List(ctx)
	if err != nil {
		return err
	}
	referenced := map[volume.CopyIdentity]struct{}{}
	for _, state := range snapshot.volumes {
		if state.CurrentCopy != nil {
			referenced[*state.CurrentCopy] = struct{}{}
		}
	}
	for _, move := range snapshot.moves {
		state, active := snapshot.volumes[move.Spec.VolumeID]
		if !active || state.ActiveMove != move.Name {
			continue
		}
		for _, identity := range []*volume.CopyIdentity{move.Status.SourceCopy, move.Status.IncomingCopy, move.Status.DestinationCopy} {
			if identity != nil {
				referenced[*identity] = struct{}{}
			}
		}
	}
	requestsByTarget := make(map[volume.CopyIdentity]cleanupapi.Cleanup, len(requests))
	for _, request := range requests {
		requestsByTarget[request.Spec.Target] = request
		if request.Spec.Reason != "OrphanReclaim" && request.Status.Phase != cleanupapi.PhaseCompleted {
			referenced[request.Spec.Target] = struct{}{}
		}
	}
	for _, request := range requests {
		if request.Spec.Reason != "OrphanReclaim" || request.Spec.Approved || request.Status.Phase == cleanupapi.PhaseCompleted || request.Status.Executor != nil {
			continue
		}
		_, reason, message := snapshot.classify(request.Spec.Target, request.Spec.ReservationUID, r.now())
		if err := r.review(ctx, request, reason, message); err != nil {
			return err
		}
	}
	for _, pool := range snapshot.pools {
		if pool.Status.Inventory == nil {
			continue
		}
		for _, observed := range pool.Status.Inventory.Copies {
			if observed.Identity == nil || !observed.Present || observed.Problem != "" || observed.Identity.Validate() != nil {
				continue
			}
			identity := *observed.Identity
			if identity.PoolName != pool.Name || identity.PoolUID != pool.UID || identity.NodeName != pool.NodeName {
				continue
			}
			if _, exists := referenced[identity]; exists {
				continue
			}
			if _, exists := snapshot.volumesByPV[identity.VolumeID]; exists &&
				volumeapi.ClassifyCopyAuthority(snapshot.volumes, identity) != volumeapi.CopyAuthoritySuperseded {
				continue
			}
			if request, exists := requestsByTarget[identity]; exists {
				if request.Status.Phase == cleanupapi.PhaseCompleted {
					if !observedAfterSettlement(pool, request.Status.SettledAt) {
						continue
					}
					if err := r.review(ctx, request, "CopyReappeared", "the exact copy was observed after its cleanup receipt settled; data is preserved and a new cleanup requires operator recovery"); err != nil {
						return err
					}
				}
				continue
			}
			reservationUID := ""
			authority := volumeapi.ClassifyCopyAuthority(snapshot.volumes, identity)
			if reservation, exists := snapshot.reservations[identity.VolumeID]; exists &&
				authority != volumeapi.CopyAuthoritySuperseded &&
				reservation.Labels["app.kubernetes.io/name"] == "shiftpv" && reservation.Labels["app.kubernetes.io/component"] == "volume-reservation" &&
				reservation.Data["volumeID"] == identity.VolumeID && reservation.Data["volumeUID"] == identity.VolumeUID {
				reservationUID = string(reservation.UID)
			}
			digest := sha256.Sum256([]byte(cleanupapi.Name(identity)))
			request, err := r.Store.Ensure(ctx, cleanupapi.Spec{
				OperationID: fmt.Sprintf("review-%x", digest[:16]), Target: identity, Reason: "OrphanReclaim", ReservationUID: reservationUID, Approved: false,
				Authority: cleanupapi.Authority{Kind: "Namespace", Name: "kube-system", UID: identity.InstallationID},
			})
			if err != nil {
				return err
			}
			if request.Status.Phase == "" || request.Status.Phase == cleanupapi.PhasePending {
				_, reason, message := snapshot.classify(identity, reservationUID, r.now())
				if reason == "OrphanReady" {
					reason = "OrphanPreserved"
					message = "exact copy is unreferenced and preserved; set spec.approved=true after review to authorize cleanup"
				}
				if err := r.Store.UpdateStatus(ctx, request.Name, request.UID, cleanupapi.Status{Phase: cleanupapi.PhaseNeedsReview, Reason: reason, Message: message}); err != nil {
					return err
				}
			}
			referenced[identity] = struct{}{}
		}
	}
	return nil
}

func observedAfterSettlement(pool volumeapi.Pool, settledAt string) bool {
	if pool.Status.Inventory == nil || pool.Status.Inventory.ObservedAt.IsZero() {
		return false
	}
	settled, err := time.Parse(time.RFC3339Nano, settledAt)
	return err == nil && pool.Status.Inventory.ObservedAt.Time.After(settled)
}

func (r *Reconciler) Reconcile(ctx context.Context, request cleanupapi.Cleanup) error {
	updated, err := r.Operator.Reclaim(ctx, request, r.Store)
	if err != nil {
		return err
	}
	request = updated
	if request.Status.Phase == cleanupapi.PhaseCompleted {
		return nil
	}
	if request.Status.Phase != cleanupapi.PhaseVerifying || request.Status.Executor == nil || request.Status.Receipt == nil ||
		request.Status.Receipt.OperationID != request.Spec.OperationID || request.Status.Receipt.ExecutorUID != request.Status.Executor.JobUID ||
		!request.Status.Receipt.Retired || !request.Status.Receipt.Purged {
		return fmt.Errorf("cleanup receipt is incomplete")
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	if request.Spec.Reason == "OrphanReclaim" && request.Spec.ReservationUID != "" {
		if err := r.releaseReservation(ctx, request); err != nil {
			return err
		}
	}
	return r.Store.UpdateStatus(ctx, request.Name, request.UID, cleanupapi.Status{
		Phase: cleanupapi.PhaseCompleted, Executor: request.Status.Executor, Receipt: request.Status.Receipt,
		SettledAt: now.Format(time.RFC3339Nano),
	})
}

func (r *Reconciler) releaseReservation(ctx context.Context, request cleanupapi.Cleanup) error {
	reservations := r.Client.CoreV1().ConfigMaps(r.Namespace)
	current, err := reservations.Get(ctx, request.Spec.Target.VolumeID, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if request.Spec.ReservationUID == "" || string(current.UID) != request.Spec.ReservationUID || current.Data["volumeID"] != request.Spec.Target.VolumeID || current.Data["volumeUID"] != request.Spec.Target.VolumeUID {
		return fmt.Errorf("capacity reservation identity changed after orphan cleanup")
	}
	uid := current.UID
	if err := reservations.Delete(ctx, current.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *Reconciler) validate() error {
	if r == nil || r.Store == nil || r.Operator == nil || r.Interval <= 0 {
		return fmt.Errorf("cleanup reconciler is not configured")
	}
	if r.Inventory != nil && (r.Client == nil || r.Namespace == "") {
		return fmt.Errorf("orphan inventory requires a Kubernetes client and namespace")
	}
	return nil
}
