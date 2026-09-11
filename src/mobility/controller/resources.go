package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
)

type resourceNames struct {
	Base          string
	PlacementPod  string
	Secret        string
	Config        string
	SourcePod     string
	SourceService string
	CopyJob       string
	PromotionJob  string
}

func namesFor(moveName string) resourceNames {
	sum := sha256.Sum256([]byte(moveName))
	base := "shiftpv-move-" + hex.EncodeToString(sum[:6])
	return resourceNames{
		Base: base, PlacementPod: base + "-placement", Secret: base + "-auth", Config: base + "-config", SourcePod: base + "-source",
		SourceService: base + "-source", CopyJob: base + "-copy", PromotionJob: base + "-promote",
	}
}

func (r *Reconciler) ensureCopyResources(ctx context.Context, move volumeapi.Move, names resourceNames) error {
	if err := r.ensureTransferSecret(ctx, move, names); err != nil {
		return err
	}
	if err := r.ensureRsyncConfig(ctx, move, names); err != nil {
		return err
	}
	if err := r.ensureSourcePod(ctx, move, names); err != nil {
		return err
	}
	if err := r.ensureSourceService(ctx, move, names); err != nil {
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

func (r *Reconciler) ensureTransferSecret(ctx context.Context, move volumeapi.Move, names resourceNames) error {
	if move.UID == "" {
		return fmt.Errorf("rsync Secret requires ShiftPVMove UID")
	}
	passwordBytes := make([]byte, 24)
	if _, err := rand.Read(passwordBytes); err != nil {
		return fmt.Errorf("generate rsync password: %w", err)
	}
	password := hex.EncodeToString(passwordBytes)
	expected := &corev1.Secret{
		ObjectMeta: moveObjectMeta(move, names.Secret, r.Namespace, transferLabels(names, move)),
		Data:       map[string][]byte{"password": []byte(password), "secrets": []byte("shiftpv:" + password + "\n")},
	}
	current, err := r.Client.CoreV1().Secrets(r.Namespace).Create(ctx, expected, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		current, err = r.Client.CoreV1().Secrets(r.Namespace).Get(ctx, names.Secret, metav1.GetOptions{})
	}
	if err != nil {
		return fmt.Errorf("create rsync Secret: %w", err)
	}
	if !sameMoveObject(&current.ObjectMeta, &expected.ObjectMeta) || !validTransferSecret(current) {
		return fmt.Errorf("rsync Secret %q identity changed", names.Secret)
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
	expected := &corev1.ConfigMap{
		ObjectMeta: moveObjectMeta(move, names.Config, r.Namespace, transferLabels(names, move)),
		Data:       map[string]string{"rsyncd.conf": configuration},
	}
	current, err := r.Client.CoreV1().ConfigMaps(r.Namespace).Create(ctx, expected, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		current, err = r.Client.CoreV1().ConfigMaps(r.Namespace).Get(ctx, names.Config, metav1.GetOptions{})
	}
	if err != nil {
		return fmt.Errorf("create rsync ConfigMap: %w", err)
	}
	if !sameMoveObject(&current.ObjectMeta, &expected.ObjectMeta) || !reflect.DeepEqual(current.Data, expected.Data) || len(current.BinaryData) != 0 {
		return fmt.Errorf("rsync ConfigMap %q identity changed", names.Config)
	}
	return nil
}

func (r *Reconciler) ensureSourcePod(ctx context.Context, move volumeapi.Move, names resourceNames) error {
	secretMode := int32(0o400)
	configMode := int32(0o644)
	poolRoot, err := r.poolMountPath(ctx, move.Spec.SourceNode)
	if err != nil {
		return err
	}
	expected := &corev1.Pod{
		ObjectMeta: moveObjectMeta(move, names.SourcePod, r.Namespace, sourceLabels(names, move)),
		Spec: corev1.PodSpec{
			NodeName: move.Spec.SourceNode, ServiceAccountName: r.ServiceAccountName,
			RestartPolicy: corev1.RestartPolicyAlways,
			Containers: []corev1.Container{{
				Name: "rsync", Image: r.HelperImage, ImagePullPolicy: corev1.PullIfNotPresent,
				Command: []string{"/shiftpv-volume-helper"}, Args: []string{
					"serve-source", "--move-name=" + move.Name, "--move-uid=" + move.UID,
					"--operation-id=" + move.Status.CopyOperationID, "--namespace=" + r.Namespace,
				},
				Env:                    podNameEnvironment(),
				Ports:                  []corev1.ContainerPort{{Name: "rsync", ContainerPort: 873, Protocol: corev1.ProtocolTCP}},
				ReadinessProbe:         &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstrFromInt(873)}}, InitialDelaySeconds: 1, TimeoutSeconds: 1, PeriodSeconds: 1, SuccessThreshold: 1, FailureThreshold: 3},
				SecurityContext:        &corev1.SecurityContext{AllowPrivilegeEscalation: boolPointer(false), RunAsUser: int64Pointer(0)},
				TerminationMessagePath: "/dev/termination-log", TerminationMessagePolicy: corev1.TerminationMessageReadFile,
				VolumeMounts: []corev1.VolumeMount{
					{Name: "pool", MountPath: "/pool", ReadOnly: true},
					{Name: "auth", MountPath: "/auth", ReadOnly: true},
					{Name: "config", MountPath: "/config", ReadOnly: true},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "pool", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: poolRoot, Type: hostPathTypePointer(corev1.HostPathDirectory)}}},
				{Name: "auth", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: names.Secret, DefaultMode: &secretMode}}},
				{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: names.Config}, DefaultMode: &configMode}}},
			},
		},
	}
	current, err := r.Client.CoreV1().Pods(r.Namespace).Create(ctx, expected, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		current, err = r.Client.CoreV1().Pods(r.Namespace).Get(ctx, names.SourcePod, metav1.GetOptions{})
	}
	if err != nil {
		return fmt.Errorf("create rsync source Pod: %w", err)
	}
	if !sameMoveObject(&current.ObjectMeta, &expected.ObjectMeta) || !sameSourcePod(current, expected) {
		return fmt.Errorf("rsync source Pod %q identity changed", names.SourcePod)
	}
	return nil
}

