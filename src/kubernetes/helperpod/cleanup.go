package helperpod

import (
	"context"
	"errors"
	"fmt"
	"reflect"
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

func (r *Runner) Reclaim(ctx context.Context, cleanup cleanupapi.Cleanup, store *cleanupapi.Store) (cleanupapi.Cleanup, error) {
	if r == nil || r.Client == nil || r.Pools == nil || r.Namespace == "" || r.Image == "" || r.Timeout <= 0 || store == nil || cleanup.UID == "" || cleanup.Name == "" || cleanup.Spec.Validate() != nil || !cleanup.Spec.Approved {
		return cleanupapi.Cleanup{}, fmt.Errorf("cleanup runner configuration is incomplete")
	}
	if cleanup.Status.Phase == cleanupapi.PhaseCompleted {
		return cleanup, nil
	}
	if cleanup.Status.Phase == cleanupapi.PhaseNeedsReview {
		return cleanupapi.Cleanup{}, fmt.Errorf("cleanup %q needs review: %s", cleanup.Name, cleanup.Status.Reason)
	}
	pool, err := r.Pools.PoolForNode(ctx, cleanup.Spec.Target.NodeName)
	if err != nil {
		return cleanupapi.Cleanup{}, classifyKubernetesAPIError(err)
	}
	if pool.Name != cleanup.Spec.Target.PoolName || pool.UID != cleanup.Spec.Target.PoolUID {
		return cleanupapi.Cleanup{}, r.needsReview(ctx, store, cleanup, "PoolIdentityChanged", "registered Pool no longer matches the approved cleanup target")
	}
	cleanup, err = refreshCleanup(ctx, store, cleanup)
	if err != nil {
		return cleanupapi.Cleanup{}, err
	}
	if cleanup.Status.Phase == cleanupapi.PhaseCompleted {
		return cleanup, nil
	}
	if cleanup.Status.Phase == cleanupapi.PhaseNeedsReview {
		return cleanupapi.Cleanup{}, fmt.Errorf("cleanup %q needs review: %s", cleanup.Name, cleanup.Status.Reason)
	}
	if cleanup.Status.Phase != cleanupapi.PhaseVerifying {
		staleAfter := r.PoolReadinessStaleAfter
		if staleAfter <= 0 {
			staleAfter = volumeapi.DefaultPoolReadinessStaleAfter
		}
		if ready, reason := pool.CleanupReadyAt(time.Now(), staleAfter); !ready {
			return cleanupapi.Cleanup{}, retryableError{err: fmt.Errorf("cleanup Pool %q is not ready: %s", pool.Name, reason)}
		}
	}
	job := r.cleanupJob(cleanup, pool.MountPath)
	if cleanup.Status.Phase == cleanupapi.PhaseVerifying {
		return r.resumeVerifying(ctx, cleanup, store, job)
	}
	jobs := r.Client.BatchV1().Jobs(r.Namespace)
	created, err := jobs.Create(ctx, job, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) || isAmbiguousKubernetesError(err) {
		createErr := err
		created, err = jobs.Get(ctx, job.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) && isAmbiguousKubernetesError(createErr) {
			return cleanupapi.Cleanup{}, fmt.Errorf("ensure cleanup Job: %w", classifyKubernetesAPIError(createErr))
		}
		if err == nil && !sameCleanupJob(created, job) {
			return cleanupapi.Cleanup{}, r.needsReview(ctx, store, cleanup, "JobIdentityChanged", fmt.Sprintf("cleanup Job %q differs from the approved effect", job.Name))
		}
	}
	if err != nil {
		return cleanupapi.Cleanup{}, fmt.Errorf("ensure cleanup Job: %w", classifyKubernetesAPIError(err))
	}
	if created.UID == "" {
		return cleanupapi.Cleanup{}, r.needsReview(ctx, store, cleanup, "ExecutorIdentityMissing", "cleanup Job has no UID")
	}
	cleanup, err = refreshCleanup(ctx, store, cleanup)
	if err != nil {
		return cleanupapi.Cleanup{}, err
	}
	if cleanup.Status.Phase == cleanupapi.PhaseCompleted {
		return cleanup, nil
	}
	if cleanup.Status.Phase == cleanupapi.PhaseNeedsReview {
		return cleanupapi.Cleanup{}, fmt.Errorf("cleanup %q needs review: %s", cleanup.Name, cleanup.Status.Reason)
	}
	if (cleanup.Status.Phase == "" || cleanup.Status.Phase == cleanupapi.PhasePending) &&
		(created.Spec.Suspend == nil || !*created.Spec.Suspend) {
		return cleanupapi.Cleanup{}, r.needsReview(ctx, store, cleanup, "ExecutorStartedBeforeBinding", "cleanup Job started before its UID was bound to durable status")
	}
	executor := &cleanupapi.Executor{JobName: created.Name, JobUID: string(created.UID), NodeName: cleanup.Spec.Target.NodeName}
	if cleanup.Status.Phase == "" || cleanup.Status.Phase == cleanupapi.PhasePending {
		if err := store.UpdateStatus(ctx, cleanup.Name, cleanup.UID, cleanupapi.Status{Phase: cleanupapi.PhaseRunning, Executor: executor}); err != nil {
			return cleanupapi.Cleanup{}, fmt.Errorf("bind cleanup executor: %w", err)
		}
		cleanup.Status = cleanupapi.Status{Phase: cleanupapi.PhaseRunning, Executor: executor}
	} else if cleanup.Status.Executor == nil || !reflect.DeepEqual(cleanup.Status.Executor, executor) {
		return cleanupapi.Cleanup{}, r.needsReview(ctx, store, cleanup, "ExecutorIdentityChanged", "cleanup executor differs from the durable cleanup status")
	}
	if err := r.startCleanupJob(ctx, store, cleanup, created, job); err != nil {
		return cleanupapi.Cleanup{}, err
	}
	err = wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, r.Timeout, true, func(pollCtx context.Context) (bool, error) {
		current, getErr := store.Get(pollCtx, cleanup.Name)
		if getErr != nil {
			return false, getErr
		}
		if current.UID != cleanup.UID || current.Spec != cleanup.Spec {
			return false, cleanupapi.ErrConflict
		}
		switch current.Status.Phase {
		case cleanupapi.PhaseCompleted:
			cleanup = current
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
		if currentJob.UID != created.UID || !sameCleanupJob(currentJob, job) {
			return false, r.needsReview(pollCtx, store, current, "ExecutorIdentityChanged", "cleanup Job changed before termination was acknowledged")
		}
		complete := false
		for _, condition := range currentJob.Status.Conditions {
			if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
				return false, r.needsReview(pollCtx, store, current, "ExecutorFailed", fmt.Sprintf("cleanup Job failed: %s", condition.Message))
			}
			if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
				complete = true
			}
		}
		if !complete {
			return false, nil
		}
		if current.Status.Phase != cleanupapi.PhaseVerifying || current.Status.Receipt == nil {
			return false, r.needsReview(pollCtx, store, current, "ReceiptMissing", "cleanup Job completed without a durable receipt")
		}
		cleanup = current
		return true, nil
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = retryableError{err: err}
		}
		return cleanupapi.Cleanup{}, fmt.Errorf("wait for cleanup receipt: %w", err)
	}
	return cleanup, nil
}

