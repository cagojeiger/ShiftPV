package helperpod

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/cleanupapi"
	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
)

const (
	cleanupNameLabel = "shiftpv.io/cleanup-name"
	cleanupUIDLabel  = "shiftpv.io/cleanup-uid"
)

// CleanupJournal is the parent-owned journal access Reclaim needs.
type CleanupJournal interface {
	Get(context.Context, cleanupapi.Authority) (cleanupapi.Cleanup, error)
	UpdateStatus(context.Context, cleanupapi.Cleanup, cleanupapi.Status) error
}

// Reclaim drives one approved cleanup to its receipt. The parent journal is the
// only durable truth, so the phase is re-read between steps and handed to
// triageCleanup: a cleanup that settled or was sent to review elsewhere ends
// this call wherever that is observed.
func (r *Runner) Reclaim(ctx context.Context, cleanup cleanupapi.Cleanup, store CleanupJournal) (cleanupapi.Cleanup, error) {
	if r == nil || r.Client == nil || r.Pools == nil || r.Namespace == "" || r.Image == "" || r.Timeout <= 0 || store == nil || cleanup.UID == "" || cleanup.Name == "" || cleanup.Spec.Validate() != nil {
		return cleanupapi.Cleanup{}, fmt.Errorf("cleanup runner configuration is incomplete")
	}
	if done, result, err := triageCleanup(cleanup); done {
		return result, err
	}
	pool, err := r.approvedPool(ctx, store, cleanup)
	if err != nil {
		return cleanupapi.Cleanup{}, err
	}
	cleanup, err = refreshCleanup(ctx, store, cleanup)
	if err != nil {
		return cleanupapi.Cleanup{}, err
	}
	if done, result, err := triageCleanup(cleanup); done {
		return result, err
	}
	if cleanup.Status.Phase != cleanupapi.PhaseVerifying {
		if err := r.cleanupPoolReady(pool); err != nil {
			return cleanupapi.Cleanup{}, err
		}
	}
	job := r.cleanupJob(cleanup, pool.MountPath)
	if cleanup.Status.Phase == cleanupapi.PhaseVerifying {
		return r.resumeVerifying(ctx, cleanup, store, job)
	}
	created, err := r.ensureJob(ctx, store, cleanup, job)
	if err != nil {
		return cleanupapi.Cleanup{}, err
	}
	cleanup, err = refreshCleanup(ctx, store, cleanup)
	if err != nil {
		return cleanupapi.Cleanup{}, err
	}
	if done, result, err := triageCleanup(cleanup); done {
		return result, err
	}
	cleanup, err = r.bindExecutor(ctx, store, cleanup, created)
	if err != nil {
		return cleanupapi.Cleanup{}, err
	}
	if err := r.startCleanupJob(ctx, store, cleanup, created, job); err != nil {
		return cleanupapi.Cleanup{}, err
	}
	return r.awaitReceipt(ctx, store, cleanup, created, job)
}

// triageCleanup reports whether the durable phase already decides this
// reconcile: a settled cleanup is returned as it stands, and one already under
// review is an error no executor work may follow.
func triageCleanup(current cleanupapi.Cleanup) (bool, cleanupapi.Cleanup, error) {
	switch current.Status.Phase {
	case cleanupapi.PhaseCompleted, cleanupapi.PhaseConfirmingAbsence:
		return true, current, nil
	case cleanupapi.PhaseNeedsReview:
		return true, cleanupapi.Cleanup{}, fmt.Errorf("cleanup %q needs review: %s", current.Name, current.Status.Reason)
	}
	return false, cleanupapi.Cleanup{}, nil
}

