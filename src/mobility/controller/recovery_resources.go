package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
)

func (r *Reconciler) quiesceMove(ctx context.Context, move volumeapi.Move) (bool, error) {
	names := namesFor(move.Name)
	quiet := true
	for _, name := range []string{names.CopyJob, names.PromotionJob, names.CleanupJob} {
		gone, err := r.removeJob(ctx, name, names.Base, "")
		if err != nil {
			return false, err
		}
		quiet = quiet && gone
	}
	// List includes orphaned helper Pods, not just the source daemon. No forced
	// deletion: API disappearance is only trusted with healthy participating nodes.
	pods, err := r.Client.CoreV1().Pods(r.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "shiftpv.io/move=" + names.Base})
	if err != nil {
		return false, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName != move.Spec.SourceNode && (move.Status.DestinationNode == "" || pod.Spec.NodeName != move.Status.DestinationNode) {
			return false, fmt.Errorf("helper Pod %q has unrecorded node %q", pod.Name, pod.Spec.NodeName)
		}
		quiet = false
		if pod.DeletionTimestamp == nil {
			uid := pod.UID
			if err := r.Client.CoreV1().Pods(r.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
		}
	}
	if !quiet {
		return false, nil
	}
	if err := r.deleteTransferResources(ctx, names); err != nil {
		return false, err
	}
	return true, nil
}

func recoveryNames(move volumeapi.Move) resourceNames {
	return namesFor(move.Name + "-recovery-" + move.UID)
}

func (r *Reconciler) recoveryJob(ctx context.Context, move volumeapi.Move, verify bool) (bool, error) {
	names := recoveryNames(move)
	node := move.Status.RecoveryOwner
	name, script := names.Base+"-verify", recoveryVerifyScript
	if !verify {
		node = move.Status.DestinationNode
		if move.Status.RecoveryOwner != move.Spec.SourceNode {
			node = move.Spec.SourceNode
		}
		if node == "" {
			return true, nil
		}
		if node == move.Status.RecoveryOwner {
			return false, fmt.Errorf("refusing to retire current owner")
		}
		name, script = names.Base+"-retire", recoveryRetireScript
	}
	job, err := r.Client.BatchV1().Jobs(r.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		job, err = r.operationJob(ctx, name, node, names, script, []corev1.EnvVar{
			{Name: "MOVE_NAME", Value: move.Name}, {Name: "VOLUME_ID", Value: move.Spec.VolumeID},
			{Name: "COMMITTED", Value: fmt.Sprint(move.Status.RecoveryOwner != move.Spec.SourceNode)},
		}, nil, nil)
		if err != nil {
			return false, err
		}
		// Preserve completion evidence across long controller outages. Only the
		// recovery controller removes these Jobs after durable phase advancement.
		job.Spec.TTLSecondsAfterFinished = nil
		zeroRetries := int32(0)
		job.Spec.BackoffLimit = &zeroRetries
		job.OwnerReferences = []metav1.OwnerReference{{APIVersion: "shiftpv.io/v1alpha1", Kind: "ShiftPVMove", Name: move.Name, UID: types.UID(move.UID)}}
		if verify {
			job.Spec.Template.Spec.Containers[0].VolumeMounts[0].ReadOnly = true
		}
		_, err = r.Client.BatchV1().Jobs(r.Namespace).Create(ctx, job, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			err = nil
		}
		return false, err
	}
	if err != nil {
		return false, err
	}
	if job.Labels["shiftpv.io/move"] != names.Base || !recoveryOwned(job, move.UID) || job.Spec.Template.Spec.NodeName != node || job.DeletionTimestamp != nil {
		return false, fmt.Errorf("recovery Job %q identity or node mismatch", name)
	}
	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		if condition.Type == batchv1.JobFailed {
			return false, fmt.Errorf("recovery Job %q failed; inspect logs and storage before retry", name)
		}
		if condition.Type == batchv1.JobComplete {
			return true, nil
		}
	}
	return false, nil
}

