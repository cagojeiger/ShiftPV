package helperpod

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"

	"github.com/cagojeiger/ShiftPV/src/volume"
)

const mountPath = "/pool"

type retryableError struct{ err error }

func (e retryableError) Error() string { return e.err.Error() }
func (e retryableError) Unwrap() error { return e.err }
func (retryableError) Retryable() bool { return true }

type Runner struct {
	Client    kubernetes.Interface
	Namespace string
	PoolRoot  string
	Image     string
	Timeout   time.Duration
	Resources corev1.ResourceRequirements
}

func (r *Runner) Create(ctx context.Context, nodeName, volumeID string) error {
	path, err := volume.Path(mountPath, volumeID)
	if err != nil {
		return err
	}
	return r.run(ctx, nodeName, volumeID, []string{"mkdir", "-p", path})
}

func (r *Runner) Delete(ctx context.Context, nodeName, volumeID string) error {
	path, err := volume.Path(mountPath, volumeID)
	if err != nil {
		return err
	}
	return r.run(ctx, nodeName, volumeID, []string{"rm", "-rf", path})
}

func (r *Runner) run(ctx context.Context, nodeName, volumeID string, command []string) error {
	if nodeName == "" {
		return fmt.Errorf("node name is required")
	}
	if !filepath.IsAbs(r.PoolRoot) {
		return fmt.Errorf("pool root must be absolute")
	}
	if r.Client == nil {
		return fmt.Errorf("Kubernetes client is required")
	}
	if r.Namespace == "" || r.Image == "" || r.Timeout <= 0 {
		return fmt.Errorf("helper Pod configuration is incomplete")
	}

	hostPathType := corev1.HostPathDirectory
	zero := int64(0)
	pod, err := r.Client.CoreV1().Pods(r.Namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "shiftpv-volume-op-",
			Labels: map[string]string{
				"app.kubernetes.io/name":      "shiftpv",
				"app.kubernetes.io/component": "volume-helper",
				"shiftpv.io/volume-id":        volumeID,
			},
		},
		Spec: corev1.PodSpec{
			NodeName:      nodeName,
			RestartPolicy: corev1.RestartPolicyNever,
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
					Path: r.PoolRoot,
					Type: &hostPathType,
				}},
			}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create helper Pod: %w", classifyKubernetesAPIError(err))
	}
	defer func() {
		_ = r.Client.CoreV1().Pods(r.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &zero})
	}()

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
			return true, nil
		case corev1.PodFailed:
			return false, retryableError{err: fmt.Errorf("helper Pod failed: %s", current.Status.Message)}
		default:
			return false, nil
		}
	})
	if err != nil {
		return fmt.Errorf("wait for helper Pod on node %q: %w", nodeName, err)
	}
	return nil
}

func classifyKubernetesAPIError(err error) error {
	if apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) || apierrors.IsServiceUnavailable(err) {
		return retryableError{err: err}
	}
	return err
}

func boolPtr(value bool) *bool    { return &value }
func int64Ptr(value int64) *int64 { return &value }
