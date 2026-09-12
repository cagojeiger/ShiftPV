package helperpod

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/cagojeiger/ShiftPV/src/volume"
)

const (
	creationOperationAnnotation = "shiftpv.io/operation-id"
	creationVolumeUIDAnnotation = "shiftpv.io/volume-uid"
	creationCopyIDAnnotation    = "shiftpv.io/copy-id"
)

// runCreation resolves one durable Pod from the recorded creation operation.
// A successful Pod remains as execution evidence until CompleteCreate succeeds.
func (r *Runner) runCreation(ctx context.Context, identity volume.CopyIdentity, operationID string, command []string) (string, error) {
	desired, err := r.creationPod(ctx, identity, operationID, command)
	if err != nil {
		return "", err
	}
	pods := r.Client.CoreV1().Pods(r.Namespace)
	current, createErr := pods.Create(ctx, desired, metav1.CreateOptions{})
	if createErr != nil {
		if !apierrors.IsAlreadyExists(createErr) && !isAmbiguousKubernetesError(createErr) {
			return "", fmt.Errorf("create creation helper Pod: %w", classifyKubernetesAPIError(createErr))
		}
		current, err = pods.Get(ctx, desired.Name, metav1.GetOptions{})
		if err != nil {
			if isAmbiguousKubernetesError(createErr) && apierrors.IsNotFound(err) {
				return "", fmt.Errorf("create creation helper Pod: %w", classifyKubernetesAPIError(createErr))
			}
			return "", fmt.Errorf("resolve creation helper Pod: %w", classifyKubernetesAPIError(err))
		}
	}
	if err := sameCreationPod(desired, current); err != nil {
		return "", err
	}
	uid := current.UID
	if uid == "" {
		return "", fmt.Errorf("creation helper Pod has no UID")
	}

	result := ""
	err = wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, r.Timeout, true, func(pollCtx context.Context) (bool, error) {
		pod, getErr := pods.Get(pollCtx, desired.Name, metav1.GetOptions{})
		if getErr != nil {
			return false, classifyKubernetesAPIError(getErr)
		}
		if pod.UID != uid {
			return false, fmt.Errorf("creation helper Pod UID changed")
		}
		if err := sameCreationPod(desired, pod); err != nil {
			return false, err
		}
		terminated, terminationErr := creationTermination(pod)
		if terminationErr != nil {
			return false, terminationErr
		}
		if terminated == nil {
			return false, nil
		}
		if pod.Status.Phase == corev1.PodSucceeded && terminated.ExitCode == 0 {
			result = terminated.Message
			return true, nil
		}
		if err := r.deleteCreationPod(pollCtx, desired, uid); err != nil {
			return false, err
		}
		return false, retryableError{err: fmt.Errorf("creation helper exited %d", terminated.ExitCode)}
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = retryableError{err: err}
		}
		return "", fmt.Errorf("wait for creation helper Pod: %w", err)
	}
	return result, nil
}

func (r *Runner) finalizeCreation(ctx context.Context, identity volume.CopyIdentity, operationID string) error {
	desired, err := r.creationPod(ctx, identity, operationID, []string{
		"/shiftpv-volume-helper", "create",
		"--operation-id=" + operationID,
		"--installation-id=" + identity.InstallationID,
		"--pool-name=" + identity.PoolName,
		"--pool-uid=" + identity.PoolUID,
		"--volume-id=" + identity.VolumeID,
		"--volume-uid=" + identity.VolumeUID,
		"--copy-id=" + identity.CopyID,
		"--node-name=" + identity.NodeName,
	})
	if err != nil {
		return err
	}
	current, err := r.Client.CoreV1().Pods(r.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read settled creation helper Pod: %w", classifyKubernetesAPIError(err))
	}
	if err := sameCreationPod(desired, current); err != nil {
		return err
	}
	terminated, err := creationTermination(current)
	if err != nil {
		return err
	}
	if terminated == nil || current.Status.Phase != corev1.PodSucceeded || terminated.ExitCode != 0 {
		return retryableError{err: fmt.Errorf("creation helper Pod has not succeeded")}
	}
	return r.deleteCreationPod(ctx, desired, current.UID)
}

func (r *Runner) creationPod(ctx context.Context, identity volume.CopyIdentity, operationID string, command []string) (*corev1.Pod, error) {
	if identity.Validate() != nil || !volume.ValidIdentityToken(operationID) || len(command) == 0 {
		return nil, fmt.Errorf("creation helper identity is incomplete")
	}
	poolRoot, err := r.poolRoot(ctx, identity.NodeName)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(poolRoot) {
		return nil, fmt.Errorf("pool root must be absolute")
	}
	if r.Client == nil || r.Namespace == "" || r.Image == "" || r.Timeout <= 0 {
		return nil, fmt.Errorf("helper Pod configuration is incomplete")
	}
	pod := r.helperPod(identity.NodeName, identity.VolumeID, poolRoot, command)
	sum := sha256.Sum256([]byte(operationID + "\x00" + identity.CopyID))
	pod.Name = fmt.Sprintf("shiftpv-create-%x", sum[:16])
	pod.Namespace = r.Namespace
	pod.GenerateName = ""
	pod.Annotations = map[string]string{
		creationOperationAnnotation: operationID,
		creationVolumeUIDAnnotation: identity.VolumeUID,
		creationCopyIDAnnotation:    identity.CopyID,
	}
	pod.Spec.AutomountServiceAccountToken = boolPtr(true)
	return pod, nil
}