// approvedPool resolves the registered Pool the approved cleanup targets. Pool
// identity drift and lost lifecycle protection are contradictions, not transient
// failures, so they send the cleanup to review instead of retrying.
func (r *Runner) approvedPool(ctx context.Context, store CleanupJournal, cleanup cleanupapi.Cleanup) (volumeapi.Pool, error) {
	pool, err := r.Pools.PoolForIdentity(ctx, cleanup.Spec.Target.PoolName, cleanup.Spec.Target.PoolUID, cleanup.Spec.Target.NodeName)
	if err != nil {
		if errors.Is(err, volumeapi.ErrStateConflict) {
			return volumeapi.Pool{}, r.needsReview(ctx, store, cleanup, "PoolIdentityChanged", "registered Pool no longer matches the approved cleanup target")
		}
		return volumeapi.Pool{}, classifyKubernetesAPIError(err)
	}
	if pool.Name != cleanup.Spec.Target.PoolName || pool.UID != cleanup.Spec.Target.PoolUID {
		return volumeapi.Pool{}, r.needsReview(ctx, store, cleanup, "PoolIdentityChanged", "registered Pool no longer matches the approved cleanup target")
	}
	if !slices.Contains(pool.Finalizers, volumeapi.PoolProtectionFinalizer) {
		return volumeapi.Pool{}, r.needsReview(ctx, store, cleanup, "PoolProtectionChanged", "cleanup Pool no longer has lifecycle protection")
	}
	return pool, nil
}

// cleanupPoolReady defers an effect that has not started yet while its Pool is
// not currently usable. The staleness budget follows the same rule
// volumeapi.Registry.readiness() applies to placement decisions and
// volumeapi.PoolReadinessStaleAfterArgument applies to the executor argument: a
// non-positive value means DefaultPoolReadinessStaleAfter.
func (r *Runner) cleanupPoolReady(pool volumeapi.Pool) error {
	staleAfter := r.PoolReadinessStaleAfter
	if staleAfter <= 0 {
		staleAfter = volumeapi.DefaultPoolReadinessStaleAfter
	}
	if ready, reason := pool.CleanupReadyAt(time.Now(), staleAfter); !ready {
		return retryableError{err: fmt.Errorf("cleanup Pool %q is not ready: %s", pool.Name, reason)}
	}
	return nil
}

// ensureJob resolves exactly one suspended executor for the approved effect. An
// accepted create whose response was lost is reacquired by name, and a Job under
// that name that is not the approved effect sends the cleanup to review.
func (r *Runner) ensureJob(ctx context.Context, store CleanupJournal, cleanup cleanupapi.Cleanup, job *batchv1.Job) (*batchv1.Job, error) {
	jobs := r.Client.BatchV1().Jobs(r.Namespace)
	created, err := jobs.Create(ctx, job, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) || isAmbiguousKubernetesError(err) {
		createErr := err
		created, err = jobs.Get(ctx, job.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) && isAmbiguousKubernetesError(createErr) {
			return nil, fmt.Errorf("ensure cleanup Job: %w", classifyKubernetesAPIError(createErr))
		}
		if err == nil && !sameCleanupJob(created, job) {
			return nil, r.needsReview(ctx, store, cleanup, "JobIdentityChanged", fmt.Sprintf("cleanup Job %q differs from the approved effect", job.Name))
		}
	}
	if err != nil {
		return nil, fmt.Errorf("ensure cleanup Job: %w", classifyKubernetesAPIError(err))
	}
	if created.UID == "" {
		return nil, r.needsReview(ctx, store, cleanup, "ExecutorIdentityMissing", "cleanup Job has no UID")
	}
	return created, nil
}

// bindExecutor makes the resolved Job UID durable before it may run. A Job that
// is already running while the journal has not bound it, or a bound executor
// that is not this one, is an identity contradiction.
func (r *Runner) bindExecutor(ctx context.Context, store CleanupJournal, cleanup cleanupapi.Cleanup, created *batchv1.Job) (cleanupapi.Cleanup, error) {
	if (cleanup.Status.Phase == "" || cleanup.Status.Phase == cleanupapi.PhasePending) &&
		(created.Spec.Suspend == nil || !*created.Spec.Suspend) {
		return cleanupapi.Cleanup{}, r.needsReview(ctx, store, cleanup, "ExecutorStartedBeforeBinding", "cleanup Job started before its UID was bound to durable status")
	}
	executor := &cleanupapi.Executor{JobName: created.Name, JobUID: string(created.UID), NodeName: cleanup.Spec.Target.NodeName}
	if cleanup.Status.Phase == "" || cleanup.Status.Phase == cleanupapi.PhasePending {
		if err := store.UpdateStatus(ctx, cleanup, cleanupapi.Status{Phase: cleanupapi.PhaseRunning, Executor: executor}); err != nil {
			return cleanupapi.Cleanup{}, fmt.Errorf("bind cleanup executor: %w", err)
		}
		cleanup.Status = cleanupapi.Status{Phase: cleanupapi.PhaseRunning, Executor: executor}
	} else if cleanup.Status.Executor == nil || !sameBoundExecutor(cleanup.Status.Executor, executor) {
		return cleanupapi.Cleanup{}, r.needsReview(ctx, store, cleanup, "ExecutorIdentityChanged", "cleanup executor differs from the durable cleanup status")
	}
	return cleanup, nil
}

