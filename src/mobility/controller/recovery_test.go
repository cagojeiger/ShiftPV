package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/mobility/fsm"
)

func recoveryFixture(t *testing.T, owner string) (*Reconciler, *memoryRepository, *fake.Clientset) {
	t.Helper()
	id := "shiftpv-0123456789abcdef0123456789abcdef"
	move := volumeapi.Move{Name: "move-test", UID: "move-uid", Spec: volumeapi.MoveSpec{VolumeID: id, SourceNode: "source", Recovery: "ResumeOwner"}, Status: volumeapi.MoveStatus{
		Phase: "Blocked", Reason: "OriginalFailure", PersistentVolumeName: "pv", ClaimNamespace: "workload", ClaimName: "claim", DestinationNode: "destination",
	}}
	repo := &memoryRepository{moves: []volumeapi.Move{move}, volumes: map[string]volumeapi.State{id: {Phase: "Blocked", ActiveMove: move.Name, OwnerNode: owner}}, pools: []volumeapi.Pool{{NodeName: "source", MountPath: "/source"}, {NodeName: "destination", MountPath: "/destination"}}}
	objects := mobilityObjects(id)
	objects[1].(*corev1.Node).Spec.Unschedulable = false
	objects[3].(*corev1.PersistentVolume).Spec.ClaimRef.UID = "claim-uid"
	objects[4].(*corev1.PersistentVolumeClaim).UID = "claim-uid"
	objects[len(objects)-1].(*corev1.Pod).Spec.NodeName = owner
	objects[len(objects)-1].(*corev1.Pod).UID = "consumer-uid"
	client := fake.NewSimpleClientset(objects...)
	return &Reconciler{Client: client, Repository: repo, Namespace: "system", HelperImage: "helper"}, repo, client
}

