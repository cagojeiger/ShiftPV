package controller

import (
	"context"
	"errors"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
)

func TestCleanupRejectsChangedExecutionBeforeAcknowledgement(t *testing.T) {
	changes := map[string]func(*batchv1.Job){
		"image":            func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].Image = "other" },
		"restart":          func(j *batchv1.Job) { j.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyOnFailure },
		"retry":            func(j *batchv1.Job) { *j.Spec.BackoffLimit = 20 },
		"missing retry":    func(j *batchv1.Job) { j.Spec.BackoffLimit = nil },
		"deadline":         func(j *batchv1.Job) { *j.Spec.ActiveDeadlineSeconds = 9999 },
		"missing deadline": func(j *batchv1.Job) { j.Spec.ActiveDeadlineSeconds = nil },
		"security": func(j *batchv1.Job) {
			j.Spec.Template.Spec.Containers[0].SecurityContext.Privileged = boolPointer(true)
		},
		"missing security": func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].SecurityContext = nil },
		"pod security": func(j *batchv1.Job) {
			j.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsUser: int64Pointer(123)}
		},
		"init":         func(j *batchv1.Job) { j.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "extra"}} },
		"ephemeral":    func(j *batchv1.Job) { j.Spec.Template.Spec.EphemeralContainers = []corev1.EphemeralContainer{{}} },
		"host pid":     func(j *batchv1.Job) { j.Spec.Template.Spec.HostPID = true },
		"host ipc":     func(j *batchv1.Job) { j.Spec.Template.Spec.HostIPC = true },
		"host network": func(j *batchv1.Job) { j.Spec.Template.Spec.HostNetwork = true },
		"args":         func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].Args = []string{"extra"} },
		"env from": func(j *batchv1.Job) {
			j.Spec.Template.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{Prefix: "extra"}}
		},
		"lifecycle": func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].Lifecycle = &corev1.Lifecycle{} },
		"probe":     func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].LivenessProbe = &corev1.Probe{} },
		"host path type": func(j *batchv1.Job) {
			j.Spec.Template.Spec.Volumes[0].HostPath.Type = hostPathTypePointer(corev1.HostPathDirectoryOrCreate)
		},
		"mount":   func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath = "/other" },
		"command": func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].Command = []string{"true"} },
		"env":     func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].Env[0].Value = "other" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r, repo, client := cleanupMoveFixture()
			move := repo.moves[0]
			if err := r.ensureCleanupJob(ctx, move, namesFor(move.Name)); err != nil {
				t.Fatal(err)
			}
			job, err := client.BatchV1().Jobs(r.Namespace).Get(ctx, namesFor(move.Name).CleanupJob, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			change(job)
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
			if _, err := client.BatchV1().Jobs(r.Namespace).Update(ctx, job, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := r.ensureCleanupJob(ctx, move, namesFor(move.Name)); err == nil {
				t.Fatal("changed execution adopted")
			}
			if err := r.acknowledgeCleanup(ctx, move); err == nil {
				t.Fatal("changed execution acknowledged")
			}
			record, err := r.cleanupJournal().Get(ctx, move.UID)
			if err != nil || record.Completed || repo.volumes[move.Spec.VolumeID].ActiveMove != move.Name {
				t.Fatalf("changed execution lost cleanup obligation: %+v %v", record, err)
			}
		})
	}
}

func TestCleanupRetainsApprovedImageAcrossRestart(t *testing.T) {
	ctx := context.Background()
	r, repo, client := cleanupMoveFixture()
	move, image := repo.moves[0], r.HelperImage
	failed := false
	client.PrependReactor("create", "jobs", func(ktesting.Action) (bool, runtime.Object, error) {
		if failed {
			return false, nil, nil
		}
		failed = true
		return true, nil, errors.New("create response unavailable")
	})
	if err := r.ensureCleanupJob(ctx, move, namesFor(move.Name)); err == nil {
		t.Fatal("create failure hidden")
	}
	r.HelperImage = "new-helper"
	if err := r.ensureCleanupJob(ctx, move, namesFor(move.Name)); err != nil {
		t.Fatal(err)
	}
	job, err := client.BatchV1().Jobs(r.Namespace).Get(ctx, namesFor(move.Name).CleanupJob, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if job.Spec.Template.Spec.Containers[0].Image != image {
		t.Fatal("approved image changed after restart")
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if _, err := client.BatchV1().Jobs(r.Namespace).UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.acknowledgeCleanup(ctx, move); err != nil {
		t.Fatal(err)
	}
}
