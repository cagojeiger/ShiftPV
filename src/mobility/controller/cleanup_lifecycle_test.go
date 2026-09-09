package controller

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cagojeiger/ShiftPV/src/lifecycle/cleanup"
)

func TestCleanupCheckClosesRecoveredObligationWithoutDeletingFiles(t *testing.T) {
	ctx := context.Background()
	r, repo, client := cleanupMoveFixture()
	move := repo.moves[0]
	intent := cleanupIntent(move, repo.pools[0].MountPath)
	j := r.cleanupJournal()
	if _, err := j.Ensure(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if err := j.BindJob(ctx, intent, "lost-original-job"); err != nil {
		t.Fatal(err)
	}
	state := repo.volumes[move.Spec.VolumeID]
	state.ActiveMove = ""
	repo.volumes[move.Spec.VolumeID] = state
	repo.moves[0].Status.Phase, repo.moves[0].Status.RecoveryPhase = "Blocked", "Recovered"
	cm, _ := client.CoreV1().ConfigMaps("system").Get(ctx, cleanup.Name(move.UID), metav1.GetOptions{})
	cm.Annotations[cleanup.CheckRequestKey] = "1"
	client.CoreV1().ConfigMaps("system").Update(ctx, cm, metav1.UpdateOptions{})
	for range 2 {
		if err := r.sweepCleanup(ctx); err != nil {
			t.Fatal(err)
		}
	}
	record, _ := j.Get(ctx, move.UID)
	jobs, _ := client.BatchV1().Jobs("system").List(ctx, metav1.ListOptions{})
	if len(jobs.Items) != 1 {
		t.Fatalf("checks=%d", len(jobs.Items))
	}
	job := &jobs.Items[0]
	if !job.Spec.Template.Spec.Containers[0].VolumeMounts[0].ReadOnly || job.Spec.Template.Spec.Containers[0].Command[0] != cleanupCheckBinary {
		t.Fatal("check is not read-only")
	}
	if record.CheckJobUID != string(job.UID) {
		t.Fatal("check UID not durable")
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	client.BatchV1().Jobs("system").UpdateStatus(ctx, job, metav1.UpdateOptions{})
	if err := r.sweepCleanup(ctx); err != nil {
		t.Fatal(err)
	}
	record, _ = j.Get(ctx, move.UID)
	if !record.Completed || record.CompletedAt.IsZero() {
		t.Fatalf("unresolved: %+v", record)
	}
	client.BatchV1().Jobs("system").Delete(ctx, job.Name, metav1.DeleteOptions{})
	if err := r.sweepCleanup(ctx); err != nil {
		t.Fatal(err)
	}
	jobs, _ = client.BatchV1().Jobs("system").List(ctx, metav1.ListOptions{})
	if len(jobs.Items) != 0 {
		t.Fatal("check replayed after completion")
	}
}

func TestCleanupSweepDoesNotRunOnEveryWake(t *testing.T) {
	ctx := context.Background()
	r, _, client := cleanupMoveFixture()
	now := time.Now()
	r.Now = func() time.Time { return now }
	if err := r.reconcileCleanupLifecycle(ctx); err != nil {
		t.Fatal(err)
	}
	before := len(client.Actions())
	for range 10 {
		if err := r.reconcileCleanupLifecycle(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(client.Actions()) != before {
		t.Fatal("cleanup scanned on every wake")
	}
	now = now.Add(time.Minute)
	if err := r.reconcileCleanupLifecycle(ctx); err != nil {
		t.Fatal(err)
	}
	if len(client.Actions()) == before {
		t.Fatal("cleanup scan never resumed")
	}
}
