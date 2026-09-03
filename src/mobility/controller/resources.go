package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
)

type resourceNames struct {
	Base          string
	Secret        string
	Config        string
	SourcePod     string
	SourceService string
	CopyJob       string
	PromotionJob  string
	CleanupJob    string
}

func namesFor(moveName string) resourceNames {
	sum := sha256.Sum256([]byte(moveName))
	base := "shiftpv-move-" + hex.EncodeToString(sum[:6])
	return resourceNames{
		Base: base, Secret: base + "-auth", Config: base + "-config", SourcePod: base + "-source",
		SourceService: base + "-source", CopyJob: base + "-copy", PromotionJob: base + "-promote", CleanupJob: base + "-cleanup",
	}
}

func (r *Reconciler) ensureCopyResources(ctx context.Context, move volumeapi.Move, names resourceNames) error {
	if err := r.ensureTransferSecret(ctx, names); err != nil {
		return err
	}
	if err := r.ensureRsyncConfig(ctx, move, names); err != nil {
		return err
	}
	if err := r.ensureSourcePod(ctx, move, names); err != nil {
		return err
	}
	if err := r.ensureSourceService(ctx, names); err != nil {
		return err
	}
	ready, err := r.sourcePodReady(ctx, names.SourcePod)
	if err != nil {
		return err
	}
	if !ready {
		return nil
	}
	return r.ensureCopyJob(ctx, move, names)
}

func (r *Reconciler) sourcePodReady(ctx context.Context, name string) (bool, error) {
	pod, err := r.Client.CoreV1().Pods(r.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("read rsync source Pod: %w", err)
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue, nil
		}
	}
	return false, nil
}

func (r *Reconciler) ensureTransferSecret(ctx context.Context, names resourceNames) error {
	passwordBytes := make([]byte, 24)
	if _, err := rand.Read(passwordBytes); err != nil {
		return fmt.Errorf("generate rsync password: %w", err)
	}
	password := hex.EncodeToString(passwordBytes)
	_, err := r.Client.CoreV1().Secrets(r.Namespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: names.Secret, Namespace: r.Namespace, Labels: transferLabels(names)},
		StringData: map[string]string{"password": password, "secrets": "shiftpv:" + password + "\n"},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create rsync Secret: %w", err)
	}
	return nil
}

func (r *Reconciler) ensureRsyncConfig(ctx context.Context, move volumeapi.Move, names resourceNames) error {
	configuration := fmt.Sprintf(`uid = 0
gid = 0
use chroot = no
read only = yes
strict modes = yes
[data]
path = /pool/volumes/%s
auth users = shiftpv
secrets file = /auth/secrets
`, move.Spec.VolumeID)
	_, err := r.Client.CoreV1().ConfigMaps(r.Namespace).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: names.Config, Namespace: r.Namespace, Labels: transferLabels(names)},
		Data:       map[string]string{"rsyncd.conf": configuration},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create rsync ConfigMap: %w", err)
	}
	return nil
}

func (r *Reconciler) ensureSourcePod(ctx context.Context, move volumeapi.Move, names resourceNames) error {
	secretMode := int32(0o400)
	poolRoot, err := r.poolMountPath(ctx, move.Spec.SourceNode)
	if err != nil {
		return err
	}
	_, err = r.Client.CoreV1().Pods(r.Namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: names.SourcePod, Namespace: r.Namespace, Labels: sourceLabels(names)},
		Spec: corev1.PodSpec{
			NodeName:      move.Spec.SourceNode,
			RestartPolicy: corev1.RestartPolicyAlways,
			Containers: []corev1.Container{{
				Name: "rsync", Image: r.HelperImage,
				Command: []string{"rsync"}, Args: []string{"--daemon", "--no-detach", "--config=/config/rsyncd.conf"},
				Ports:           []corev1.ContainerPort{{Name: "rsync", ContainerPort: 873}},
				ReadinessProbe:  &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstrFromInt(873)}}, InitialDelaySeconds: 1, PeriodSeconds: 1},
				SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: boolPointer(false), RunAsUser: int64Pointer(0)},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "pool", MountPath: "/pool", ReadOnly: true},
					{Name: "auth", MountPath: "/auth", ReadOnly: true},
					{Name: "config", MountPath: "/config", ReadOnly: true},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "pool", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: poolRoot, Type: hostPathTypePointer(corev1.HostPathDirectory)}}},
				{Name: "auth", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: names.Secret, DefaultMode: &secretMode}}},
				{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: names.Config}}}},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create rsync source Pod: %w", err)
	}
	return nil
}

func (r *Reconciler) ensureSourceService(ctx context.Context, names resourceNames) error {
	_, err := r.Client.CoreV1().Services(r.Namespace).Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: names.SourceService, Namespace: r.Namespace, Labels: transferLabels(names)},
		Spec:       corev1.ServiceSpec{Selector: sourceLabels(names), Ports: []corev1.ServicePort{{Name: "rsync", Port: 873, TargetPort: intstrFromInt(873)}}},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create rsync source Service: %w", err)
	}
	return nil
}