func finishRecoveryJobs(t *testing.T, client *fake.Clientset, condition batchv1.JobConditionType) {
	t.Helper()
	jobs, err := client.BatchV1().Jobs("system").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		job.Status.Conditions = []batchv1.JobCondition{{Type: condition, Status: corev1.ConditionTrue}}
		if _, err := client.BatchV1().Jobs("system").UpdateStatus(context.Background(), job, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecoveryResumesOnlyCurrentOwnerAndSurvivesEveryBoundary(t *testing.T) {
	for _, owner := range []string{"source", "destination"} {
		t.Run(owner, func(t *testing.T) {
			r, repo, client := recoveryFixture(t, owner)
			id := repo.moves[0].Spec.VolumeID
			for cycle := 0; cycle < 24; cycle++ {
				// Construct a fresh reconciler on every pass: no in-memory recovery state.
				restarted := &Reconciler{Client: client, Repository: repo, Namespace: r.Namespace, HelperImage: r.HelperImage}
				if err := restarted.ReconcileAll(context.Background()); err != nil {
					t.Fatal(err)
				}
				if repo.volumes[id].OwnerNode != owner {
					t.Fatal("recovery changed authoritative owner")
				}
				if repo.volumes[id].Phase == "Ready" {
					state := repo.volumes[id]
					state.PublishedNodes = []string{owner}
					repo.volumes[id] = state
				}
				finishRecoveryJobs(t, client, batchv1.JobComplete)
			}
			if repo.moves[0].Status.Phase != "Blocked" || repo.moves[0].Status.Reason != "OriginalFailure" || repo.moves[0].Status.RecoveryPhase != recoveryRecovered || repo.volumes[id].ActiveMove != "" || !strings.Contains(repo.moves[0].Status.Message, "no operator action is required") {
				t.Fatalf("move=%+v volume=%+v", repo.moves[0], repo.volumes[id])
			}
			jobs, _ := client.BatchV1().Jobs("system").List(context.Background(), metav1.ListOptions{})
			if len(jobs.Items) != 0 {
				t.Fatal("recovery jobs left behind")
			}
		})
	}
}

func TestRecoveryFailsClosedOnUnsafeObservation(t *testing.T) {
	for _, scenario := range []string{"foreign writer", "unknown owner", "changed owner", "different move", "not blocked", "cordoned owner", "unready peer", "binding UID changed", "API failure", "unrecorded destination"} {
		t.Run(scenario, func(t *testing.T) {
			r, repo, client := recoveryFixture(t, "source")
			id := repo.moves[0].Spec.VolumeID
			state := repo.volumes[id]
			switch scenario {
			case "foreign writer":
				state.PublishedNodes = []string{"destination"}
			case "unknown owner":
				state.OwnerNode = "unknown"
			case "changed owner":
				repo.moves[0].Status.RecoveryOwner = "destination"
			case "different move":
				state.ActiveMove = "newer-move"
			case "not blocked":
				state.Phase = "Moving"
			case "unrecorded destination":
				repo.moves[0].Status.DestinationNode = ""
				repo.moves[0].Status.CopyJobName = "copy"
			case "cordoned owner", "unready peer":
				name := "source"
				if scenario == "unready peer" {
					name = "destination"
				}
				node, _ := client.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
				if scenario == "cordoned owner" {
					node.Spec.Unschedulable = true
				} else {
					node.Status.Conditions = nil
				}
				_, _ = client.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{})
			case "binding UID changed":
				claim, _ := client.CoreV1().PersistentVolumeClaims("workload").Get(context.Background(), "claim", metav1.GetOptions{})
				claim.UID = "replacement-claim"
				_, _ = client.CoreV1().PersistentVolumeClaims("workload").Update(context.Background(), claim, metav1.UpdateOptions{})
			case "API failure":
				client.PrependReactor("get", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, fmt.Errorf("timeout") })
			}
			repo.volumes[id] = state
			if err := r.ReconcileAll(context.Background()); err == nil {
				t.Fatal("unsafe observation was accepted")
			}
			if repo.volumes[id].Phase == "Ready" || repo.volumes[id].OwnerNode != state.OwnerNode || repo.moves[0].Status.RecoveryReason == "" || !strings.Contains(repo.moves[0].Status.Message, "operator action required") {
				t.Fatalf("unsafe recovery state: %+v %+v", repo.volumes[id], repo.moves[0])
			}
			jobs, _ := client.BatchV1().Jobs("system").List(context.Background(), metav1.ListOptions{})
			if len(jobs.Items) != 0 {
				t.Fatal("unsafe observation started helper jobs")
			}
		})
	}
}

func TestRecoveryWaitsForOldHelpersAndFailedVerification(t *testing.T) {
	r, repo, client := recoveryFixture(t, "source")
	move := repo.moves[0]
	move.Status.RecoveryOwner, move.Status.RecoveryPhase = "source", recoveryQuiescing
	repo.moves[0] = move
	names := namesFor(move.Name)
	_, _ = client.BatchV1().Jobs("system").Create(context.Background(), &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: names.CopyJob, UID: "old-job", Labels: transferLabels(names)}}, metav1.CreateOptions{})
	_, _ = client.CoreV1().Pods("system").Create(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: names.SourcePod, UID: "old-pod", Labels: transferLabels(names)}, Spec: corev1.PodSpec{NodeName: "source"}}, metav1.CreateOptions{})
	if err := r.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repo.moves[0].Status.RecoveryPhase != recoveryQuiescing {
		t.Fatal("advanced without read-back of termination")
	}
	for _, action := range client.Actions() {
		if action.GetVerb() != "delete" {
			continue
		}
		opts := action.(k8stesting.DeleteAction).GetDeleteOptions()
		if opts.Preconditions == nil || opts.Preconditions.UID == nil || (opts.GracePeriodSeconds != nil && *opts.GracePeriodSeconds == 0) {
			t.Fatalf("unsafe delete: %#v", opts)
		}
	}
	for i := 0; i < 2; i++ {
		if err := r.ReconcileAll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	jobs, _ := client.BatchV1().Jobs("system").List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 1 || jobs.Items[0].Spec.TTLSecondsAfterFinished != nil || !jobs.Items[0].Spec.Template.Spec.Containers[0].VolumeMounts[0].ReadOnly {
		t.Fatalf("verify job not read-only/durable: %+v", jobs)
	}
	finishRecoveryJobs(t, client, batchv1.JobFailed)
	if err := r.ReconcileAll(context.Background()); err == nil {
		t.Fatal("failed verification accepted")
	}
	if repo.volumes[move.Spec.VolumeID].Phase != "Blocked" {
		t.Fatal("failed verification opened mount guard")
	}
}

