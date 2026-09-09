package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/cagojeiger/ShiftPV/src/lifecycle/cleanup"
)

func cleanupReviewFixture(t *testing.T) (*Reconciler, *memoryRepository, *fake.Clientset, cleanup.Intent) {
	t.Helper()
	ctx := context.Background()
	r, repo, client := cleanupMoveFixture()
	move := repo.moves[0]
	i := cleanupIntent(move, repo.pools[0].MountPath)
	if _, err := r.cleanupJournal().Ensure(ctx, i); err != nil {
		t.Fatal(err)
	}
	if err := r.cleanupJournal().BindJob(ctx, i, "original"); err != nil {
		t.Fatal(err)
	}
	state := repo.volumes[i.VolumeID]
	state.ActiveMove = ""
	repo.volumes[i.VolumeID] = state
	repo.moves[0].Status.Phase, repo.moves[0].Status.RecoveryPhase = "Blocked", "Recovered"
	requestCleanupCheck(t, client, i, "1")
	return r, repo, client, i
}

func requestCleanupCheck(t *testing.T, client *fake.Clientset, i cleanup.Intent, sequence string) {
	t.Helper()
	cm, err := client.CoreV1().ConfigMaps("system").Get(context.Background(), cleanup.Name(i.MoveUID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cm.Annotations == nil {
		cm.Annotations = map[string]string{}
	}
	cm.Annotations[cleanup.CheckRequestKey] = sequence
	if _, err := client.CoreV1().ConfigMaps("system").Update(context.Background(), cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupCheckFailureRequiresIncreasingSequence(t *testing.T) {
	ctx := context.Background()
	r, _, client, i := cleanupReviewFixture(t)
	for range 2 {
		if err := r.sweepCleanup(ctx); err != nil {
			t.Fatal(err)
		}
	}
	record, _ := r.cleanupJournal().Get(ctx, i.MoveUID)
	job, _ := client.BatchV1().Jobs("system").Get(ctx, cleanupCheckName(record), metav1.GetOptions{})
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	client.BatchV1().Jobs("system").UpdateStatus(ctx, job, metav1.UpdateOptions{})
	if err := r.sweepCleanup(ctx); err != nil {
		t.Fatal(err)
	}
	client.BatchV1().Jobs("system").Delete(ctx, job.Name, metav1.DeleteOptions{})
	for range 2 {
		if err := r.sweepCleanup(ctx); err != nil {
			t.Fatal(err)
		}
	}
	jobs, _ := client.BatchV1().Jobs("system").List(ctx, metav1.ListOptions{})
	if len(jobs.Items) != 0 {
		t.Fatal("failed check replayed")
	}
	record, _ = r.cleanupJournal().Get(ctx, i.MoveUID)
	if record.Completed || record.State != cleanup.NeedsReview {
		t.Fatal("failed proof acknowledged")
	}
	requestCleanupCheck(t, client, i, "2")
	for range 2 {
		if err := r.sweepCleanup(ctx); err != nil {
			t.Fatal(err)
		}
	}
	record, _ = r.cleanupJournal().Get(ctx, i.MoveUID)
	client.BatchV1().Jobs("system").Delete(ctx, cleanupCheckName(record), metav1.DeleteOptions{})
	if err := r.sweepCleanup(ctx); err != nil {
		t.Fatal(err)
	}
	requestCleanupCheck(t, client, i, "1")
	if err := r.sweepCleanup(ctx); err == nil {
		t.Fatal("older sequence accepted")
	}
	jobs, _ = client.BatchV1().Jobs("system").List(ctx, metav1.ListOptions{})
	if len(jobs.Items) != 0 {
		t.Fatal("missing check recreated")
	}
}

func TestCleanupCheckAuthorityAndParentLoss(t *testing.T) {
	for _, scenario := range []string{"owner-source", "active", "publication", "pool", "move-uid", "node", "next-owner", "parent-loss", "api-timeout"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			r, repo, client, i := cleanupReviewFixture(t)
			state := repo.volumes[i.VolumeID]
			switch scenario {
			case "owner-source":
				state.OwnerNode = i.SourceNode
			case "active":
				state.ActiveMove = "other"
			case "publication":
				state.PublishedNodes = []string{i.SourceNode}
			case "pool":
				repo.pools[0].MountPath = "/replacement"
			case "move-uid":
				repo.moves[0].UID = "replacement"
			case "node":
				node, _ := client.CoreV1().Nodes().Get(ctx, i.SourceNode, metav1.GetOptions{})
				node.Status.Conditions = nil
				client.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
			case "next-owner":
				state.OwnerNode = "third-node"
			case "parent-loss":
				repo.moves = nil
				r.Repository = &completionReadRepository{memoryRepository: repo, getError: apierrors.NewNotFound(schema.GroupResource{Resource: "shiftpvvolumes"}, i.VolumeID)}
			case "api-timeout":
				r.Repository = &completionReadRepository{memoryRepository: repo, getError: errors.New("API timeout")}
			}
			repo.volumes[i.VolumeID] = state
			allowed := scenario == "next-owner" || scenario == "parent-loss"
			err := r.sweepCleanup(ctx)
			if (err == nil) != allowed {
				t.Fatalf("authority result: %v", err)
			}
			if allowed {
				if err := r.sweepCleanup(ctx); err != nil {
					t.Fatal(err)
				}
			}
			jobs, _ := client.BatchV1().Jobs("system").List(ctx, metav1.ListOptions{})
			if allowed && len(jobs.Items) != 1 {
				t.Fatal("safe check blocked")
			}
			if !allowed && len(jobs.Items) != 0 {
				t.Fatal("unsafe authority started check")
			}
		})
	}
}

func TestCleanupRetentionRequiresTerminalMoveAndStableMetadata(t *testing.T) {
	ctx := context.Background()
	r, repo, client, i := cleanupReviewFixture(t)
	now := time.Now()
	r.Now = func() time.Time { return now }
	if err := r.cleanupJournal().Complete(ctx, i, "original"); err != nil {
		t.Fatal(err)
	}
	cm, _ := client.CoreV1().ConfigMaps("system").Get(ctx, cleanup.Name(i.MoveUID), metav1.GetOptions{})
	cm.UID = "request-uid"
	cm.ResourceVersion = "42"
	client.CoreV1().ConfigMaps("system").Update(ctx, cm, metav1.UpdateOptions{})
	now = now.Add(8 * 24 * time.Hour)
	repo.moves[0].Status.Phase = "Completing"
	if err := r.sweepCleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.cleanupJournal().Get(ctx, i.MoveUID); err != nil {
		t.Fatal("completion evidence pruned before Move ended")
	}
	repo.moves[0].Status.Phase = "Succeeded"
	state := repo.volumes[i.VolumeID]
	state.ActiveMove = "another"
	repo.volumes[i.VolumeID] = state
	if err := r.sweepCleanup(ctx); err != nil {
		t.Fatal(err)
	}
	state.ActiveMove = ""
	repo.volumes[i.VolumeID] = state
	client.PrependReactor("delete", "configmaps", func(a ktesting.Action) (bool, runtime.Object, error) {
		p := a.(ktesting.DeleteAction).GetDeleteOptions().Preconditions
		if p == nil || p.UID == nil || string(*p.UID) != "request-uid" || p.ResourceVersion == nil || *p.ResourceVersion != "42" {
			t.Fatal("unsafe metadata deletion")
		}
		return false, nil, nil
	})
	if err := r.sweepCleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.cleanupJournal().Get(ctx, i.MoveUID); !apierrors.IsNotFound(err) {
		t.Fatalf("expired record retained: %v", err)
	}
}

func TestCleanupSweepRotatesAfterCanceledRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, repo, client := cleanupMoveFixture()
	repo.moves = nil
	var intents []cleanup.Intent
	for n := 0; n < 3; n++ {
		i := cleanup.Intent{MoveName: fmt.Sprint("move-", n), MoveUID: fmt.Sprint("uid-", n), VolumeID: fmt.Sprint("volume-", n), SourceNode: "source", DestinationNode: "destination", PoolPath: "/source-pool", JobName: fmt.Sprint("job-", n)}
		r.cleanupJournal().Ensure(context.Background(), i)
		intents = append(intents, i)
	}
	sort.Slice(intents, func(i, j int) bool { return cleanup.Name(intents[i].MoveUID) < cleanup.Name(intents[j].MoveUID) })
	first := true
	client.PrependReactor("update", "configmaps", func(a ktesting.Action) (bool, runtime.Object, error) {
		if first {
			first = false
			cancel()
			return true, nil, context.DeadlineExceeded
		}
		return false, nil, nil
	})
	if err := r.sweepCleanup(ctx); err == nil {
		t.Fatal("cancellation hidden")
	}
	if err := r.sweepCleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, i := range intents {
		rec, err := r.cleanupJournal().Get(context.Background(), i.MoveUID)
		if err != nil || rec.State != cleanup.NeedsReview {
			t.Fatalf("request starved: %+v %v", rec, err)
		}
	}
}

func TestCleanupSweepVisitsAllRecordsAcrossBatchLimit(t *testing.T) {
	ctx := context.Background()
	r, repo, _ := cleanupMoveFixture()
	repo.moves = nil
	var intents []cleanup.Intent
	for n := range 40 {
		i := cleanup.Intent{MoveName: fmt.Sprint("move-", n), MoveUID: fmt.Sprint("uid-", n), VolumeID: fmt.Sprint("volume-", n), SourceNode: "source", DestinationNode: "destination", PoolPath: "/source-pool", JobName: fmt.Sprint("job-", n)}
		if _, err := r.cleanupJournal().Ensure(ctx, i); err != nil {
			t.Fatal(err)
		}
		intents = append(intents, i)
	}
	for pass := range 2 {
		if err := r.sweepCleanup(ctx); err != nil {
			t.Fatal(err)
		}
		visited := 0
		for _, i := range intents {
			record, err := r.cleanupJournal().Get(ctx, i.MoveUID)
			if err != nil {
				t.Fatal(err)
			}
			if record.State == cleanup.NeedsReview {
				visited++
			}
		}
		if want := []int{32, 40}[pass]; visited != want {
			t.Fatalf("pass=%d visited=%d want=%d", pass, visited, want)
		}
	}
}

func TestCleanupRetentionPreservesUnknownAndIncompleteEvidence(t *testing.T) {
	for _, scenario := range []string{"incomplete", "invalid-time", "invalid-state", "volume-timeout", "young", "legacy-time"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			r, repo, client, i := cleanupReviewFixture(t)
			now := time.Now()
			r.Now = func() time.Time { return now }
			if scenario != "incomplete" {
				if err := r.cleanupJournal().Complete(ctx, i, "original"); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "young" {
				now = now.Add(8 * 24 * time.Hour)
			}
			cm, _ := client.CoreV1().ConfigMaps("system").Get(ctx, cleanup.Name(i.MoveUID), metav1.GetOptions{})
			switch scenario {
			case "invalid-time":
				cm.Annotations["shiftpv.io/cleanup-completed-at"] = "broken"
			case "invalid-state":
				cm.Annotations[cleanup.StateKey] = "Invented"
			case "legacy-time":
				delete(cm.Annotations, "shiftpv.io/cleanup-completed-at")
			case "volume-timeout":
				r.Repository = &completionReadRepository{memoryRepository: repo, getError: context.DeadlineExceeded}
			}
			if _, err := client.CoreV1().ConfigMaps("system").Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			client.PrependReactor("delete", "configmaps", func(ktesting.Action) (bool, runtime.Object, error) {
				t.Fatal("unsafe evidence pruning attempted")
				return true, nil, errors.New("unsafe deletion")
			})
			err := r.sweepCleanup(ctx)
			wantError := scenario == "invalid-time" || scenario == "invalid-state" || scenario == "volume-timeout"
			if (err != nil) != wantError {
				t.Fatalf("error=%v", err)
			}
			if scenario == "legacy-time" {
				record, err := r.cleanupJournal().Get(ctx, i.MoveUID)
				if err != nil || !record.CompletedAt.Equal(now) {
					t.Fatal("legacy completion did not start retention at first observation")
				}
			}
		})
	}
}