// awaitReceipt watches the bound executor until the journal carries its receipt.
// The journal decides settlement; the Job is read only to detect an executor
// that disappeared, changed or terminated without writing one.
func (r *Runner) awaitReceipt(ctx context.Context, store CleanupJournal, cleanup cleanupapi.Cleanup, created, expected *batchv1.Job) (cleanupapi.Cleanup, error) {
	jobs := r.Client.BatchV1().Jobs(r.Namespace)
	result := cleanup
	err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, r.Timeout, true, func(pollCtx context.Context) (bool, error) {
		current, getErr := store.Get(pollCtx, cleanup.Spec.Authority)
		if getErr != nil {
			return false, getErr
		}
		if current.UID != cleanup.UID || current.Spec != cleanup.Spec {
			return false, cleanupapi.ErrConflict
		}
		switch current.Status.Phase {
		case cleanupapi.PhaseCompleted, cleanupapi.PhaseConfirmingAbsence:
			result = current
			return true, nil
		case cleanupapi.PhaseNeedsReview:
			return false, fmt.Errorf("cleanup needs review: %s", current.Status.Reason)
		}
		if current.Status.Executor == nil || current.Status.Executor.JobName != created.Name || current.Status.Executor.JobUID != string(created.UID) {
			return false, r.needsReview(pollCtx, store, current, "ExecutorIdentityChanged", "cleanup executor differs from the durable cleanup status")
		}
		currentJob, jobErr := jobs.Get(pollCtx, created.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(jobErr) {
			return false, r.needsReview(pollCtx, store, current, "ExecutorDisappeared", "cleanup Job disappeared before termination was acknowledged")
		}
		if jobErr != nil {
			return false, classifyKubernetesAPIError(jobErr)
		}
		if currentJob.UID != created.UID || !sameCleanupJob(currentJob, expected) {
			return false, r.needsReview(pollCtx, store, current, "ExecutorIdentityChanged", "cleanup Job changed before termination was acknowledged")
		}
		complete, failure, failed := cleanupJobOutcome(currentJob)
		if failed {
			return false, r.needsReview(pollCtx, store, current, "ExecutorFailed", fmt.Sprintf("cleanup Job failed: %s", failure))
		}
		if !complete {
			return false, nil
		}
		if current.Status.Phase != cleanupapi.PhaseVerifying || current.Status.Receipt == nil {
			return false, r.needsReview(pollCtx, store, current, "ReceiptMissing", "cleanup Job completed without a durable receipt")
		}
		result = current
		return true, nil
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = retryableError{err: err}
		}
		return cleanupapi.Cleanup{}, fmt.Errorf("wait for cleanup receipt: %w", err)
	}
	return result, nil
}

// cleanupJobOutcome reads one Job's terminal conditions. A true Failed condition
// anywhere in the list wins over a true Complete one, because a Job that failed
// after completing a Pod has not delivered the approved effect.
func cleanupJobOutcome(job *batchv1.Job) (complete bool, failure string, failed bool) {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			return false, condition.Message, true
		}
		if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
			complete = true
		}
	}
	return complete, "", false
}

