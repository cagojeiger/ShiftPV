package controller

import (
	"context"
	"errors"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/cagojeiger/ShiftPV/src/lifecycle/cleanup"
)

func assignJobUIDs(client *fake.Clientset) {
	client.PrependReactor("create", "jobs", func(action ktesting.Action) (bool, runtime.Object, error) {
		job := action.(ktesting.CreateAction).GetObject().(*batchv1.Job)
		job.UID = types.UID(job.Name + "-uid")
		return false, nil, nil
	})
}

func TestCleanupIntentWriteFailureNeverStartsDiskWork(t *testing.T) {
	r, repo, client := cleanupMoveFixture()
	client.PrependReactor("create", "configmaps", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("intent unavailable")
	})
	if err := r.reconcileMove(context.Background(), repo.moves[0]); err == nil {
		t.Fatal("lost intent write hidden")
	}
	jobs, err := client.BatchV1().Jobs("system").List(context.Background(), metav1.ListOptions{})
	if err != nil || len(jobs.Items) != 0 {
		t.Fatalf("disk work before request: %+v %v", jobs, err)
	}
}

func TestCleanupAcknowledgementSurvivesJobLoss(t *testing.T) {
	ctx := context.Background()
	r, repo, client := cleanupMoveFixture()
	if err := r.reconcileMove(ctx, repo.moves[0]); err != nil {
		t.Fatal(err)
	}
	move := repo.moves[0]
	job, err := client.BatchV1().Jobs("system").Get(ctx, namesFor(move.Name).CleanupJob, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if job.Spec.TTLSecondsAfterFinished != nil {
		t.Fatal("unacknowledged cleanup evidence expires")
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if _, err := client.BatchV1().Jobs("system").UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	r.Repository = &rejectedStatusRepository{memoryRepository: repo, rejectNext: true}
	if err := r.reconcileMove(ctx, move); err == nil {
		t.Fatal("expected journal error")
	}
	record, err := (cleanup.Journal{Client: client, Namespace: "system"}).Get(ctx, move.UID)
	if err != nil || !record.Completed {
		t.Fatalf("cleanup acknowledgement absent: %+v %v", record, err)
	}
	if err := client.BatchV1().Jobs("system").Delete(ctx, job.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	r.Repository = repo
	for range 2 {
		if err := r.reconcileMove(ctx, repo.moves[0]); err != nil {
			t.Fatal(err)
		}
	}
	if repo.moves[0].Status.Phase != "Succeeded" || repo.volumes[move.Spec.VolumeID].ActiveMove != "" {
		t.Fatal("acknowledged cleanup did not converge")
	}
	jobs, err := client.BatchV1().Jobs("system").List(ctx, metav1.ListOptions{})
	if err != nil || len(jobs.Items) != 0 {
		t.Fatalf("disk action replayed: %+v %v", jobs, err)
	}
}

func TestCleanupRejectsChangedPoolOrJobIncarnation(t *testing.T) {
	for _, change := range []string{"pool", "job", "owner", "move", "publisher"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			r, repo, client := cleanupMoveFixture()
			if err := r.reconcileMove(ctx, repo.moves[0]); err != nil {
				t.Fatal(err)
			}
			move := repo.moves[0]
			switch change {
			case "pool":
				repo.pools[0].MountPath = "/different-pool"
			case "job":
				job, err := client.BatchV1().Jobs("system").Get(ctx, namesFor(move.Name).CleanupJob, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				job.UID = "replacement"
				if _, err := client.BatchV1().Jobs("system").Update(ctx, job, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			default:
				state := repo.volumes[move.Spec.VolumeID]
				switch change {
				case "owner":
					state.OwnerNode = "source"
				case "move":
					state.ActiveMove = "next-move"
				case "publisher":
					state.PublishedNodes = []string{"source", "destination"}
				}
				repo.volumes[move.Spec.VolumeID] = state
			}
			if err := r.ensureCleanupJob(ctx, move, namesFor(move.Name)); err == nil {
				t.Fatal("changed authority accepted")
			}
		})
	}
}

func TestCleanupAcknowledgementFailureKeepsJobAndLock(t *testing.T) {
	ctx := context.Background()
	r, repo, client := cleanupMoveFixture()
	if err := r.reconcileMove(ctx, repo.moves[0]); err != nil {
		t.Fatal(err)
	}
	move := repo.moves[0]
	job, err := client.BatchV1().Jobs("system").Get(ctx, namesFor(move.Name).CleanupJob, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if _, err := client.BatchV1().Jobs("system").UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	failed := false
	client.PrependReactor("update", "configmaps", func(ktesting.Action) (bool, runtime.Object, error) {
		if failed {
			return false, nil, nil
		}
		failed = true
		return true, nil, errors.New("acknowledgement unavailable")
	})
	if err := r.reconcileMove(ctx, move); err == nil {
		t.Fatal("acknowledgement error hidden")
	}
	job, err = client.BatchV1().Jobs("system").Get(ctx, job.Name, metav1.GetOptions{})
	if err != nil || job.Spec.TTLSecondsAfterFinished != nil || repo.moves[0].Status.Phase != "CleaningSource" || repo.volumes[move.Spec.VolumeID].ActiveMove != move.Name {
		t.Fatalf("acknowledgement failure lost evidence/lock: %+v %v", job, err)
	}
	for range 2 {
		if err := r.reconcileMove(ctx, repo.moves[0]); err != nil {
			t.Fatal(err)
		}
	}
	if repo.moves[0].Status.Phase != "Succeeded" {
		t.Fatal("retry did not finish")
	}
}

func TestCleanupMissingBoundJobNeverRecreatesDiskWork(t *testing.T) {
	ctx := context.Background()
	r, repo, client := cleanupMoveFixture()
	if err := r.reconcileMove(ctx, repo.moves[0]); err != nil {
		t.Fatal(err)
	}
	move := repo.moves[0]
	if err := client.BatchV1().Jobs("system").Delete(ctx, namesFor(move.Name).CleanupJob, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileMove(ctx, move); err != nil {
		t.Fatal(err)
	}
	if repo.moves[0].Status.Phase != "Blocked" || repo.moves[0].Status.Reason != "CleanupFailed" {
		t.Fatal("lost evidence did not enter recoverable blocked state")
	}
	if err := r.ensureCleanupJob(ctx, move, namesFor(move.Name)); err == nil {
		t.Fatal("bound Job recreated")
	}
	jobs, err := client.BatchV1().Jobs("system").List(ctx, metav1.ListOptions{})
	if err != nil || len(jobs.Items) != 0 || repo.volumes[move.Spec.VolumeID].ActiveMove != move.Name {
		t.Fatalf("lost worker restarted: %+v %v", jobs, err)
	}
}

func TestCleanupJournalIsNotPolledBeforeCleanup(t *testing.T) {
	r, repo, client := cleanupMoveFixture()
	move := repo.moves[0]
	move.Status.Phase = "Copying"
	client.PrependReactor("get", "configmaps", func(ktesting.Action) (bool, runtime.Object, error) {
		t.Error("cleanup journal was read before cleanup")
		return true, nil, errors.New("unrelated journal unavailable")
	})
	if _, err := r.observe(context.Background(), move); err != nil {
		t.Fatal(err)
	}
}