func (r *Runner) resumeVerifying(ctx context.Context, cleanup cleanupapi.Cleanup, store *cleanupapi.Store, expected *batchv1.Job) (cleanupapi.Cleanup, error) {
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
		if current.Status.Phase == cleanupapi.PhaseCompleted {
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

func refreshCleanup(ctx context.Context, store *cleanupapi.Store, expected cleanupapi.Cleanup) (cleanupapi.Cleanup, error) {
	current, err := store.Get(ctx, expected.Name)
	if err != nil {
		return cleanupapi.Cleanup{}, fmt.Errorf("read cleanup intent: %w", classifyKubernetesAPIError(err))
	}
	if current.UID != expected.UID || current.Spec != expected.Spec {
		return cleanupapi.Cleanup{}, cleanupapi.ErrConflict
	}
	return current, nil
}

func (r *Runner) startCleanupJob(ctx context.Context, store *cleanupapi.Store, cleanup cleanupapi.Cleanup, created, expected *batchv1.Job) error {
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

func (r *Runner) needsReview(ctx context.Context, store *cleanupapi.Store, cleanup cleanupapi.Cleanup, reason, message string) error {
	if updateErr := store.UpdateStatus(ctx, cleanup.Name, cleanup.UID, cleanupapi.Status{
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
		ObjectMeta: metav1.ObjectMeta{Name: cleanup.Name + "-effect", Namespace: r.Namespace, Labels: labels},
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
							"cleanup", "--cleanup-name=" + cleanup.Name, "--cleanup-uid=" + cleanup.UID,
							"--operation-id=" + cleanup.Spec.OperationID, "--namespace=" + r.Namespace,
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

func sameCleanupJob(current, expected *batchv1.Job) bool {
	if current == nil || expected == nil || current.Labels[cleanupUIDLabel] != expected.Labels[cleanupUIDLabel] || current.Labels[cleanupNameLabel] != expected.Labels[cleanupNameLabel] {
		return false
	}
	currentPod, expectedPod := current.Spec.Template.Spec, expected.Spec.Template.Spec
	if current.DeletionTimestamp != nil || current.Namespace != expected.Namespace || current.Name != expected.Name ||
		!oneOrDefault(current.Spec.Parallelism) || !oneOrDefault(current.Spec.Completions) ||
		(current.Spec.ManualSelector != nil && *current.Spec.ManualSelector) ||
		(current.Spec.CompletionMode != nil && *current.Spec.CompletionMode != batchv1.NonIndexedCompletion) ||
		current.Spec.PodFailurePolicy != nil || current.Spec.SuccessPolicy != nil || current.Spec.BackoffLimitPerIndex != nil ||
		current.Spec.MaxFailedIndexes != nil || (current.Spec.ManagedBy != nil && *current.Spec.ManagedBy != batchv1.JobControllerName) ||
		current.Spec.Template.Labels[cleanupUIDLabel] != expected.Spec.Template.Labels[cleanupUIDLabel] ||
		current.Spec.Template.Labels[cleanupNameLabel] != expected.Spec.Template.Labels[cleanupNameLabel] ||
		currentPod.NodeName != expectedPod.NodeName || currentPod.ServiceAccountName != expectedPod.ServiceAccountName ||
		currentPod.RestartPolicy != expectedPod.RestartPolicy || currentPod.HostPID || currentPod.HostIPC || currentPod.HostNetwork ||
		len(currentPod.InitContainers) != 0 || len(currentPod.EphemeralContainers) != 0 ||
		len(currentPod.Containers) != 1 || len(expectedPod.Containers) != 1 || len(currentPod.Volumes) != 1 || len(expectedPod.Volumes) != 1 {
		return false
	}
	currentContainer, expectedContainer := currentPod.Containers[0], expectedPod.Containers[0]
	return currentContainer.Name == expectedContainer.Name && currentContainer.Image == expectedContainer.Image &&
		reflect.DeepEqual(currentContainer.Command, expectedContainer.Command) &&
		reflect.DeepEqual(currentContainer.Args, expectedContainer.Args) &&
		reflect.DeepEqual(currentContainer.Env, expectedContainer.Env) && len(currentContainer.EnvFrom) == 0 &&
		reflect.DeepEqual(currentContainer.Resources, expectedContainer.Resources) &&
		reflect.DeepEqual(currentContainer.SecurityContext, expectedContainer.SecurityContext) &&
		reflect.DeepEqual(currentContainer.VolumeMounts, expectedContainer.VolumeMounts) &&
		len(currentContainer.VolumeDevices) == 0 && currentContainer.Lifecycle == nil && currentContainer.LivenessProbe == nil &&
		currentContainer.ReadinessProbe == nil && currentContainer.StartupProbe == nil &&
		reflect.DeepEqual(currentPod.Volumes, expectedPod.Volumes) &&
		reflect.DeepEqual(current.Spec.BackoffLimit, expected.Spec.BackoffLimit) &&
		reflect.DeepEqual(current.Spec.ActiveDeadlineSeconds, expected.Spec.ActiveDeadlineSeconds)
}

func oneOrDefault(value *int32) bool { return value == nil || *value == 1 }

func hostPathTypePtr(value corev1.HostPathType) *corev1.HostPathType { return &value }