func (r *Runner) resumeVerifying(ctx context.Context, cleanup cleanupapi.Cleanup, store CleanupJournal, expected *batchv1.Job) (cleanupapi.Cleanup, error) {
	if cleanup.Status.Executor == nil || cleanup.Status.Receipt == nil ||
		cleanup.Status.Receipt.OperationID != cleanup.Spec.OperationID ||
		cleanup.Status.Receipt.ExecutorUID != cleanup.Status.Executor.JobUID ||
		!cleanup.Status.Receipt.Retired || !cleanup.Status.Receipt.Purged {
		return cleanupapi.Cleanup{}, r.needsReview(ctx, store, cleanup, "ReceiptInvalid", "cleanup receipt does not match the durable executor and intent")
	}
	jobs := r.Client.BatchV1().Jobs(r.Namespace)
	selector := labels.Set{cleanupNameLabel: cleanup.Name, cleanupUIDLabel: cleanup.UID}.AsSelector().String()
	var result cleanupapi.Cleanup
	err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, r.Timeout, true, func(pollCtx context.Context) (bool, error) {
		current, err := refreshCleanup(pollCtx, store, cleanup)
		if err != nil {
			return false, err
		}
		if current.Status.Phase == cleanupapi.PhaseCompleted || current.Status.Phase == cleanupapi.PhaseConfirmingAbsence {
			result = current
			return true, nil
		}
		if current.Status.Phase != cleanupapi.PhaseVerifying || current.Status.Executor == nil || current.Status.Receipt == nil ||
			!reflect.DeepEqual(current.Status.Executor, cleanup.Status.Executor) || !reflect.DeepEqual(current.Status.Receipt, cleanup.Status.Receipt) {
			return false, cleanupapi.ErrConflict
		}
		job, getErr := jobs.Get(pollCtx, current.Status.Executor.JobName, metav1.GetOptions{})
		if apierrors.IsNotFound(getErr) {
			pods, listErr := r.Client.CoreV1().Pods(r.Namespace).List(pollCtx, metav1.ListOptions{LabelSelector: selector})
			if listErr != nil {
				return false, classifyKubernetesAPIError(listErr)
			}
			for index := range pods.Items {
				if ownedByJob(&pods.Items[index], types.UID(current.Status.Executor.JobUID)) {
					return false, nil
				}
			}
			result = current
			return true, nil
		}
		if getErr != nil {
			return false, classifyKubernetesAPIError(getErr)
		}
		if string(job.UID) != current.Status.Executor.JobUID || !sameCleanupJob(job, expected) {
			return false, r.needsReview(pollCtx, store, current, "ExecutorIdentityChanged", "cleanup executor differs from the durable cleanup status")
		}
		// First condition wins here, unlike cleanupJobOutcome: a receipt already
		// exists, so a Complete recorded before a later Failed still resumes.
		for _, condition := range job.Status.Conditions {
			if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
				return false, r.needsReview(pollCtx, store, current, "ExecutorFailed", fmt.Sprintf("cleanup Job failed after writing its receipt: %s", condition.Message))
			}
			if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
				result = current
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = retryableError{err: err}
		}
		return cleanupapi.Cleanup{}, fmt.Errorf("wait for cleanup executor settlement: %w", err)
	}
	return result, nil
}

func ownedByJob(pod *corev1.Pod, uid types.UID) bool {
	for _, owner := range pod.OwnerReferences {
		if owner.APIVersion == "batch/v1" && owner.Kind == "Job" && owner.UID == uid {
			return true
		}
	}
	return false
}

func refreshCleanup(ctx context.Context, store CleanupJournal, expected cleanupapi.Cleanup) (cleanupapi.Cleanup, error) {
	current, err := store.Get(ctx, expected.Spec.Authority)
	if err != nil {
		return cleanupapi.Cleanup{}, fmt.Errorf("read cleanup intent: %w", classifyKubernetesAPIError(err))
	}
	if current.UID != expected.UID || current.Spec != expected.Spec {
		return cleanupapi.Cleanup{}, cleanupapi.ErrConflict
	}
	return current, nil
}