func (r *Reconciler) ensureCopyJob(ctx context.Context, move volumeapi.Move, names resourceNames) error {
	script := `set -eu
staging="/pool/.shiftpv/incoming/${MOVE_NAME}"
mkdir -p "${staging}"
export RSYNC_PASSWORD="$(cat /auth/password)"
rsync -a --delete "rsync://shiftpv@${SOURCE_SERVICE}/data/" "${staging}/"
rsync -a --checksum --delete --dry-run "rsync://shiftpv@${SOURCE_SERVICE}/data/" "${staging}/" > /tmp/rsync-diff
test ! -s /tmp/rsync-diff
printf '%s\n' "${MOVE_NAME}" > "${staging}/.shiftpv-move-id"
`
	return r.ensureJob(ctx, names.CopyJob, move.Status.DestinationNode, names, script, []corev1.EnvVar{
		{Name: "MOVE_NAME", Value: move.Name}, {Name: "SOURCE_SERVICE", Value: names.SourceService},
	}, []corev1.VolumeMount{{Name: "auth", MountPath: "/auth", ReadOnly: true}}, []corev1.Volume{{Name: "auth", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: names.Secret}}}})
}

func (r *Reconciler) ensurePromotionJob(ctx context.Context, move volumeapi.Move, names resourceNames) error {
	script := `set -eu
staging="/pool/.shiftpv/incoming/${MOVE_NAME}"
final="/pool/volumes/${VOLUME_ID}"
if test -d "${final}"; then
  test "$(cat "${final}/.shiftpv-move-id")" = "${MOVE_NAME}"
  exit 0
fi
test "$(cat "${staging}/.shiftpv-move-id")" = "${MOVE_NAME}"
test "$(stat -c '%d' "${staging}")" = "$(stat -c '%d' /pool)"
mkdir -p /pool/volumes
mv "${staging}" "${final}"
`
	return r.ensureJob(ctx, names.PromotionJob, move.Status.DestinationNode, names, script, []corev1.EnvVar{
		{Name: "MOVE_NAME", Value: move.Name}, {Name: "VOLUME_ID", Value: move.Spec.VolumeID},
	}, nil, nil)
}

func (r *Reconciler) ensureCleanupJob(ctx context.Context, move volumeapi.Move, names resourceNames) error {
	script := `set -eu
source="/pool/volumes/${VOLUME_ID}"
retired="/pool/.shiftpv/retired/${MOVE_NAME}"
if test -d "${retired}"; then exit 0; fi
test -d "${source}"
mkdir -p /pool/.shiftpv/retired
mv "${source}" "${retired}"
`
	return r.ensureJob(ctx, names.CleanupJob, move.Spec.SourceNode, names, script, []corev1.EnvVar{
		{Name: "MOVE_NAME", Value: move.Name}, {Name: "VOLUME_ID", Value: move.Spec.VolumeID},
	}, nil, nil)
}

func (r *Reconciler) ensureJob(ctx context.Context, name, nodeName string, names resourceNames, script string, env []corev1.EnvVar, extraMounts []corev1.VolumeMount, extraVolumes []corev1.Volume) error {
	backoff := int32(2)
	ttl := int32(600)
	deadline := int64(300)
	poolRoot, err := r.poolMountPath(ctx, nodeName)
	if err != nil {
		return err
	}
	mounts := append([]corev1.VolumeMount{{Name: "pool", MountPath: "/pool"}}, extraMounts...)
	volumes := append([]corev1.Volume{{Name: "pool", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: poolRoot, Type: hostPathTypePointer(corev1.HostPathDirectory)}}}}, extraVolumes...)
	_, err = r.Client.BatchV1().Jobs(r.Namespace).Create(ctx, &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Namespace, Labels: transferLabels(names)},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff, TTLSecondsAfterFinished: &ttl, ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: transferLabels(names)},
				Spec: corev1.PodSpec{NodeName: nodeName, RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{Name: "operation", Image: r.HelperImage, Command: []string{"/bin/sh", "-c", script}, Env: env, VolumeMounts: mounts,
						SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: boolPointer(false), RunAsUser: int64Pointer(0)}}},
					Volumes: volumes,
				},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create mobility Job %q: %w", name, err)
	}
	return nil
}

func (r *Reconciler) poolMountPath(ctx context.Context, nodeName string) (string, error) {
	pools, err := r.Repository.Pools(ctx)
	if err != nil {
		return "", err
	}
	var result string
	for _, pool := range pools {
		if pool.NodeName != nodeName {
			continue
		}
		if result != "" {
			return "", fmt.Errorf("multiple ShiftPVPools are registered for node %q", nodeName)
		}
		result = filepath.Clean(pool.MountPath)
	}
	if !filepath.IsAbs(result) || result == "/" {
		return "", fmt.Errorf("node %q has no valid ShiftPVPool mountPath", nodeName)
	}
	return result, nil
}

func (r *Reconciler) deleteTransferResources(ctx context.Context, names resourceNames) error {
	var errs []error
	if err := r.Client.CoreV1().Pods(r.Namespace).Delete(ctx, names.SourcePod, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, err)
	}
	if err := r.Client.CoreV1().Services(r.Namespace).Delete(ctx, names.SourceService, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, err)
	}
	if err := r.Client.CoreV1().ConfigMaps(r.Namespace).Delete(ctx, names.Config, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, err)
	}
	if err := r.Client.CoreV1().Secrets(r.Namespace).Delete(ctx, names.Secret, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, err)
	}
	return errorsJoin(errs...)
}

func transferLabels(names resourceNames) map[string]string {
	return map[string]string{"app.kubernetes.io/name": "shiftpv", "app.kubernetes.io/component": "mobility", "shiftpv.io/move": names.Base}
}

func sourceLabels(names resourceNames) map[string]string {
	labels := transferLabels(names)
	labels["shiftpv.io/role"] = "source"
	return labels
}
