package helperpod

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	poolcapacity "github.com/cagojeiger/ShiftPV/src/pool/capacity"
	"github.com/cagojeiger/ShiftPV/src/volume"
)

const mountPath = "/pool"

type retryableError struct{ err error }

func (e retryableError) Error() string { return e.err.Error() }
func (e retryableError) Unwrap() error { return e.err }
func (retryableError) Retryable() bool { return true }

type Runner struct {
	Client             kubernetes.Interface
	Namespace          string
	ServiceAccountName string
	Pools              interface {
		PoolForNode(context.Context, string) (volumeapi.Pool, error)
		PoolForIdentity(context.Context, string, string, string) (volumeapi.Pool, error)
	}
	Image                   string
	Timeout                 time.Duration
	Resources               corev1.ResourceRequirements
	PoolReadinessStaleAfter time.Duration
}

func (r *Runner) CreateCopy(ctx context.Context, identity volume.CopyIdentity) error {
	if err := identity.Validate(); err != nil {
		return err
	}
	operationID, err := volumeapi.CreationOperationID(identity.VolumeUID)
	if err != nil {
		return err
	}
	_, err = r.runCreation(ctx, identity, operationID, []string{
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
	return err
}

// FinalizeCreate removes only the exact, successfully terminated helper after
// the controller has made the volume Ready. Absence is the settled state.
func (r *Runner) FinalizeCreate(ctx context.Context, identity volume.CopyIdentity) error {
	if err := identity.Validate(); err != nil {
		return err
	}
	operationID, err := volumeapi.CreationOperationID(identity.VolumeUID)
	if err != nil {
		return err
	}
	return r.finalizeCreation(ctx, identity, operationID)
}

func (r *Runner) StatFS(ctx context.Context, nodeName string) (poolcapacity.Filesystem, error) {
	output, err := r.runForResult(ctx, nodeName, "pool-capacity", []string{
		"sh", "-c", "stat -f -c '%b %a %S %d' /pool > /dev/termination-log",
	})
	if err != nil {
		return poolcapacity.Filesystem{}, err
	}
	stats, err := poolcapacity.ParseStatOutput(output)
	if err != nil {
		return poolcapacity.Filesystem{}, retryableError{err: fmt.Errorf("decode helper statfs result: %w", err)}
	}
	return stats, nil
}

func (r *Runner) VolumeUsage(ctx context.Context, nodeName, volumeID string) (int64, error) {
	path, err := volume.Path(mountPath, volumeID)
	if err != nil {
		return 0, err
	}
	output, err := r.runForResult(ctx, nodeName, volumeID, []string{
		"sh", "-c", "du -sb \"$1\" | awk '{print $1}' > /dev/termination-log", "shiftpv-du", path,
	})
	if err != nil {
		return 0, err
	}
	bytes, err := strconv.ParseInt(strings.TrimSpace(output), 10, 64)
	if err != nil || bytes < 0 {
		return 0, retryableError{err: fmt.Errorf("decode helper volume usage result %q", output)}
	}
	return bytes, nil
}

func (r *Runner) runForResult(ctx context.Context, nodeName, volumeID string, command []string) (string, error) {
	if nodeName == "" {
		return "", fmt.Errorf("node name is required")
	}
	poolRoot, err := r.poolRoot(ctx, nodeName)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(poolRoot) {
		return "", fmt.Errorf("pool root must be absolute")
	}
	if r.Client == nil {
		return "", fmt.Errorf("Kubernetes client is required")
	}
	if r.Namespace == "" || r.Image == "" || r.Timeout <= 0 {
		return "", fmt.Errorf("helper Pod configuration is incomplete")
	}

	zero := int64(0)
	pod, err := r.Client.CoreV1().Pods(r.Namespace).Create(ctx, r.helperPod(nodeName, volumeID, poolRoot, command), metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("create helper Pod: %w", classifyKubernetesAPIError(err))
	}
	defer func() {
		_ = r.Client.CoreV1().Pods(r.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &zero})
	}()

	result := ""
	err = wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, r.Timeout, true, func(pollCtx context.Context) (bool, error) {
		current, getErr := r.Client.CoreV1().Pods(r.Namespace).Get(pollCtx, pod.Name, metav1.GetOptions{})
		if getErr != nil {
			if apierrors.IsNotFound(getErr) {
				return false, nil
			}
			return false, classifyKubernetesAPIError(getErr)
		}
		switch current.Status.Phase {
		case corev1.PodSucceeded:
			if len(current.Status.ContainerStatuses) == 1 && current.Status.ContainerStatuses[0].State.Terminated != nil {
				result = current.Status.ContainerStatuses[0].State.Terminated.Message
			}
			return true, nil
		case corev1.PodFailed:
			return false, retryableError{err: fmt.Errorf("helper Pod failed: %s", current.Status.Message)}
		default:
			return false, nil
		}
	})
	if err != nil {
		return "", fmt.Errorf("wait for helper Pod on node %q: %w", nodeName, err)
	}
	return result, nil
}

func (r *Runner) helperPod(nodeName, volumeID, poolRoot string, command []string) *corev1.Pod {
	hostPathType := corev1.HostPathDirectory
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "shiftpv-volume-op-",
			Labels: map[string]string{
				"app.kubernetes.io/name":      "shiftpv",
				"app.kubernetes.io/component": "volume-helper",
				"shiftpv.io/volume-id":        volumeID,
			},
		},
		Spec: corev1.PodSpec{
			NodeName:           nodeName,
			ServiceAccountName: r.ServiceAccountName,
			RestartPolicy:      corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:         "operation",
				Image:        r.Image,
				Command:      command,
				Resources:    r.Resources,
				VolumeMounts: []corev1.VolumeMount{{Name: "pool", MountPath: mountPath}},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: boolPtr(false),
					RunAsUser:                int64Ptr(0),
					RunAsGroup:               int64Ptr(0),
				},
			}},
			Volumes: []corev1.Volume{{
				Name: "pool",
				VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
					Path: poolRoot,
					Type: &hostPathType,
				}},
			}},
		},
	}
}

func (r *Runner) poolRoot(ctx context.Context, nodeName string) (string, error) {
	if r.Pools == nil {
		return "", fmt.Errorf("ShiftPVPool registry is required")
	}
	pool, err := r.Pools.PoolForNode(ctx, nodeName)
	if err != nil {
		return "", fmt.Errorf("resolve ShiftPVPool for node %q: %w", nodeName, err)
	}
	return pool.MountPath, nil
}

func classifyKubernetesAPIError(err error) error {
	if apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) || apierrors.IsServiceUnavailable(err) {
		return retryableError{err: err}
	}
	return err
}

func boolPtr(value bool) *bool    { return &value }
func int64Ptr(value int64) *int64 { return &value }