func (r *Runner) startCleanupJob(ctx context.Context, store CleanupJournal, cleanup cleanupapi.Cleanup, created, expected *batchv1.Job) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current, err := r.Client.BatchV1().Jobs(r.Namespace).Get(ctx, created.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return r.needsReview(ctx, store, cleanup, "ExecutorDisappeared", "bound cleanup Job disappeared before execution")
		}
		if err != nil {
			return fmt.Errorf("read bound cleanup Job: %w", classifyKubernetesAPIError(err))
		}
		if current.UID != created.UID || !sameCleanupJob(current, expected) {
			return r.needsReview(ctx, store, cleanup, "ExecutorIdentityChanged", "bound cleanup Job differs from the approved effect")
		}
		if current.Spec.Suspend == nil || !*current.Spec.Suspend {
			return nil
		}
		current.Spec.Suspend = boolPtr(false)
		if _, err := r.Client.BatchV1().Jobs(r.Namespace).Update(ctx, current, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("start bound cleanup Job: %w", classifyKubernetesAPIError(err))
		}
		return nil
	})
}

func (r *Runner) needsReview(ctx context.Context, store CleanupJournal, cleanup cleanupapi.Cleanup, reason, message string) error {
	if updateErr := store.UpdateStatus(ctx, cleanup, cleanupapi.Status{
		Phase: cleanupapi.PhaseNeedsReview, Reason: reason, Message: message,
		Executor: cleanup.Status.Executor, Receipt: cleanup.Status.Receipt,
	}); updateErr != nil {
		return fmt.Errorf("mark cleanup for review after %s: %w", reason, updateErr)
	}
	return fmt.Errorf("cleanup needs review: %s: %s", reason, message)
}

func (r *Runner) cleanupJob(cleanup cleanupapi.Cleanup, poolRoot string) *batchv1.Job {
	backoff := int32(2)
	ttl := int32(600)
	suspended := true
	deadline := int64(r.Timeout.Seconds())
	if deadline < 1 {
		deadline = 1
	}
	labels := map[string]string{
		"app.kubernetes.io/name":      "shiftpv",
		"app.kubernetes.io/component": "cleanup-helper",
		cleanupNameLabel:              cleanup.Name,
		cleanupUIDLabel:               cleanup.UID,
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: cleanup.Name + "-effect", Namespace: r.Namespace, Labels: labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "shiftpv.io/v1alpha1", Kind: cleanup.Spec.Authority.Kind, Name: cleanup.Spec.Authority.Name,
				UID: types.UID(cleanup.Spec.Authority.UID), Controller: boolPtr(true), BlockOwnerDeletion: boolPtr(true),
			}},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff, TTLSecondsAfterFinished: &ttl, ActiveDeadlineSeconds: &deadline, Suspend: &suspended,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					NodeName: cleanup.Spec.Target.NodeName, ServiceAccountName: r.ServiceAccountName,
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name: "cleanup", Image: r.Image,
						Command: []string{"/shiftpv-volume-helper"},
						Args: []string{
							"cleanup", "--authority-kind=" + cleanup.Spec.Authority.Kind,
							"--authority-name=" + cleanup.Spec.Authority.Name, "--authority-uid=" + cleanup.Spec.Authority.UID,
							"--operation-id=" + cleanup.Spec.OperationID, "--namespace=" + r.Namespace,
							volumeapi.PoolReadinessStaleAfterArgument(r.PoolReadinessStaleAfter),
						},
						Env:       []corev1.EnvVar{{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"}}}},
						Resources: r.Resources,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: boolPtr(false), RunAsUser: int64Ptr(0), RunAsGroup: int64Ptr(0),
						},
						VolumeMounts: []corev1.VolumeMount{{Name: "pool", MountPath: mountPath}},
					}},
					Volumes: []corev1.Volume{{Name: "pool", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: poolRoot, Type: hostPathTypePtr(corev1.HostPathDirectory)}}}},
				},
			},
		},
	}
}

// fieldCheck is one row of an ordered comparison table: the name a mismatch is
// reported under, and the comparison itself. Rows are evaluated in order and
// stop at the first false one, so a later row may rely on an earlier row having
// fixed a length or excluded a nil.
type fieldCheck struct {
	field string
	same  func() bool
}

// firstDifference returns the name of the first row that does not hold, or ""
// when every row holds.
func firstDifference(checks []fieldCheck) string {
	for _, check := range checks {
		if !check.same() {
			return check.field
		}
	}
	return ""
}

