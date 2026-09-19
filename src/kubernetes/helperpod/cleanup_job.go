package helperpod

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/project-jelly/ShiftPV/src/kubernetes/cleanupapi"
	"github.com/project-jelly/ShiftPV/src/kubernetes/volumeapi"
)

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

func hostPathTypePtr(value corev1.HostPathType) *corev1.HostPathType { return &value }