func (r *Reconciler) ensureSourceService(ctx context.Context, move volumeapi.Move, names resourceNames) error {
	expected := &corev1.Service{
		ObjectMeta: moveObjectMeta(move, names.SourceService, r.Namespace, transferLabels(names, move)),
		Spec:       corev1.ServiceSpec{Selector: sourceLabels(names, move), Ports: []corev1.ServicePort{{Name: "rsync", Port: 873, TargetPort: intstrFromInt(873)}}},
	}
	current, err := r.Client.CoreV1().Services(r.Namespace).Create(ctx, expected, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		current, err = r.Client.CoreV1().Services(r.Namespace).Get(ctx, names.SourceService, metav1.GetOptions{})
	}
	if err != nil {
		return fmt.Errorf("create rsync source Service: %w", err)
	}
	if !sameMoveObject(&current.ObjectMeta, &expected.ObjectMeta) || !reflect.DeepEqual(current.Spec.Selector, expected.Spec.Selector) || !sameServicePorts(current.Spec.Ports, expected.Spec.Ports) {
		return fmt.Errorf("rsync source Service %q identity changed", names.SourceService)
	}
	return nil
}

func (r *Reconciler) ensureCopyJob(ctx context.Context, move volumeapi.Move, names resourceNames) error {
	if move.Status.IncomingCopy == nil || move.Status.CopyOperationID == "" {
		return fmt.Errorf("copy identity is missing")
	}
	command := []string{"/shiftpv-volume-helper"}
	args := []string{
		"copy", "--move-name=" + move.Name, "--move-uid=" + move.UID,
		"--operation-id=" + move.Status.CopyOperationID, "--namespace=" + r.Namespace,
		"--source-service=" + names.SourceService, "--password-file=/auth/password",
	}
	secretMode := int32(0o644)
	return r.ensureJob(ctx, move, names.CopyJob, move.Status.DestinationNode, names, command, args, nil,
		[]corev1.VolumeMount{{Name: "auth", MountPath: "/auth", ReadOnly: true}},
		[]corev1.Volume{{Name: "auth", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: names.Secret, DefaultMode: &secretMode}}}})
}