func sameCleanupJob(current, expected *batchv1.Job) bool {
	return cleanupJobDifference(current, expected) == ""
}

// cleanupJobDifference names the first field in which a live Job departs from
// the approved cleanup effect, or returns "" when the Job is exactly that
// effect. The three tables keep the original comparison order, so the effect
// rows may index the first container and Volume: the shape rows have already
// fixed both counts.
func cleanupJobDifference(current, expected *batchv1.Job) string {
	if current == nil || expected == nil {
		return "job"
	}
	if field := firstDifference(cleanupJobIdentityChecks(current, expected)); field != "" {
		return field
	}
	if field := firstDifference(cleanupJobShapeChecks(current, expected)); field != "" {
		return field
	}
	return firstDifference(cleanupJobEffectChecks(current, expected))
}

func cleanupJobIdentityChecks(current, expected *batchv1.Job) []fieldCheck {
	return []fieldCheck{
		{"metadata.labels[" + cleanupUIDLabel + "]", func() bool {
			return current.Labels[cleanupUIDLabel] == expected.Labels[cleanupUIDLabel]
		}},
		{"metadata.labels[" + cleanupNameLabel + "]", func() bool {
			return current.Labels[cleanupNameLabel] == expected.Labels[cleanupNameLabel]
		}},
		{"metadata.deletionTimestamp", func() bool { return current.DeletionTimestamp == nil }},
		{"metadata.namespace", func() bool { return current.Namespace == expected.Namespace }},
		{"metadata.name", func() bool { return current.Name == expected.Name }},
		{"metadata.ownerReferences", func() bool {
			return reflect.DeepEqual(current.OwnerReferences, expected.OwnerReferences)
		}},
	}
}

// cleanupJobShapeChecks holds the executor to one Pod attempt of the approved
// shape: no fan-out, no alternative completion or failure handling, no foreign
// controller, and no extra container or Volume.
func cleanupJobShapeChecks(current, expected *batchv1.Job) []fieldCheck {
	currentPod, expectedPod := current.Spec.Template.Spec, expected.Spec.Template.Spec
	return []fieldCheck{
		{"spec.parallelism", func() bool { return oneOrDefault(current.Spec.Parallelism) }},
		{"spec.completions", func() bool { return oneOrDefault(current.Spec.Completions) }},
		{"spec.manualSelector", func() bool {
			return current.Spec.ManualSelector == nil || !*current.Spec.ManualSelector
		}},
		{"spec.completionMode", func() bool {
			return current.Spec.CompletionMode == nil || *current.Spec.CompletionMode == batchv1.NonIndexedCompletion
		}},
		{"spec.podFailurePolicy", func() bool { return current.Spec.PodFailurePolicy == nil }},
		{"spec.successPolicy", func() bool { return current.Spec.SuccessPolicy == nil }},
		{"spec.backoffLimitPerIndex", func() bool { return current.Spec.BackoffLimitPerIndex == nil }},
		{"spec.maxFailedIndexes", func() bool { return current.Spec.MaxFailedIndexes == nil }},
		{"spec.managedBy", func() bool {
			return current.Spec.ManagedBy == nil || *current.Spec.ManagedBy == batchv1.JobControllerName
		}},
		{"spec.template.labels[" + cleanupUIDLabel + "]", func() bool {
			return current.Spec.Template.Labels[cleanupUIDLabel] == expected.Spec.Template.Labels[cleanupUIDLabel]
		}},
		{"spec.template.labels[" + cleanupNameLabel + "]", func() bool {
			return current.Spec.Template.Labels[cleanupNameLabel] == expected.Spec.Template.Labels[cleanupNameLabel]
		}},
		{"spec.template.spec.nodeName", func() bool { return currentPod.NodeName == expectedPod.NodeName }},
		{"spec.template.spec.serviceAccountName", func() bool {
			return currentPod.ServiceAccountName == expectedPod.ServiceAccountName
		}},
		{"spec.template.spec.restartPolicy", func() bool { return currentPod.RestartPolicy == expectedPod.RestartPolicy }},
		{"spec.template.spec.hostPID", func() bool { return !currentPod.HostPID }},
		{"spec.template.spec.hostIPC", func() bool { return !currentPod.HostIPC }},
		{"spec.template.spec.hostNetwork", func() bool { return !currentPod.HostNetwork }},
		{"spec.template.spec.initContainers", func() bool { return len(currentPod.InitContainers) == 0 }},
		{"spec.template.spec.ephemeralContainers", func() bool { return len(currentPod.EphemeralContainers) == 0 }},
		{"spec.template.spec.containers", func() bool {
			return len(currentPod.Containers) == 1 && len(expectedPod.Containers) == 1
		}},
		{"spec.template.spec.volumes", func() bool {
			return len(currentPod.Volumes) == 1 && len(expectedPod.Volumes) == 1
		}},
	}
}