func TestRecoveryPlacementHonorsPDBAndPodUID(t *testing.T) {
	r, repo, client := recoveryFixture(t, "source")
	move := repo.moves[0]
	move.Status.RecoveryOwner = "source"
	claim, _ := client.CoreV1().PersistentVolumeClaims("workload").Get(context.Background(), "claim", metav1.GetOptions{})
	pod, _ := client.CoreV1().Pods("workload").Get(context.Background(), "consumer", metav1.GetOptions{})
	pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: placementHoldName}}
	_, _ = client.CoreV1().Pods("workload").Update(context.Background(), pod, metav1.UpdateOptions{})
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		eviction := action.(k8stesting.CreateAction).GetObject().(*policyv1.Eviction)
		if eviction.DeleteOptions == nil || eviction.DeleteOptions.Preconditions == nil || *eviction.DeleteOptions.Preconditions.UID != pod.UID {
			t.Fatal("eviction has no observed UID")
		}
		return true, nil, apierrors.NewTooManyRequests("PDB denied", 1)
	})
	if ready, err := r.recoverPlacement(context.Background(), move, claim); err == nil || ready {
		t.Fatal("PDB bypassed")
	}
}

func TestDiscoveryWaitsForDestinationRecoveryJournalAfterFinalCAS(t *testing.T) {
	r, repo, client := recoveryFixture(t, "destination")
	id := repo.moves[0].Spec.VolumeID
	repo.moves[0].Status.RecoveryPhase = recoveryCompleting
	repo.moves[0].Status.RecoveryOwner = "destination"
	repo.volumes[id] = volumeapi.State{Phase: "Ready", OwnerNode: "destination", PublishedNodes: []string{"destination"}}
	node, _ := client.CoreV1().Nodes().Get(context.Background(), "destination", metav1.GetOptions{})
	node.Spec.Unschedulable = true
	_, _ = client.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{})
	if err := r.discoverMoves(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.moves) != 1 {
		t.Fatal("new transaction discovered before recovery terminal journal")
	}
	if err := r.reconcileRecovery(context.Background(), repo.moves[0]); err != nil {
		t.Fatal(err)
	}
	if repo.moves[0].Status.RecoveryPhase != recoveryRecovered {
		t.Fatal("final CAS crash did not converge")
	}
}

func TestCleanupFailureReasonIsNotMaskedByEvictedConsumer(t *testing.T) {
	r, repo, client := recoveryFixture(t, "destination")
	move := repo.moves[0]
	move.Spec.Recovery = ""
	move.Status.Phase, move.Status.ConsumerName = string(fsm.PhaseCleaningSource), "original-evicted-pod"
	repo.moves[0] = move
	state := repo.volumes[move.Spec.VolumeID]
	state.Phase = "Ready"
	state.PublishedNodes = []string{"destination"}
	repo.volumes[move.Spec.VolumeID] = state
	_, _ = client.BatchV1().Jobs("system").Create(context.Background(), &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: namesFor(move.Name).CleanupJob},
		Status:     batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}},
	}, metav1.CreateOptions{})
	if err := r.reconcileMove(context.Background(), move); err != nil {
		t.Fatal(err)
	}
	if repo.moves[0].Status.Reason != "CleanupFailed" {
		t.Fatalf("actual cleanup failure masked by %q", repo.moves[0].Status.Reason)
	}
}
