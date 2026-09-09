package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/lifecycle/cleanup"
)

const cleanupRetention = 7 * 24 * time.Hour

func (r *Reconciler) reconcileCleanupLifecycle(ctx context.Context) error {
	now := r.now()
	if !r.lastCleanupScan.IsZero() && now.Sub(r.lastCleanupScan) < time.Minute {
		return nil
	}
	r.lastCleanupScan = now
	scanCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return r.sweepCleanup(scanCtx)
}

func (r *Reconciler) sweepCleanup(ctx context.Context) error {
	requests, err := r.Client.CoreV1().ConfigMaps(r.Namespace).List(ctx, metav1.ListOptions{LabelSelector: cleanup.Label})
	if err != nil {
		return err
	}
	if len(requests.Items) == 0 {
		return nil
	}
	moves, err := r.Repository.ListMoves(ctx)
	if err != nil {
		return err
	}
	byName := make(map[string]volumeapi.Move, len(moves))
	for _, move := range moves {
		byName[move.Name] = move
	}
	var failures []error
	sort.Slice(requests.Items, func(i, j int) bool { return requests.Items[i].Name < requests.Items[j].Name })
	start := sort.Search(len(requests.Items), func(i int) bool { return requests.Items[i].Name > r.cleanupCursor })
	for offset := 0; offset < len(requests.Items) && offset < 32; offset++ {
		if ctx.Err() != nil {
			return errors.Join(append(failures, ctx.Err())...)
		}
		index := (start + offset) % len(requests.Items)
		cm := &requests.Items[index]
		r.cleanupCursor = cm.Name
		record, err := cleanup.Decode(cm)
		if err == nil {
			itemCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err = r.reconcileCleanupRecord(itemCtx, record, byName)
			cancel()
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("cleanup request %s: %w", cm.Name, err))
		}
	}
	return errors.Join(failures...)
}

func (r *Reconciler) reconcileCleanupRecord(ctx context.Context, record cleanup.Record, moves map[string]volumeapi.Move) error {
	if record.Completed {
		return r.retainCleanupEvidence(ctx, record, moves)
	}
	if record.CheckDone {
		if err := r.expireFinishedCheck(ctx, record); err != nil {
			return err
		}
	}
	if record.CheckID != "" && !record.CheckDone {
		return r.reconcileCleanupCheck(ctx, record, moves)
	}
	if record.CheckRequest != "" && record.CheckRequest != record.CheckID {
		if err := r.cleanupCheckAuthority(ctx, record, moves); err != nil {
			return r.cleanupReview(ctx, record, err)
		}
		if err := r.cleanupJournal().StartCheck(ctx, record.Intent, record.CheckRequest, r.HelperImage); err != nil {
			return r.cleanupReview(ctx, record, err)
		}
		return nil
	}
	if record.CheckDone {
		return nil
	}
	move, exists := moves[record.Intent.MoveName]
	if !exists || move.UID != record.Intent.MoveUID || move.Status.Phase == "Blocked" {
		return r.cleanupJournal().SetState(ctx, record.Intent, cleanup.NeedsReview, "Restore service if needed; resolve retained paths and request a read-only cleanup check")
	}
	job, err := r.Client.BatchV1().Jobs(r.Namespace).Get(ctx, record.Intent.JobName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) && record.JobUID == "" {
		return r.cleanupJournal().SetState(ctx, record.Intent, cleanup.Pending, "Waiting for Move cleanup")
	}
	if err != nil {
		return r.cleanupReview(ctx, record, err)
	}
	if err := validateCleanupJob(job, record); err != nil {
		return r.cleanupReview(ctx, record, err)
	}
	_, failed := jobFinished(job)
	if failed {
		return r.cleanupJournal().SetState(ctx, record.Intent, cleanup.NeedsReview, "Cleanup Job failed; inspect storage and request ResumeOwner")
	}
	return r.cleanupJournal().SetState(ctx, record.Intent, cleanup.Running, "Move owns cleanup and acknowledgement")
}

func (r *Reconciler) cleanupReview(ctx context.Context, record cleanup.Record, cause error) error {
	err := r.cleanupJournal().SetState(ctx, record.Intent, cleanup.NeedsReview, cause.Error())
	return errors.Join(cause, err)
}

func terminalCleanupMove(move volumeapi.Move) bool {
	return move.Status.Phase == "Succeeded" || (move.Status.Phase == "Blocked" && move.Status.RecoveryPhase == recoveryRecovered)
}

