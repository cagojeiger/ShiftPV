package controller

import (
	"context"
	"fmt"
	"reflect"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/lifecycle/cleanup"
)

const cleanupSourceScript = `set -eu
source="/pool/volumes/${VOLUME_ID}"
retired_root="/pool/.shiftpv/retired"
retired="${retired_root}/${MOVE_NAME}"

for path in /pool /pool/volumes /pool/.shiftpv "${retired_root}" "${source}" "${retired}"; do
  test ! -L "${path}"
done
mkdir -p "${retired_root}"

if test -e "${source}"; then
  test -d "${source}"
  test ! -e "${retired}"
  test "$(stat -c %d "${source}")" = "$(stat -c %d "${retired_root}")"
  mv -T "${source}" "${retired}"
elif test -e "${retired}"; then
  test -d "${retired}"
fi

if test -e "${retired}"; then
  rm -rf --one-file-system -- "${retired}"
fi
test ! -e "${source}"
test ! -L "${source}"
test ! -e "${retired}"
test ! -L "${retired}"
`

func (r *Reconciler) ensureCleanupJob(ctx context.Context, move volumeapi.Move, names resourceNames) error {
	state, err := r.Repository.Get(ctx, move.Spec.VolumeID)
	if err != nil {
		return err
	}
	if state.Phase != volumeapi.PhaseReady || state.ActiveMove != move.Name || state.OwnerNode != move.Status.DestinationNode || !contains(state.PublishedNodes, move.Status.DestinationNode) || contains(state.PublishedNodes, move.Spec.SourceNode) {
		return fmt.Errorf("source cleanup requires matching committed authority and destination publication")
	}
	root, err := r.poolMountPath(ctx, move.Spec.SourceNode)
	if err != nil {
		return err
	}
	intent := cleanupIntent(move, root)
	record, err := r.cleanupJournal().Ensure(ctx, intent)
	if err != nil || record.Completed {
		return err
	}
	job, err := r.Client.BatchV1().Jobs(r.Namespace).Get(ctx, names.CleanupJob, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if record.JobUID != "" {
			return fmt.Errorf("recorded cleanup Job disappeared before acknowledgement")
		}
		job, err = r.operationJob(ctx, names.CleanupJob, move.Spec.SourceNode, names, cleanupSourceScript, cleanupEnv(move), nil, nil)
		if err != nil {
			return err
		}
		// The approved path is immutable even if the Pool changes between reads.
		job.Spec.Template.Spec.Volumes[0].HostPath.Path = intent.PoolPath
		job.Spec.TTLSecondsAfterFinished = nil
		job.Labels["shiftpv.io/move-uid"] = move.UID
		job, err = r.Client.BatchV1().Jobs(r.Namespace).Create(ctx, job, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			job, err = r.Client.BatchV1().Jobs(r.Namespace).Get(ctx, names.CleanupJob, metav1.GetOptions{})
		}
	}
	if err != nil {
		return err
	}
	if err := validateCleanupJob(job, record); err != nil {
		return err
	}
	return r.cleanupJournal().BindJob(ctx, intent, string(job.UID))
}

func cleanupEnv(move volumeapi.Move) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "MOVE_NAME", Value: move.Name}, {Name: "VOLUME_ID", Value: move.Spec.VolumeID},
	}
}

func cleanupIntent(move volumeapi.Move, root string) cleanup.Intent {
	return cleanup.Intent{MoveName: move.Name, MoveUID: move.UID, VolumeID: move.Spec.VolumeID, SourceNode: move.Spec.SourceNode, DestinationNode: move.Status.DestinationNode, PoolPath: root, JobName: namesFor(move.Name).CleanupJob}
}

func (r *Reconciler) cleanupJournal() cleanup.Journal {
	return cleanup.Journal{Client: r.Client, Namespace: r.Namespace, Now: r.Now}
}

func validateCleanupJob(job *batchv1.Job, record cleanup.Record) error {
	i := record.Intent
	pod := job.Spec.Template.Spec
	if job.UID == "" || (record.JobUID != "" && string(job.UID) != record.JobUID) || job.DeletionTimestamp != nil || job.Name != i.JobName || job.Labels["shiftpv.io/move-uid"] != i.MoveUID || pod.NodeName != i.SourceNode || len(pod.Containers) != 1 || len(pod.Volumes) != 1 || pod.Volumes[0].HostPath == nil || pod.Volumes[0].HostPath.Path != i.PoolPath {
		return fmt.Errorf("cleanup Job identity or storage target changed")
	}
	c := pod.Containers[0]
	if !reflect.DeepEqual(c.Command, []string{"/bin/sh", "-c", cleanupSourceScript}) || !reflect.DeepEqual(c.Env, []corev1.EnvVar{{Name: "MOVE_NAME", Value: i.MoveName}, {Name: "VOLUME_ID", Value: i.VolumeID}}) || !reflect.DeepEqual(c.VolumeMounts, []corev1.VolumeMount{{Name: "pool", MountPath: "/pool"}}) {
		return fmt.Errorf("cleanup Job action differs from request")
	}
	return nil
}

func (r *Reconciler) cleanupState(ctx context.Context, move volumeapi.Move) (complete, failed bool, err error) {
	record, err := r.cleanupJournal().Get(ctx, move.UID)
	if apierrors.IsNotFound(err) {
		_, jobErr := r.Client.BatchV1().Jobs(r.Namespace).Get(ctx, namesFor(move.Name).CleanupJob, metav1.GetOptions{})
		if apierrors.IsNotFound(jobErr) {
			return false, false, nil
		}
		if jobErr != nil {
			return false, false, jobErr
		}
		return false, false, fmt.Errorf("cleanup Job exists without durable request")
	}
	if err != nil {
		return false, false, err
	}
	if record.Intent != cleanupIntent(move, record.Intent.PoolPath) {
		return false, false, fmt.Errorf("cleanup request belongs to a different Move")
	}
	if record.Completed {
		return true, false, nil
	}
	job, err := r.Client.BatchV1().Jobs(r.Namespace).Get(ctx, record.Intent.JobName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) && record.JobUID == "" {
		return false, false, nil
	}
	if apierrors.IsNotFound(err) {
		return false, true, nil
	}
	if err != nil {
		return false, false, err
	}
	if err := validateCleanupJob(job, record); err != nil {
		return false, false, err
	}
	if record.JobUID == "" {
		return false, false, nil
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			complete = true
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			failed = true
		}
	}
	return complete, failed, nil
}

func (r *Reconciler) acknowledgeCleanup(ctx context.Context, move volumeapi.Move) error {
	complete, failed, err := r.cleanupState(ctx, move)
	if err != nil {
		return err
	}
	if !complete || failed {
		return fmt.Errorf("cleanup success is not confirmed")
	}
	record, err := r.cleanupJournal().Get(ctx, move.UID)
	if err != nil {
		return err
	}
	if err := r.cleanupJournal().Complete(ctx, record.Intent, record.JobUID); err != nil {
		return err
	}
	job, err := r.Client.BatchV1().Jobs(r.Namespace).Get(ctx, record.Intent.JobName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := validateCleanupJob(job, record); err != nil {
		return err
	}
	if job.Spec.TTLSecondsAfterFinished != nil {
		return nil
	}
	ttl := int32(600)
	job.Spec.TTLSecondsAfterFinished = &ttl
	_, err = r.Client.BatchV1().Jobs(r.Namespace).Update(ctx, job, metav1.UpdateOptions{})
	return err
}