func (r *Reconciler) ensurePromotionJob(ctx context.Context, move volumeapi.Move, names resourceNames) error {
	if move.Status.IncomingCopy == nil || move.Status.DestinationCopy == nil || move.Status.PromotionOperationID == "" {
		return fmt.Errorf("promotion identity is missing")
	}
	return r.ensureJob(ctx, move, names.PromotionJob, move.Status.DestinationNode, names,
		[]string{"/shiftpv-volume-helper"}, []string{
			"promote", "--move-name=" + move.Name, "--move-uid=" + move.UID,
			"--operation-id=" + move.Status.PromotionOperationID, "--namespace=" + r.Namespace,
		}, nil, nil, nil)
}

func (r *Reconciler) ensureJob(ctx context.Context, move volumeapi.Move, name, nodeName string, names resourceNames, command, args []string, env []corev1.EnvVar, extraMounts []corev1.VolumeMount, extraVolumes []corev1.Volume) error {
	env = append(append([]corev1.EnvVar{}, env...), podNameEnvironment()...)
	job, err := r.operationJob(ctx, name, nodeName, names, env, extraMounts, extraVolumes)
	if err != nil {
		return err
	}
	job.OwnerReferences = []metav1.OwnerReference{{APIVersion: "shiftpv.io/v1alpha1", Kind: "ShiftPVMove", Name: move.Name, UID: types.UID(move.UID), Controller: boolPointer(true), BlockOwnerDeletion: boolPointer(true)}}
	job.Labels["shiftpv.io/move-uid"] = move.UID
	job.Spec.Template.Labels["shiftpv.io/move-uid"] = move.UID
	job.Spec.Template.Spec.Containers[0].Command = command
	job.Spec.Template.Spec.Containers[0].Args = args
	created, err := r.Client.BatchV1().Jobs(r.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		created, err = r.Client.BatchV1().Jobs(r.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil && !sameOperationJob(created, job) {
			return fmt.Errorf("mobility Job %q identity changed", name)
		}
	}
	if err != nil {
		return fmt.Errorf("create mobility Job %q: %w", name, err)
	}
	return nil
}

func (r *Reconciler) operationJob(ctx context.Context, name, nodeName string, names resourceNames, env []corev1.EnvVar, extraMounts []corev1.VolumeMount, extraVolumes []corev1.Volume) (*batchv1.Job, error) {
	backoff := int32(2)
	ttl := int32(600)
	deadline := int64(300)
	poolRoot, err := r.poolMountPath(ctx, nodeName)
	if err != nil {
		return nil, err
	}
	mounts := append([]corev1.VolumeMount{{Name: "pool", MountPath: "/pool"}}, extraMounts...)
	volumes := append([]corev1.Volume{{Name: "pool", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: poolRoot, Type: hostPathTypePointer(corev1.HostPathDirectory)}}}}, extraVolumes...)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Namespace, Labels: transferLabels(names)},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff, TTLSecondsAfterFinished: &ttl, ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: transferLabels(names)},
				Spec: corev1.PodSpec{NodeName: nodeName, ServiceAccountName: r.ServiceAccountName, RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{Name: "operation", Image: r.HelperImage, Env: env, VolumeMounts: mounts,
						SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: boolPointer(false), RunAsUser: int64Pointer(0)}}},
					Volumes: volumes,
				},
			},
		},
	}, nil
}