func (r *Reconciler) removeRecoveryJobs(ctx context.Context, move volumeapi.Move) (bool, error) {
	names := recoveryNames(move)
	quiet := true
	for _, name := range []string{names.Base + "-verify", names.Base + "-retire"} {
		gone, err := r.removeJob(ctx, name, names.Base, move.UID)
		if err != nil {
			return false, err
		}
		quiet = quiet && gone
	}
	pods, err := r.Client.CoreV1().Pods(r.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "shiftpv.io/move=" + names.Base})
	if err != nil {
		return false, err
	}
	return quiet && len(pods.Items) == 0, nil
}

func (r *Reconciler) removeJob(ctx context.Context, name, label, moveUID string) (bool, error) {
	job, err := r.Client.BatchV1().Jobs(r.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if job.Labels["shiftpv.io/move"] != label || (moveUID != "" && !recoveryOwned(job, moveUID)) {
		return false, fmt.Errorf("refusing to delete unrelated Job %q", name)
	}
	if job.DeletionTimestamp == nil {
		uid := job.UID
		foreground := metav1.DeletePropagationForeground
		err = r.Client.BatchV1().Jobs(r.Namespace).Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: &foreground})
		if apierrors.IsNotFound(err) {
			err = nil
		}
	}
	return false, err
}

func recoveryOwned(job *batchv1.Job, uid string) bool {
	for _, ref := range job.OwnerReferences {
		if ref.Kind == "ShiftPVMove" && ref.APIVersion == "shiftpv.io/v1alpha1" && string(ref.UID) == uid && uid != "" {
			return true
		}
	}
	return false
}

const recoveryVerifyScript = `set -eu
test ! -L /pool/volumes
final="/pool/volumes/${VOLUME_ID}"
test ! -L "${final}"
test -d "${final}"
test -r "${final}"
if test "${COMMITTED}" = true; then
  test ! -L "${final}/.shiftpv-move-id"
  test "$(cat "${final}/.shiftpv-move-id")" = "${MOVE_NAME}"
fi
`

// Rename, never delete or overwrite. A completed rename is an idempotent retry;
// finding both the original and quarantine paths is ambiguous and must fail.
const recoveryRetireScript = `set -eu
for path in /pool/volumes /pool/.shiftpv /pool/.shiftpv/incoming /pool/.shiftpv/aborted; do
  test ! -L "${path}"
done
mkdir -p /pool/.shiftpv/aborted
retire() {
  source=$1
  target=$2
  test ! -L "${source}"
  test ! -L "${target}"
  if test -e "${source}"; then
    test -d "${source}"
    test ! -e "${target}"
    test "$(stat -c %d "${source}")" = "$(stat -c %d /pool/.shiftpv/aborted)"
    mv -T "${source}" "${target}"
  elif test -e "${target}"; then
    test -d "${target}"
  fi
}
final="/pool/volumes/${VOLUME_ID}"
test ! -L "${final}"
if test "${COMMITTED}" = true && test ! -e "${final}" && test ! -e "/pool/.shiftpv/aborted/${MOVE_NAME}-final"; then
  # Original cleanup might already have retired the old source before failing.
  # Otherwise a missing source and missing quarantine are not explained.
  test ! -L /pool/.shiftpv/retired
  test ! -L "/pool/.shiftpv/retired/${MOVE_NAME}"
  test -d "/pool/.shiftpv/retired/${MOVE_NAME}"
fi
if test -e "${final}" && test "${COMMITTED}" = false; then
  test ! -L "${final}/.shiftpv-move-id"
  test "$(cat "${final}/.shiftpv-move-id")" = "${MOVE_NAME}"
fi
retire "${final}" "/pool/.shiftpv/aborted/${MOVE_NAME}-final"
retire "/pool/.shiftpv/incoming/${MOVE_NAME}" "/pool/.shiftpv/aborted/${MOVE_NAME}-incoming"
`