func (r *Reconciler) cleanupCheckAuthority(ctx context.Context, record cleanup.Record, moves map[string]volumeapi.Move) error {
	i := record.Intent
	if move, exists := moves[i.MoveName]; exists {
		if move.UID != i.MoveUID || !terminalCleanupMove(move) || cleanupIntent(move, i.PoolPath) != i {
			return fmt.Errorf("cleanup check requires the original Move to be terminal or absent")
		}
	}
	state, err := r.Repository.Get(ctx, i.VolumeID)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil && (state.Phase != volumeapi.PhaseReady || state.ActiveMove != "" || state.OwnerNode == "" || state.OwnerNode == i.SourceNode || contains(state.PublishedNodes, i.SourceNode)) {
		return fmt.Errorf("cleanup check requires an idle committed owner without source publication")
	}
	root, err := r.poolMountPath(ctx, i.SourceNode)
	if err != nil {
		return err
	}
	if root != i.PoolPath {
		return fmt.Errorf("registered source Pool path differs from cleanup intent")
	}
	node, err := r.Client.CoreV1().Nodes().Get(ctx, i.SourceNode, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if !nodeReady(node) || node.DeletionTimestamp != nil {
		return fmt.Errorf("source Node must be Ready for cleanup verification")
	}
	readyPools, err := r.Repository.ReadyPools(ctx)
	if err != nil {
		return err
	}
	ready := false
	for _, pool := range readyPools {
		if pool.NodeName == i.SourceNode && pool.MountPath == i.PoolPath {
			ready = true
		}
	}
	if !ready {
		return fmt.Errorf("source Pool must be Ready for cleanup verification")
	}
	if _, err := r.Client.BatchV1().Jobs(r.Namespace).Get(ctx, i.JobName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		if err != nil {
			return err
		}
		return fmt.Errorf("original cleanup Job must be gone before verification")
	}
	// Original and recovery workers must be quiescent; the check itself is read-only.
	original := namesFor(i.MoveName)
	recovery := recoveryNames(volumeapi.Move{Name: i.MoveName, UID: i.MoveUID})
	for _, base := range []string{original.Base, recovery.Base} {
		jobs, err := r.Client.BatchV1().Jobs(r.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "shiftpv.io/move=" + base})
		if err != nil {
			return err
		}
		if len(jobs.Items) != 0 {
			return fmt.Errorf("Move helper Jobs must be removed before cleanup verification")
		}
		pods, err := r.Client.CoreV1().Pods(r.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "shiftpv.io/move=" + base})
		if err != nil {
			return err
		}
		if len(pods.Items) != 0 {
			return fmt.Errorf("Move helper Pods must be gone before cleanup verification")
		}
	}
	return nil
}

func (r *Reconciler) retainCleanupEvidence(ctx context.Context, record cleanup.Record, moves map[string]volumeapi.Move) error {
	if record.CompletedAt.IsZero() {
		return r.cleanupJournal().SetState(ctx, record.Intent, cleanup.Pending, "")
	}
	// An acknowledgement may precede the final Move status write by an arbitrary outage.
	if move, exists := moves[record.Intent.MoveName]; exists && (move.UID != record.Intent.MoveUID || !terminalCleanupMove(move)) {
		return nil
	}
	state, err := r.Repository.Get(ctx, record.Intent.VolumeID)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil && state.ActiveMove != "" {
		return nil
	}
	for _, name := range []string{record.Intent.JobName, cleanupCheckName(record)} {
		if name == "" {
			continue
		}
		job, err := r.Client.BatchV1().Jobs(r.Namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if name == record.Intent.JobName {
			err = validateCleanupJob(job, record)
		} else {
			err = validateCleanupCheck(job, record)
		}
		if err != nil {
			return err
		}
		complete, failed := jobFinished(job)
		if !complete || failed {
			return fmt.Errorf("completed cleanup evidence Job is not successful")
		}
		if err := r.expireCleanupJob(ctx, job); err != nil {
			return err
		}
		return nil
	}
	if record.CompletedAt.After(r.now().Add(-cleanupRetention)) {
		return nil
	}
	return r.cleanupJournal().DeleteCompleted(ctx, record, r.now().Add(-cleanupRetention))
}

func jobFinished(job *batchv1.Job) (complete, failed bool) {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		if c.Type == batchv1.JobComplete {
			complete = true
		}
		if c.Type == batchv1.JobFailed {
			failed = true
		}
	}
	return
}

func (r *Reconciler) expireCleanupJob(ctx context.Context, job *batchv1.Job) error {
	if job.Spec.TTLSecondsAfterFinished != nil {
		return nil
	}
	ttl := int32(600)
	job.Spec.TTLSecondsAfterFinished = &ttl
	_, err := r.Client.BatchV1().Jobs(r.Namespace).Update(ctx, job, metav1.UpdateOptions{})
	return err
}

func (r *Reconciler) expireFinishedCheck(ctx context.Context, record cleanup.Record) error {
	job, err := r.Client.BatchV1().Jobs(r.Namespace).Get(ctx, cleanupCheckName(record), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := validateCleanupCheck(job, record); err != nil {
		return err
	}
	complete, failed := jobFinished(job)
	if !complete && !failed {
		return fmt.Errorf("recorded terminal check Job is still running")
	}
	return r.expireCleanupJob(ctx, job)
}