func podNameEnvironment() []corev1.EnvVar {
	return []corev1.EnvVar{{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"}}}}
}

func sameOperationJob(current, expected *batchv1.Job) bool {
	if current == nil || expected == nil || current.DeletionTimestamp != nil || current.Name != expected.Name || current.Namespace != expected.Namespace ||
		!reflect.DeepEqual(current.OwnerReferences, expected.OwnerReferences) || current.Labels["shiftpv.io/move-uid"] != expected.Labels["shiftpv.io/move-uid"] ||
		!reflect.DeepEqual(current.Spec.BackoffLimit, expected.Spec.BackoffLimit) || !reflect.DeepEqual(current.Spec.ActiveDeadlineSeconds, expected.Spec.ActiveDeadlineSeconds) {
		return false
	}
	actual, wanted := current.Spec.Template.Spec, expected.Spec.Template.Spec
	if actual.NodeName != wanted.NodeName || actual.ServiceAccountName != wanted.ServiceAccountName || actual.RestartPolicy != wanted.RestartPolicy ||
		actual.HostPID || actual.HostIPC || actual.HostNetwork || len(actual.InitContainers) != 0 || len(actual.EphemeralContainers) != 0 ||
		len(actual.Containers) != 1 || len(wanted.Containers) != 1 || !reflect.DeepEqual(actual.Volumes, wanted.Volumes) {
		return false
	}
	a, w := actual.Containers[0], wanted.Containers[0]
	return a.Name == w.Name && a.Image == w.Image && reflect.DeepEqual(a.Command, w.Command) && reflect.DeepEqual(a.Args, w.Args) &&
		reflect.DeepEqual(a.Env, w.Env) && len(a.EnvFrom) == 0 && reflect.DeepEqual(a.Resources, w.Resources) &&
		reflect.DeepEqual(a.VolumeMounts, w.VolumeMounts) && reflect.DeepEqual(a.SecurityContext, w.SecurityContext) &&
		len(a.VolumeDevices) == 0 && a.Lifecycle == nil && a.LivenessProbe == nil && a.ReadinessProbe == nil && a.StartupProbe == nil
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

func (r *Reconciler) deleteTransferResources(ctx context.Context, move volumeapi.Move, names resourceNames) error {
	var errs []error
	if pod, err := r.Client.CoreV1().Pods(r.Namespace).Get(ctx, names.SourcePod, metav1.GetOptions{}); err == nil {
		if !moveOwned(pod.OwnerReferences, move.UID) || pod.Labels["shiftpv.io/move-uid"] != move.UID {
			errs = append(errs, fmt.Errorf("refusing to delete unrelated Pod %q", names.SourcePod))
		} else if uid := pod.UID; uid == "" {
			errs = append(errs, fmt.Errorf("source Pod %q has no UID", names.SourcePod))
		} else if err := r.Client.CoreV1().Pods(r.Namespace).Delete(ctx, names.SourcePod, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	} else if !apierrors.IsNotFound(err) {
		errs = append(errs, err)
	}
	if service, err := r.Client.CoreV1().Services(r.Namespace).Get(ctx, names.SourceService, metav1.GetOptions{}); err == nil {
		if !moveOwned(service.OwnerReferences, move.UID) || service.Labels["shiftpv.io/move-uid"] != move.UID {
			errs = append(errs, fmt.Errorf("refusing to delete unrelated Service %q", names.SourceService))
		} else if uid := service.UID; uid == "" {
			errs = append(errs, fmt.Errorf("source Service %q has no UID", names.SourceService))
		} else if err := r.Client.CoreV1().Services(r.Namespace).Delete(ctx, names.SourceService, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	} else if !apierrors.IsNotFound(err) {
		errs = append(errs, err)
	}
	if config, err := r.Client.CoreV1().ConfigMaps(r.Namespace).Get(ctx, names.Config, metav1.GetOptions{}); err == nil {
		if !moveOwned(config.OwnerReferences, move.UID) || config.Labels["shiftpv.io/move-uid"] != move.UID {
			errs = append(errs, fmt.Errorf("refusing to delete unrelated ConfigMap %q", names.Config))
		} else if uid := config.UID; uid == "" {
			errs = append(errs, fmt.Errorf("rsync ConfigMap %q has no UID", names.Config))
		} else if err := r.Client.CoreV1().ConfigMaps(r.Namespace).Delete(ctx, names.Config, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	} else if !apierrors.IsNotFound(err) {
		errs = append(errs, err)
	}
	if secret, err := r.Client.CoreV1().Secrets(r.Namespace).Get(ctx, names.Secret, metav1.GetOptions{}); err == nil {
		if !moveOwned(secret.OwnerReferences, move.UID) || secret.Labels["shiftpv.io/move-uid"] != move.UID {
			errs = append(errs, fmt.Errorf("refusing to delete unrelated Secret %q", names.Secret))
		} else if uid := secret.UID; uid == "" {
			errs = append(errs, fmt.Errorf("rsync Secret %q has no UID", names.Secret))
		} else if err := r.Client.CoreV1().Secrets(r.Namespace).Delete(ctx, names.Secret, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	} else if !apierrors.IsNotFound(err) {
		errs = append(errs, err)
	}
	return errorsJoin(errs...)
}

func transferLabels(names resourceNames, moves ...volumeapi.Move) map[string]string {
	labels := map[string]string{"app.kubernetes.io/name": "shiftpv", "app.kubernetes.io/component": "mobility", "shiftpv.io/move": names.Base}
	if len(moves) == 1 && moves[0].UID != "" {
		labels["shiftpv.io/move-uid"] = moves[0].UID
	}
	return labels
}

func sourceLabels(names resourceNames, moves ...volumeapi.Move) map[string]string {
	labels := transferLabels(names, moves...)
	labels["shiftpv.io/role"] = "source"
	return labels
}

func moveObjectMeta(move volumeapi.Move, name, namespace string, labels map[string]string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name: name, Namespace: namespace, Labels: labels,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "shiftpv.io/v1alpha1", Kind: "ShiftPVMove", Name: move.Name, UID: types.UID(move.UID),
			Controller: boolPointer(true), BlockOwnerDeletion: boolPointer(true),
		}},
	}
}

func sameMoveObject(current, expected *metav1.ObjectMeta) bool {
	return current != nil && expected != nil && current.DeletionTimestamp == nil &&
		current.Name == expected.Name && current.Namespace == expected.Namespace &&
		current.Labels["shiftpv.io/move"] == expected.Labels["shiftpv.io/move"] &&
		current.Labels["shiftpv.io/move-uid"] == expected.Labels["shiftpv.io/move-uid"] &&
		reflect.DeepEqual(current.OwnerReferences, expected.OwnerReferences)
}

func validTransferSecret(secret *corev1.Secret) bool {
	if secret == nil || len(secret.Data) != 2 || len(secret.Data["password"]) == 0 {
		return false
	}
	return string(secret.Data["secrets"]) == "shiftpv:"+string(secret.Data["password"])+"\n"
}

func sameSourcePod(current, expected *corev1.Pod) bool {
	if current == nil || expected == nil {
		return false
	}
	a, w := current.Spec, expected.Spec
	serviceAccountVolume, volumesMatch := sameSourceVolumes(a.Volumes, w.Volumes)
	if a.NodeName != w.NodeName || a.ServiceAccountName != w.ServiceAccountName || a.RestartPolicy != w.RestartPolicy || a.HostPID || a.HostIPC || a.HostNetwork ||
		len(a.InitContainers) != 0 || len(a.EphemeralContainers) != 0 || len(a.Containers) != 1 || len(w.Containers) != 1 || !volumesMatch {
		return false
	}
	ac, wc := a.Containers[0], w.Containers[0]
	return ac.Name == wc.Name && ac.Image == wc.Image && reflect.DeepEqual(ac.Command, wc.Command) && reflect.DeepEqual(ac.Args, wc.Args) &&
		reflect.DeepEqual(ac.Env, wc.Env) && len(ac.EnvFrom) == 0 && reflect.DeepEqual(ac.Ports, wc.Ports) && reflect.DeepEqual(ac.ReadinessProbe, wc.ReadinessProbe) &&
		ac.ImagePullPolicy == wc.ImagePullPolicy && reflect.DeepEqual(ac.Resources, wc.Resources) &&
		ac.TerminationMessagePath == wc.TerminationMessagePath && ac.TerminationMessagePolicy == wc.TerminationMessagePolicy &&
		reflect.DeepEqual(ac.SecurityContext, wc.SecurityContext) && sameSourceVolumeMounts(ac.VolumeMounts, wc.VolumeMounts, serviceAccountVolume) && len(ac.VolumeDevices) == 0 &&
		ac.Lifecycle == nil && ac.LivenessProbe == nil && ac.StartupProbe == nil
}

func sameSourceVolumes(current, expected []corev1.Volume) (string, bool) {
	if len(current) != len(expected) && len(current) != len(expected)+1 {
		return "", false
	}
	remaining := make(map[string]corev1.Volume, len(current))
	for _, volume := range current {
		if volume.Name == "" {
			return "", false
		}
		remaining[volume.Name] = volume
	}
	for _, wanted := range expected {
		actual, ok := remaining[wanted.Name]
		if !ok || !reflect.DeepEqual(actual, wanted) {
			return "", false
		}
		delete(remaining, wanted.Name)
	}
	if len(remaining) == 0 {
		return "", true
	}
	if len(remaining) != 1 {
		return "", false
	}
	for name, volume := range remaining {
		return name, validServiceAccountProjection(volume)
	}
	return "", false
}

func validServiceAccountProjection(volume corev1.Volume) bool {
	if !strings.HasPrefix(volume.Name, "kube-api-access-") || volume.Projected == nil || len(volume.Projected.Sources) != 3 ||
		volume.Projected.DefaultMode == nil || *volume.Projected.DefaultMode != 0o644 {
		return false
	}
	var token, rootCA, namespace bool
	for _, source := range volume.Projected.Sources {
		switch {
		case source.ServiceAccountToken != nil:
			projection := source.ServiceAccountToken
			token = projection.Path == "token" && projection.Audience == "" && projection.ExpirationSeconds != nil && *projection.ExpirationSeconds > 0
		case source.ConfigMap != nil:
			projection := source.ConfigMap
			rootCA = projection.Name == "kube-root-ca.crt" && len(projection.Items) == 1 && projection.Items[0].Key == "ca.crt" && projection.Items[0].Path == "ca.crt"
		case source.DownwardAPI != nil:
			projection := source.DownwardAPI
			namespace = len(projection.Items) == 1 && projection.Items[0].Path == "namespace" && projection.Items[0].FieldRef != nil &&
				projection.Items[0].FieldRef.APIVersion == "v1" && projection.Items[0].FieldRef.FieldPath == "metadata.namespace"
		default:
			return false
		}
	}
	return token && rootCA && namespace
}

func sameSourceVolumeMounts(current, expected []corev1.VolumeMount, serviceAccountVolume string) bool {
	if len(current) != len(expected) && (serviceAccountVolume == "" || len(current) != len(expected)+1) {
		return false
	}
	remaining := make(map[string]corev1.VolumeMount, len(current))
	for _, mount := range current {
		if mount.Name == "" {
			return false
		}
		remaining[mount.Name] = mount
	}
	for _, wanted := range expected {
		actual, ok := remaining[wanted.Name]
		if !ok || !reflect.DeepEqual(actual, wanted) {
			return false
		}
		delete(remaining, wanted.Name)
	}
	if serviceAccountVolume == "" {
		return len(remaining) == 0
	}
	mount, ok := remaining[serviceAccountVolume]
	return ok && len(remaining) == 1 && mount.MountPath == "/var/run/secrets/kubernetes.io/serviceaccount" && mount.ReadOnly &&
		mount.SubPath == "" && mount.SubPathExpr == "" && mount.MountPropagation == nil
}

func sameServicePorts(current, expected []corev1.ServicePort) bool {
	if len(current) != len(expected) {
		return false
	}
	for i := range current {
		if current[i].Name != expected[i].Name || current[i].Port != expected[i].Port || current[i].TargetPort != expected[i].TargetPort ||
			(current[i].Protocol != "" && current[i].Protocol != corev1.ProtocolTCP) {
			return false
		}
	}
	return true
}