// cleanupJobEffectChecks compares what the single container would actually do:
// the command and its identity-bound arguments, the Pool mount, and the retry
// and deadline budget the effect was approved with.
func cleanupJobEffectChecks(current, expected *batchv1.Job) []fieldCheck {
	currentPod, expectedPod := current.Spec.Template.Spec, expected.Spec.Template.Spec
	currentContainer, expectedContainer := currentPod.Containers[0], expectedPod.Containers[0]
	return []fieldCheck{
		{"spec.template.spec.containers[0].name", func() bool { return currentContainer.Name == expectedContainer.Name }},
		{"spec.template.spec.containers[0].image", func() bool { return currentContainer.Image == expectedContainer.Image }},
		{"spec.template.spec.containers[0].command", func() bool {
			return reflect.DeepEqual(currentContainer.Command, expectedContainer.Command)
		}},
		{"spec.template.spec.containers[0].args", func() bool {
			return reflect.DeepEqual(currentContainer.Args, expectedContainer.Args)
		}},
		{"spec.template.spec.containers[0].env", func() bool {
			return reflect.DeepEqual(currentContainer.Env, expectedContainer.Env)
		}},
		{"spec.template.spec.containers[0].envFrom", func() bool { return len(currentContainer.EnvFrom) == 0 }},
		{"spec.template.spec.containers[0].resources", func() bool {
			return reflect.DeepEqual(currentContainer.Resources, expectedContainer.Resources)
		}},
		{"spec.template.spec.containers[0].securityContext", func() bool {
			return reflect.DeepEqual(currentContainer.SecurityContext, expectedContainer.SecurityContext)
		}},
		{"spec.template.spec.containers[0].volumeMounts", func() bool {
			return reflect.DeepEqual(currentContainer.VolumeMounts, expectedContainer.VolumeMounts)
		}},
		{"spec.template.spec.containers[0].volumeDevices", func() bool { return len(currentContainer.VolumeDevices) == 0 }},
		{"spec.template.spec.containers[0].lifecycle", func() bool { return currentContainer.Lifecycle == nil }},
		{"spec.template.spec.containers[0].livenessProbe", func() bool { return currentContainer.LivenessProbe == nil }},
		{"spec.template.spec.containers[0].readinessProbe", func() bool { return currentContainer.ReadinessProbe == nil }},
		{"spec.template.spec.containers[0].startupProbe", func() bool { return currentContainer.StartupProbe == nil }},
		{"spec.template.spec.volumes[0]", func() bool { return reflect.DeepEqual(currentPod.Volumes, expectedPod.Volumes) }},
		{"spec.backoffLimit", func() bool {
			return reflect.DeepEqual(current.Spec.BackoffLimit, expected.Spec.BackoffLimit)
		}},
		{"spec.activeDeadlineSeconds", func() bool {
			return reflect.DeepEqual(current.Spec.ActiveDeadlineSeconds, expected.Spec.ActiveDeadlineSeconds)
		}},
	}
}

func oneOrDefault(value *int32) bool { return value == nil || *value == 1 }

func sameBoundExecutor(current, expected *cleanupapi.Executor) bool {
	return current != nil && expected != nil && current.JobName == expected.JobName && current.JobUID == expected.JobUID &&
		current.NodeName == expected.NodeName && (expected.PodUID == "" || current.PodUID == expected.PodUID)
}

func hostPathTypePtr(value corev1.HostPathType) *corev1.HostPathType { return &value }
