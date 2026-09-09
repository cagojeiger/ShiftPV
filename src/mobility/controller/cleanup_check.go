package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/lifecycle/cleanup"
)

const cleanupCheckBinary = "/shiftpv-cleanup-check"

func cleanupCheckName(record cleanup.Record) string {
	if record.CheckID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(record.Intent.MoveUID + "/" + record.CheckID))
	return "shiftpv-check-" + hex.EncodeToString(sum[:12])
}

func (r *Reconciler) reconcileCleanupCheck(ctx context.Context, record cleanup.Record, moves map[string]volumeapi.Move) error {
	if err := r.cleanupCheckAuthority(ctx, record, moves); err != nil {
		return r.cleanupReview(ctx, record, err)
	}
	name := cleanupCheckName(record)
	job, err := r.Client.BatchV1().Jobs(r.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if record.CheckJobUID != "" {
			return r.cleanupJournal().FinishCheck(ctx, record.Intent, record.CheckID, record.CheckJobUID, false)
		}
		job, err = r.newCleanupCheck(ctx, record)
		if err != nil {
			return err
		}
		job, err = r.Client.BatchV1().Jobs(r.Namespace).Create(ctx, job, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			job, err = r.Client.BatchV1().Jobs(r.Namespace).Get(ctx, name, metav1.GetOptions{})
		}
	}
	if err != nil {
		return err
	}
	if err := validateCleanupCheck(job, record); err != nil {
		return r.cleanupReview(ctx, record, err)
	}
	if record.CheckJobUID == "" {
		return r.cleanupJournal().BindCheck(ctx, record.Intent, record.CheckID, string(job.UID))
	}
	complete, failed := jobFinished(job)
	if !complete && !failed {
		return r.cleanupJournal().SetState(ctx, record.Intent, cleanup.Running, "Checking source paths")
	}
	if err := r.cleanupJournal().FinishCheck(ctx, record.Intent, record.CheckID, string(job.UID), complete && !failed); err != nil {
		return err
	}
	return r.expireCleanupJob(ctx, job)
}

func (r *Reconciler) newCleanupCheck(ctx context.Context, record cleanup.Record) (*batchv1.Job, error) {
	i := record.Intent
	names := namesFor("check-" + i.MoveUID + "-" + record.CheckID)
	job, err := r.operationJob(ctx, cleanupCheckName(record), i.SourceNode, names, "", cleanupEnv(volumeapi.Move{Name: i.MoveName, Spec: volumeapi.MoveSpec{VolumeID: i.VolumeID}}), nil, nil)
	if err != nil {
		return nil, err
	}
	job.Labels["shiftpv.io/move-uid"], job.Annotations = i.MoveUID, map[string]string{cleanup.CheckRequestKey: record.CheckID}
	job.Spec.TTLSecondsAfterFinished = nil
	zero := int32(0)
	job.Spec.BackoffLimit = &zero
	job.Spec.Template.Spec.Volumes[0].HostPath.Path = i.PoolPath
	job.Spec.Template.Spec.Containers[0].Command = []string{cleanupCheckBinary, "/pool"}
	job.Spec.Template.Spec.Containers[0].Image = record.CheckImage
	job.Spec.Template.Spec.Containers[0].VolumeMounts[0].ReadOnly = true
	job.Spec.Template.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem = boolPointer(true)
	job.Spec.Template.Spec.Containers[0].SecurityContext.Capabilities = &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}
	return job, nil
}

func validateCleanupCheck(job *batchv1.Job, record cleanup.Record) error {
	i, pod := record.Intent, job.Spec.Template.Spec
	if job.UID == "" || (record.CheckJobUID != "" && string(job.UID) != record.CheckJobUID) || job.Name != cleanupCheckName(record) || job.DeletionTimestamp != nil || job.Labels["shiftpv.io/move-uid"] != i.MoveUID || job.Annotations[cleanup.CheckRequestKey] != record.CheckID || pod.NodeName != i.SourceNode || len(pod.Volumes) != 1 || pod.Volumes[0].HostPath == nil || pod.Volumes[0].HostPath.Path != i.PoolPath || len(pod.Containers) != 1 {
		return fmt.Errorf("cleanup check Job identity or path changed")
	}
	c := pod.Containers[0]
	if c.Image != record.CheckImage || pod.RestartPolicy != corev1.RestartPolicyNever || job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 || job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 300 {
		return fmt.Errorf("cleanup check image or execution budget changed")
	}
	security := &corev1.SecurityContext{AllowPrivilegeEscalation: boolPointer(false), RunAsUser: int64Pointer(0), ReadOnlyRootFilesystem: boolPointer(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}
	if len(pod.InitContainers) != 0 || len(pod.EphemeralContainers) != 0 || pod.HostPID || pod.HostIPC || pod.HostNetwork || len(c.EnvFrom) != 0 || len(c.Args) != 0 || c.Lifecycle != nil || !reflect.DeepEqual(c.SecurityContext, security) {
		return fmt.Errorf("cleanup check security or execution settings changed")
	}
	expectedEnv := []corev1.EnvVar{{Name: "MOVE_NAME", Value: i.MoveName}, {Name: "VOLUME_ID", Value: i.VolumeID}}
	if !reflect.DeepEqual(c.Command, []string{cleanupCheckBinary, "/pool"}) || !reflect.DeepEqual(c.Env, expectedEnv) || !reflect.DeepEqual(c.VolumeMounts, []corev1.VolumeMount{{Name: "pool", MountPath: "/pool", ReadOnly: true}}) {
		return fmt.Errorf("cleanup check must use the approved read-only action")
	}
	return nil
}
