package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/cleanupapi"
	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	poolcapacity "github.com/cagojeiger/ShiftPV/src/pool/capacity"
	"github.com/cagojeiger/ShiftPV/src/volume"
)

func recoveryFixture(t *testing.T, owner string) (*Reconciler, *memoryRepository, *fake.Clientset) {
	t.Helper()
	id := "shiftpv-0123456789abcdef0123456789abcdef"
	source, incoming, destination := testCopyIdentities(id, "source", "destination")
	incoming.CopyID = "move-move-uid-incoming"
	destination.CopyID = "move-move-uid-serving"
	move := volumeapi.Move{Name: "move-test", UID: "move-uid", Spec: volumeapi.MoveSpec{VolumeID: id, SourceNode: "source", Recovery: "ResumeOwner"}, Status: volumeapi.MoveStatus{
		Phase: "Blocked", Reason: "OriginalFailure", PersistentVolumeName: "pv", ClaimNamespace: "workload", ClaimName: "claim", DestinationNode: "destination", SourceCopy: &source,
	}}
	state := volumeapi.State{UID: source.VolumeUID, Phase: "Blocked", ActiveMove: move.Name, OwnerNode: owner, CurrentCopy: &source, CapacityBytes: 32 << 20}
	if owner == "destination" {
		move.Status.DestinationPoolUID = destination.PoolUID
		move.Status.SourceCopy, move.Status.IncomingCopy, move.Status.DestinationCopy = &source, &incoming, &destination
		move.Status.CopyJobName = namesFor(move.Name).CopyJob
		move.Status.CopyOperationID, move.Status.PromotionOperationID = "copy-"+move.UID, "promote-"+move.UID
		move.Status.CapacityApproved = true
		state.CurrentCopy = &destination
	}
	repo := &memoryRepository{moves: []volumeapi.Move{move}, volumes: map[string]volumeapi.State{id: state}, pools: []volumeapi.Pool{
		{Name: source.PoolName, UID: source.PoolUID, NodeName: "source", MountPath: "/source"},
		{Name: destination.PoolName, UID: destination.PoolUID, NodeName: "destination", MountPath: "/destination"},
	}}
	objects := mobilityObjects(id)
	objects[1].(*corev1.Node).Spec.Unschedulable = false
	objects[3].(*corev1.PersistentVolume).Spec.ClaimRef.UID = "claim-uid"
	objects[4].(*corev1.PersistentVolumeClaim).UID = "claim-uid"
	objects[len(objects)-1].(*corev1.Pod).Spec.NodeName = owner
	objects[len(objects)-1].(*corev1.Pod).UID = "consumer-uid"
	client := fake.NewSimpleClientset(objects...)
	return &Reconciler{
		Client: client, Repository: repo, Namespace: "system", HelperImage: "helper", ServiceAccountName: "shiftpv-controller",
		Cleanups: newTestCleanupStore(), CleanupOperator: receiptCleanupOperator{},
	}, repo, client
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
				restarted := &Reconciler{
					Client: client, Repository: repo, Namespace: r.Namespace, HelperImage: r.HelperImage, ServiceAccountName: r.ServiceAccountName,
					Cleanups: r.Cleanups, CleanupOperator: r.CleanupOperator,
				}
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
	jobMeta := moveObjectMeta(move, names.CopyJob, "system", transferLabels(names, move))
	jobMeta.UID = "old-job"
	podMeta := moveObjectMeta(move, names.SourcePod, "system", transferLabels(names, move))
	podMeta.UID = "old-pod"
	_, _ = client.BatchV1().Jobs("system").Create(context.Background(), &batchv1.Job{ObjectMeta: jobMeta}, metav1.CreateOptions{})
	_, _ = client.CoreV1().Pods("system").Create(context.Background(), &corev1.Pod{ObjectMeta: podMeta, Spec: corev1.PodSpec{NodeName: "source"}}, metav1.CreateOptions{})
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

func TestRecoveryQuiescesScheduledPlacementBeforeDestinationIsRecorded(t *testing.T) {
	r, repo, client := recoveryFixture(t, "source")
	move := repo.moves[0]
	move.Status.DestinationNode = ""
	move.Status.CandidateNodes = []string{"destination"}
	move.Status.RecoveryOwner = "source"
	move.Status.RecoveryPhase = recoveryQuiescing
	repo.moves[0] = move

	names := namesFor(move.Name)
	placement := r.placementPod(move, &corev1.Pod{}, names)
	placement.UID = "placement-uid"
	placement.Spec.NodeName = "destination"
	if _, err := client.CoreV1().Pods("system").Create(context.Background(), placement, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := r.ReconcileAll(context.Background()); err != nil {
		t.Fatalf("scheduled reservation blocked recovery before destination persistence: %v", err)
	}
	if _, err := client.CoreV1().Pods("system").Get(context.Background(), names.PlacementPod, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("placement reservation remained after quiesce: %v", err)
	}
	if repo.moves[0].Status.RecoveryReason != "" || repo.moves[0].Status.RecoveryPhase != recoveryQuiescing {
		t.Fatalf("unexpected recovery state after reservation deletion: %+v", repo.moves[0].Status)
	}
	if err := r.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repo.moves[0].Status.RecoveryPhase != recoveryVerifying {
		t.Fatalf("recovery did not advance after reservation disappearance: %+v", repo.moves[0].Status)
	}
}

func TestRecoveryRejectsUnrecordedDataHelper(t *testing.T) {
	r, repo, client := recoveryFixture(t, "source")
	move := repo.moves[0]
	move.Status.DestinationNode = ""
	move.Status.RecoveryOwner = "source"
	move.Status.RecoveryPhase = recoveryQuiescing
	repo.moves[0] = move

	names := namesFor(move.Name)
	_, err := client.CoreV1().Pods("system").Create(context.Background(), &corev1.Pod{
		ObjectMeta: func() metav1.ObjectMeta {
			metadata := moveObjectMeta(move, names.SourcePod, "system", transferLabels(names, move))
			metadata.UID = "unknown-helper"
			return metadata
		}(),
		Spec: corev1.PodSpec{NodeName: "unknown"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if err := r.ReconcileAll(context.Background()); err == nil {
		t.Fatal("unrecorded data helper was accepted")
	}
	if repo.moves[0].Status.RecoveryReason != "RecoveryStepFailed" || !strings.Contains(repo.moves[0].Status.RecoveryMessage, "unrecorded node") {
		t.Fatalf("unexpected recovery error: %+v", repo.moves[0].Status)
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

func rollbackRecoveryFixture(t *testing.T, copies []volumeapi.CopyObservation) (*Reconciler, *memoryRepository, volume.CopyIdentity, volume.CopyIdentity) {
	t.Helper()
	r, repo, _ := recoveryFixture(t, "source")
	move := repo.moves[0]
	state, err := repo.Get(context.Background(), move.Spec.VolumeID)
	if err != nil {
		t.Fatal(err)
	}
	source := *state.CurrentCopy
	incoming := volume.CopyIdentity{
		InstallationID: source.InstallationID, PoolName: "destination-pool", PoolUID: "destination-pool-uid",
		VolumeID: move.Spec.VolumeID, VolumeUID: source.VolumeUID, CopyID: "move-" + move.UID + "-incoming",
		NodeName: "destination", Role: volume.RoleIncoming,
	}
	destination := incoming
	destination.CopyID, destination.Role = "move-"+move.UID+"-serving", volume.RoleServing
	transitionedAt := time.Date(2026, 9, 13, 1, 0, 0, 0, time.UTC)
	move.Status.RecoveryOwner, move.Status.RecoveryPhase = "source", recoveryRetiring
	move.Status.LastTransitionTime = transitionedAt.Format(time.RFC3339Nano)
	move.Status.DestinationNode, move.Status.DestinationPoolUID = "destination", destination.PoolUID
	move.Status.SourceCopy, move.Status.IncomingCopy, move.Status.DestinationCopy = &source, &incoming, &destination
	move.Status.CopyJobName = namesFor(move.Name).CopyJob
	move.Status.CopyOperationID, move.Status.PromotionOperationID = "copy-"+move.UID, "promote-"+move.UID
	move.Status.SourceBytes = 32 << 20
	move.Status.CapacityApproved = true
	repo.moves[0] = move

	observedAt := transitionedAt.Add(time.Second)
	for index := range repo.pools {
		if repo.pools[index].NodeName != "destination" {
			continue
		}
		repo.pools[index].Generation = 1
		repo.pools[index].Finalizers = []string{cleanupapi.PoolProtectionFinalizer}
		repo.pools[index].Status = volumeapi.PoolStatus{
			ObservedGeneration: 1,
			LastProbeTime:      metav1.NewTime(observedAt),
			Conditions: []metav1.Condition{{
				Type: volumeapi.PoolConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 1,
			}},
			Inventory: &volumeapi.PoolInventory{ObservedAt: metav1.NewTime(observedAt), Valid: true, Copies: copies},
		}
	}
	r.Now = func() time.Time { return observedAt.Add(time.Second) }
	r.PoolReadinessStaleAfter = time.Minute
	return r, repo, incoming, destination
}

func TestRecoveryRollbackCleansExactlyOneObservedDestinationArtifact(t *testing.T) {
	for _, role := range []string{volume.RoleIncoming, volume.RoleServing} {
		t.Run(role, func(t *testing.T) {
			r, repo, incoming, destination := rollbackRecoveryFixture(t, nil)
			target := incoming
			if role == volume.RoleServing {
				target = destination
			}
			repo.pools[1].Status.Inventory.Copies = []volumeapi.CopyObservation{{Identity: &target, Present: true}}
			move := repo.moves[0]
			state, err := repo.Get(context.Background(), move.Spec.VolumeID)
			if err != nil {
				t.Fatal(err)
			}
			done, err := r.settleRecoveryArtifacts(context.Background(), &move, state)
			if err != nil || done {
				t.Fatalf("first settlement done=%v err=%v", done, err)
			}
			cleanup, err := r.Cleanups.Get(context.Background(), cleanupapiAuthority(move))
			if err != nil || cleanup.Spec.Reason != "MoveRollback" || cleanup.Spec.Target != target || cleanup.Status.Phase != "Completed" {
				t.Fatalf("cleanup=%#v err=%v", cleanup, err)
			}
			settled := repo.moves[0]
			if settled.Status.CapacityApproved || settled.Status.CapacityReason != recoveryCapacitySettled {
				t.Fatalf("capacity hold was not durably settled: %+v", settled.Status)
			}
			done, err = r.settleRecoveryArtifacts(context.Background(), &settled, state)
			if err != nil || !done {
				t.Fatalf("settled retry done=%v err=%v", done, err)
			}
		})
	}
}

func TestRecoveryRollbackAcceptsOnlyFreshExactAbsence(t *testing.T) {
	t.Run("no destination effect before source identity journal", func(t *testing.T) {
		r, repo, _ := recoveryFixture(t, "source")
		move := repo.moves[0]
		move.Status.SourceCopy = nil
		move.Status.RecoveryOwner, move.Status.RecoveryPhase = "source", recoveryRetiring
		move.Status.CapacityReason = "DestinationFilesystemSpace"
		repo.moves[0] = move
		state, _ := repo.Get(context.Background(), move.Spec.VolumeID)
		done, err := r.settleRecoveryArtifacts(context.Background(), &move, state)
		if err != nil || done {
			t.Fatalf("first settlement done=%v err=%v", done, err)
		}
		if repo.moves[0].Status.CapacityApproved || repo.moves[0].Status.CapacityReason != recoveryCapacitySettled {
			t.Fatalf("no-effect recovery did not settle: %+v", repo.moves[0].Status)
		}
	})

	t.Run("already absent", func(t *testing.T) {
		r, repo, _, _ := rollbackRecoveryFixture(t, nil)
		move := repo.moves[0]
		state, _ := repo.Get(context.Background(), move.Spec.VolumeID)
		done, err := r.settleRecoveryArtifacts(context.Background(), &move, state)
		if err != nil || done {
			t.Fatalf("first settlement done=%v err=%v", done, err)
		}
		if repo.moves[0].Status.CapacityApproved || repo.moves[0].Status.CapacityReason != recoveryCapacitySettled {
			t.Fatalf("fresh absence did not settle capacity: %+v", repo.moves[0].Status)
		}
		if _, err := r.Cleanups.Get(context.Background(), cleanupapiAuthority(move)); !apierrors.IsNotFound(err) {
			t.Fatalf("already-absent rollback invented a cleanup receipt: %v", err)
		}
	})

	t.Run("inventory not after Retiring", func(t *testing.T) {
		r, repo, _, _ := rollbackRecoveryFixture(t, nil)
		move := repo.moves[0]
		transitionedAt, _ := time.Parse(time.RFC3339Nano, move.Status.LastTransitionTime)
		repo.pools[1].Status.Inventory.ObservedAt = metav1.NewTime(transitionedAt)
		state, _ := repo.Get(context.Background(), move.Spec.VolumeID)
		if done, err := r.settleRecoveryArtifacts(context.Background(), &move, state); err == nil || done {
			t.Fatalf("non-causal absence was accepted: done=%v err=%v", done, err)
		}
		if !repo.moves[0].Status.CapacityApproved {
			t.Fatal("capacity hold was released without post-Retiring evidence")
		}
	})
}

func TestRecoveryRollbackPreservesAmbiguousArtifactsForReview(t *testing.T) {
	for _, scenario := range []string{"both transaction copies", "problem observation", "conflicting copy", "published transaction"} {
		t.Run(scenario, func(t *testing.T) {
			r, repo, incoming, destination := rollbackRecoveryFixture(t, nil)
			switch scenario {
			case "both transaction copies":
				repo.pools[1].Status.Inventory.Copies = []volumeapi.CopyObservation{{Identity: &incoming, Present: true}, {Identity: &destination, Present: true}}
			case "problem observation":
				repo.pools[1].Status.Inventory.Copies = []volumeapi.CopyObservation{{Marker: "path:volumes/unknown", Present: true, Problem: "UnrecordedPath"}}
			case "conflicting copy":
				conflict := destination
				conflict.CopyID = "foreign-copy"
				repo.pools[1].Status.Inventory.Copies = []volumeapi.CopyObservation{{Identity: &conflict, Present: true}}
			case "published transaction":
				repo.pools[1].Status.Inventory.Copies = []volumeapi.CopyObservation{{Identity: &incoming, Present: true, Published: true}}
			}
			move := repo.moves[0]
			err := r.reconcileRecovery(context.Background(), move)
			if !errors.Is(err, errRecoveryCleanupNeedsReview) || repo.moves[0].Status.RecoveryReason != "CleanupNeedsReview" {
				t.Fatalf("ambiguous artifact was not sent to review: status=%+v err=%v", repo.moves[0].Status, err)
			}
			if !repo.moves[0].Status.CapacityApproved {
				t.Fatal("ambiguous artifact released the capacity hold")
			}
			if _, err := r.Cleanups.Get(context.Background(), cleanupapiAuthority(move)); !apierrors.IsNotFound(err) {
				t.Fatalf("ambiguous target created a destructive intent: %v", err)
			}
		})
	}
}

func TestPostcommitRecoveryWaitsForActualDestinationPublicationBeforeSourceCleanup(t *testing.T) {
	r, repo, _ := recoveryFixture(t, "destination")
	move := repo.moves[0]
	move.Status.RecoveryOwner, move.Status.RecoveryPhase = "destination", recoveryRetiring
	repo.moves[0] = move
	state := repo.volumes[move.Spec.VolumeID]
	state.Phase = volumeapi.PhaseReady
	state.PublishedNodes = nil
	repo.volumes[move.Spec.VolumeID] = state

	if err := r.reconcileRecovery(context.Background(), move); err == nil {
		t.Fatal("source cleanup started before destination publication")
	}
	if !repo.moves[0].Status.CapacityApproved || repo.moves[0].Status.CapacityReason == recoveryCapacitySettled {
		t.Fatalf("unpublished destination released source hold: %+v", repo.moves[0].Status)
	}
	if _, err := r.Cleanups.Get(context.Background(), cleanupapiAuthority(move)); !apierrors.IsNotFound(err) {
		t.Fatalf("unpublished destination created cleanup intent: %v", err)
	}

	state.PublishedNodes = []string{"destination"}
	repo.volumes[move.Spec.VolumeID] = state
	move = repo.moves[0]
	destination := *move.Status.DestinationCopy
	repo.readyPoolsConfigured = true
	repo.readyPools = []volumeapi.Pool{{
		Name: destination.PoolName, UID: destination.PoolUID, NodeName: destination.NodeName, MountPath: "/destination",
		Status: volumeapi.PoolStatus{Inventory: &volumeapi.PoolInventory{
			Valid: true, Copies: []volumeapi.CopyObservation{{Identity: &destination, Present: true, Published: false}},
		}},
	}}
	if err := r.reconcileRecovery(context.Background(), move); err == nil {
		t.Fatal("source cleanup started before destination scanner publication proof")
	}
	if !repo.moves[0].Status.CapacityApproved || repo.moves[0].Status.CapacityReason == recoveryCapacitySettled {
		t.Fatalf("unproven destination released source hold: %+v", repo.moves[0].Status)
	}
	repo.readyPools[0].Status.Inventory.Copies[0].Published = true
	if err := r.reconcileRecovery(context.Background(), move); err != nil {
		t.Fatal(err)
	}
	if repo.moves[0].Status.CapacityApproved || repo.moves[0].Status.CapacityReason != recoveryCapacitySettled {
		t.Fatalf("published destination did not settle source hold: %+v", repo.moves[0].Status)
	}
	cleanup, err := r.Cleanups.Get(context.Background(), cleanupapiAuthority(move))
	if err != nil || cleanup.Spec.Reason != "MoveSource" || cleanup.Spec.Target != *move.Status.SourceCopy || cleanup.Status.Phase != "Completed" {
		t.Fatalf("postcommit cleanup=%#v err=%v", cleanup, err)
	}
}

func TestRecoveredMoveReleasesFinalizerOnlyAfterCapacitySettlement(t *testing.T) {
	r, repo, _ := recoveryFixture(t, "source")
	move := repo.moves[0]
	move.Finalizers = []string{volumeapi.MoveProtectionFinalizer}
	move.Status.RecoveryOwner, move.Status.RecoveryPhase = "source", recoveryResuming
	move.Status.CapacityReason = recoveryCapacitySettled
	repo.moves[0] = move

	if err := r.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := repo.volumes[move.Spec.VolumeID]
	state.PublishedNodes = []string{"source"}
	repo.volumes[move.Spec.VolumeID] = state
	for cycle := 0; cycle < 4; cycle++ {
		if err := r.ReconcileAll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	got := repo.moves[0]
	if got.Status.RecoveryPhase != recoveryRecovered || repo.volumes[move.Spec.VolumeID].ActiveMove != "" ||
		contains(got.Finalizers, volumeapi.MoveProtectionFinalizer) || !volumeapi.MoveCleanupSettled(got) {
		t.Fatalf("recovery did not terminally settle: move=%+v volume=%+v", got, repo.volumes[move.Spec.VolumeID])
	}
	reserved, err := poolcapacity.ReservedBytes(repo.volumes, repo.moves, "destination")
	if err != nil || reserved != 0 {
		t.Fatalf("settled recovery retained destination capacity: reserved=%d err=%v", reserved, err)
	}

	unsafe := got
	unsafe.Finalizers = []string{volumeapi.MoveProtectionFinalizer}
	unsafe.Status.CapacityApproved = true
	unsafe.Status.CapacityReason = ""
	repo.moves[0] = unsafe
	if err := r.ReconcileAll(context.Background()); err == nil {
		t.Fatal("Recovered without capacity settlement was accepted")
	}
	if !contains(repo.moves[0].Finalizers, volumeapi.MoveProtectionFinalizer) {
		t.Fatal("unsafe Recovered move lost its finalizer")
	}
}

func cleanupapiAuthority(move volumeapi.Move) cleanupapi.Authority {
	return cleanupapi.Authority{Kind: "ShiftPVMove", Name: move.Name, UID: move.UID}
}