func sameCreationPod(expected, current *corev1.Pod) error {
	if expected == nil || current == nil || current.UID == "" || current.DeletionTimestamp != nil ||
		current.Name != expected.Name || current.Namespace != expected.Namespace ||
		current.Spec.NodeName != expected.Spec.NodeName || current.Spec.ServiceAccountName != expected.Spec.ServiceAccountName ||
		current.Spec.RestartPolicy != corev1.RestartPolicyNever || current.Spec.AutomountServiceAccountToken == nil ||
		!*current.Spec.AutomountServiceAccountToken || current.Spec.HostNetwork || current.Spec.HostPID || current.Spec.HostIPC ||
		len(current.Spec.Containers) != 1 || len(current.Spec.InitContainers) != 0 || len(current.Spec.EphemeralContainers) != 0 {
		return fmt.Errorf("creation helper Pod identity or execution shape changed")
	}
	for key, value := range expected.Labels {
		if current.Labels[key] != value {
			return fmt.Errorf("creation helper Pod label changed")
		}
	}
	for key, value := range expected.Annotations {
		if current.Annotations[key] != value {
			return fmt.Errorf("creation helper Pod annotation changed")
		}
	}
	want, got := expected.Spec.Containers[0], current.Spec.Containers[0]
	if got.Name != want.Name || got.Image != want.Image || !reflect.DeepEqual(got.Command, want.Command) ||
		len(got.Args) != 0 || len(got.Env) != 0 || len(got.EnvFrom) != 0 ||
		!reflect.DeepEqual(got.Resources, want.Resources) || !reflect.DeepEqual(got.SecurityContext, want.SecurityContext) {
		return fmt.Errorf("creation helper Pod command changed")
	}
	for _, podVolume := range current.Spec.Volumes {
		if podVolume.Name != "pool" || !reflect.DeepEqual(podVolume.HostPath, expected.Spec.Volumes[0].HostPath) {
			continue
		}
		for _, mount := range got.VolumeMounts {
			if mount.Name == "pool" && mount.MountPath == mountPath && mount.SubPath == "" && mount.SubPathExpr == "" {
				return nil
			}
		}
	}
	return fmt.Errorf("creation helper Pod Pool mount changed")
}

func creationTermination(pod *corev1.Pod) (*corev1.ContainerStateTerminated, error) {
	if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
		return nil, nil
	}
	if len(pod.Status.ContainerStatuses) != 1 || pod.Status.ContainerStatuses[0].Name != "operation" ||
		pod.Status.ContainerStatuses[0].State.Terminated == nil {
		return nil, retryableError{err: fmt.Errorf("creation helper Pod lacks termination evidence")}
	}
	return pod.Status.ContainerStatuses[0].State.Terminated, nil
}

func (r *Runner) deleteCreationPod(ctx context.Context, expected *corev1.Pod, uid types.UID) error {
	if uid == "" {
		return fmt.Errorf("creation helper Pod UID is missing")
	}
	zero := int64(0)
	pods := r.Client.CoreV1().Pods(r.Namespace)
	err := pods.Delete(ctx, expected.Name, metav1.DeleteOptions{GracePeriodSeconds: &zero, Preconditions: &metav1.Preconditions{UID: &uid}})
	if err != nil && !apierrors.IsNotFound(err) && !isAmbiguousKubernetesError(err) {
		return fmt.Errorf("delete creation helper Pod: %w", classifyKubernetesAPIError(err))
	}
	err = wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, r.Timeout, true, func(pollCtx context.Context) (bool, error) {
		current, getErr := pods.Get(pollCtx, expected.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(getErr) {
			return true, nil
		}
		if getErr != nil {
			return false, classifyKubernetesAPIError(getErr)
		}
		if current.UID != uid {
			return false, fmt.Errorf("creation helper Pod was replaced during deletion")
		}
		identity := current.DeepCopy()
		identity.DeletionTimestamp = nil
		if err := sameCreationPod(expected, identity); err != nil {
			return false, err
		}
		return false, nil
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = retryableError{err: err}
		}
		return fmt.Errorf("wait for creation helper Pod deletion: %w", err)
	}
	return nil
}

func isAmbiguousKubernetesError(err error) bool {
	return apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) || apierrors.IsServiceUnavailable(err)
}
