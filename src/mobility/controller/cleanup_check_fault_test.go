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
	ktesting "k8s.io/client-go/testing"

	"github.com/cagojeiger/ShiftPV/src/lifecycle/cleanup"
)

func TestCleanupCheckRecoversLostAPIResponses(t *testing.T) {
	for _, boundary := range []string{"start", "create", "bind", "finish", "ttl"} {
		for _, accepted := range []bool{false, true} {
			t.Run(boundary+map[bool]string{false: "-rejected", true: "-accepted"}[accepted], func(t *testing.T) {
				ctx := context.Background()
				r, repo, client, i := cleanupReviewFixture(t)
				steps := map[string]int{"start": 0, "create": 1, "bind": 1, "finish": 2, "ttl": 2}[boundary]
				for range steps {
					if err := r.sweepCleanup(ctx); err != nil {
						t.Fatal(err)
					}
				}
				record, _ := r.cleanupJournal().Get(ctx, i.MoveUID)
				if boundary == "finish" || boundary == "ttl" {
					job, _ := client.BatchV1().Jobs("system").Get(ctx, cleanupCheckName(record), metav1.GetOptions{})
					job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
					if _, err := client.BatchV1().Jobs("system").UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				verb, resource := "update", "configmaps"
				if boundary == "create" {
					verb, resource = "create", "jobs"
				} else if boundary == "ttl" {
					resource = "jobs"
				}
				injected := false
				client.PrependReactor(verb, resource, func(a ktesting.Action) (bool, runtime.Object, error) {
					if injected {
						return false, nil, nil
					}
					injected = true
					if accepted {
						var err error
						if verb == "create" {
							job := a.(ktesting.CreateAction).GetObject().(*batchv1.Job)
							job.UID = types.UID(job.Name + "-uid")
							err = client.Tracker().Create(a.GetResource(), job, a.GetNamespace())
						} else {
							err = client.Tracker().Update(a.GetResource(), a.(ktesting.UpdateAction).GetObject(), a.GetNamespace())
						}
						if err != nil {
							t.Fatal(err)
						}
					}
					return true, nil, errors.New("API response lost")
				})
				if err := r.sweepCleanup(ctx); err == nil || !injected {
					t.Fatal("fault not observed")
				}
				if boundary == "finish" && !accepted {
					job, _ := client.BatchV1().Jobs("system").Get(ctx, cleanupCheckName(record), metav1.GetOptions{})
					if job.Spec.TTLSecondsAfterFinished != nil {
						t.Fatal("unacknowledged proof can expire")
					}
				}
				// Recreate only the controller process, retaining Kubernetes state.
				r = &Reconciler{Client: client, Repository: repo, Namespace: r.Namespace, HelperImage: r.HelperImage}
				for attempt := range 4 {
					if err := r.sweepCleanup(ctx); err != nil {
						t.Fatal(err)
					}
					jobs, _ := client.BatchV1().Jobs("system").List(ctx, metav1.ListOptions{})
					if attempt == 0 && boundary == "start" && !accepted && len(jobs.Items) == 0 {
						continue // Durable intent precedes worker creation on the next pass.
					}
					if len(jobs.Items) != 1 {
						t.Fatalf("expected one attempt, got %d", len(jobs.Items))
					}
					job := &jobs.Items[0]
					job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
					if _, err := client.BatchV1().Jobs("system").UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				record, err := r.cleanupJournal().Get(ctx, i.MoveUID)
				if err != nil || !record.Completed || record.State != cleanup.Completed {
					t.Fatalf("did not converge: %+v %v", record, err)
				}
				job, _ := client.BatchV1().Jobs("system").Get(ctx, cleanupCheckName(record), metav1.GetOptions{})
				if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 600 || string(job.UID) != record.CheckJobUID {
					t.Fatal("completion proof/TTL did not converge")
				}
			})
		}
	}
}

func TestLostCleanupJobBeforePhaseWriteIsRecoverable(t *testing.T) {
	ctx := context.Background()
	r, repo, client := cleanupMoveFixture()
	r.Repository = &rejectedStatusRepository{memoryRepository: repo, rejectNext: true}
	if err := r.reconcileMove(ctx, repo.moves[0]); err == nil {
		t.Fatal("phase failure not injected")
	}
	move := repo.moves[0]
	if move.Status.Phase != "WaitingForDestinationPublish" {
		t.Fatal("phase unexpectedly persisted")
	}
	if err := client.BatchV1().Jobs("system").Delete(ctx, namesFor(move.Name).CleanupJob, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	r.Repository = repo
	if err := r.reconcileMove(ctx, move); err != nil {
		t.Fatal(err)
	}
	if repo.moves[0].Status.Phase != "Blocked" || repo.moves[0].Status.Reason != "CleanupFailed" || repo.volumes[move.Spec.VolumeID].ActiveMove != move.Name {
		t.Fatal("lost evidence did not retain authority in recoverable Blocked")
	}
	jobs, _ := client.BatchV1().Jobs("system").List(ctx, metav1.ListOptions{})
	if len(jobs.Items) != 0 {
		t.Fatal("destructive cleanup replayed")
	}
}

func TestCleanupCheckRejectsChangedExecution(t *testing.T) {
	for _, change := range []string{"uid", "image", "writable", "capability", "command", "retry", "deadline"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			r, _, client, i := cleanupReviewFixture(t)
			for range 2 {
				if err := r.sweepCleanup(ctx); err != nil {
					t.Fatal(err)
				}
			}
			record, _ := r.cleanupJournal().Get(ctx, i.MoveUID)
			job, _ := client.BatchV1().Jobs("system").Get(ctx, cleanupCheckName(record), metav1.GetOptions{})
			c := &job.Spec.Template.Spec.Containers[0]
			switch change {
			case "uid":
				job.UID = "replacement"
			case "image":
				c.Image = "different"
			case "writable":
				c.VolumeMounts[0].ReadOnly = false
			case "capability":
				c.SecurityContext.Capabilities.Add = []corev1.Capability{"DAC_OVERRIDE"}
			case "command":
				c.Command = []string{"true"}
			case "retry":
				*job.Spec.BackoffLimit = 1
			case "deadline":
				*job.Spec.ActiveDeadlineSeconds = 999
			}
			if err := validateCleanupCheck(job, record); err == nil {
				t.Fatal("changed execution accepted")
			}
		})
	}
}
