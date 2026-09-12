package cleanupcontroller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/cleanupapi"
	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	poolcapacity "github.com/cagojeiger/ShiftPV/src/pool/capacity"
	"github.com/cagojeiger/ShiftPV/src/volume"
)

type operator struct {
	err    error
	calls  int
	result *cleanupapi.Cleanup
}

type inventory struct {
	pools   []volumeapi.Pool
	volumes map[string]volumeapi.State
	moves   []volumeapi.Move
	fail    string
}

type countingInventory struct {
	inventory
	poolCalls   int
	volumeCalls int
	moveCalls   int
}

func (i *countingInventory) ListPools(ctx context.Context) ([]volumeapi.Pool, error) {
	i.poolCalls++
	return i.inventory.ListPools(ctx)
}

func (i *countingInventory) ListVolumes(ctx context.Context) (map[string]volumeapi.State, error) {
	i.volumeCalls++
	return i.inventory.ListVolumes(ctx)
}

func (i *countingInventory) ListMoves(ctx context.Context) ([]volumeapi.Move, error) {
	i.moveCalls++
	return i.inventory.ListMoves(ctx)
}

func orphanKubernetesClient() *fake.Clientset {
	return fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "installation"}})
}

func readyPool(identity volume.CopyIdentity, copies ...volumeapi.CopyObservation) volumeapi.Pool {
	now := metav1.NewTime(time.Unix(1, 0))
	return volumeapi.Pool{
		Name: identity.PoolName, UID: identity.PoolUID, NodeName: identity.NodeName, Generation: 1,
		Status: volumeapi.PoolStatus{
			ObservedGeneration: 1, LastProbeTime: now,
			Conditions: []metav1.Condition{{Type: volumeapi.PoolConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 1, LastTransitionTime: now, Reason: "Ready", Message: "ready"}},
			Inventory:  &volumeapi.PoolInventory{ObservedAt: now, Valid: true, Copies: copies},
		},
	}
}

func (i inventory) ListPools(context.Context) ([]volumeapi.Pool, error) {
	if i.fail == "pools" {
		return nil, errors.New("pool inventory unavailable")
	}
	return i.pools, nil
}
func (i inventory) ListVolumes(context.Context) (map[string]volumeapi.State, error) {
	if i.fail == "volumes" {
		return nil, errors.New("volume inventory unavailable")
	}
	return i.volumes, nil
}
func (i inventory) ListMoves(context.Context) ([]volumeapi.Move, error) {
	if i.fail == "moves" {
		return nil, errors.New("move inventory unavailable")
	}
	return i.moves, nil
}

func (o *operator) Reclaim(ctx context.Context, request cleanupapi.Cleanup, store *cleanupapi.Store) (cleanupapi.Cleanup, error) {
	o.calls++
	if o.err != nil {
		return cleanupapi.Cleanup{}, o.err
	}
	if o.result != nil {
		return *o.result, nil
	}
	if request.Status.Phase == cleanupapi.PhaseVerifying || request.Status.Phase == cleanupapi.PhaseCompleted {
		return request, nil
	}
	executor := &cleanupapi.Executor{JobName: "job", JobUID: "job-uid", NodeName: request.Spec.Target.NodeName}
	if request.Status.Phase == "" || request.Status.Phase == cleanupapi.PhasePending {
		if err := store.UpdateStatus(ctx, request.Name, request.UID, cleanupapi.Status{Phase: cleanupapi.PhaseRunning, Executor: executor}); err != nil {
			return cleanupapi.Cleanup{}, err
		}
	}
	receipt := &cleanupapi.Receipt{OperationID: request.Spec.OperationID, ExecutorUID: executor.JobUID, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Retired: true, Purged: true}
	if err := store.UpdateStatus(ctx, request.Name, request.UID, cleanupapi.Status{Phase: cleanupapi.PhaseVerifying, Executor: executor, Receipt: receipt}); err != nil {
		return cleanupapi.Cleanup{}, err
	}
	return store.Get(ctx, request.Name)
}

func TestReconcileAllResumesPendingAndSettlesReceipt(t *testing.T) {
	store, request := fixture(t)
	worker := &operator{}
	reconciler := &Reconciler{Store: store, Operator: worker, Interval: time.Second, Now: func() time.Time { return time.Unix(1, 0) }}
	if err := reconciler.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	settled, err := store.Get(context.Background(), request.Name)
	if err != nil || settled.Status.Phase != cleanupapi.PhaseCompleted || settled.Status.SettledAt == "" || worker.calls != 1 {
		t.Fatalf("settled=%#v calls=%d err=%v", settled, worker.calls, err)
	}
	if err := reconciler.ReconcileAll(context.Background()); err != nil || worker.calls != 1 {
		t.Fatalf("completed request reran: calls=%d err=%v", worker.calls, err)
	}
}

func TestRunReconcilesImmediatelyAndStopsWithContext(t *testing.T) {
	store, request := fixture(t)
	worker := &operator{}
	reconciler := &Reconciler{Store: store, Operator: worker, Interval: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reconciler.Run(ctx) }()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		current, err := store.Get(context.Background(), request.Name)
		if err == nil && current.Status.Phase == cleanupapi.PhaseCompleted {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("cleanup loop did not reconcile immediately")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := (&Reconciler{}).Run(context.Background()); err == nil {
		t.Fatal("invalid cleanup loop configuration accepted")
	}
}

func TestReconcilePreservesIntentOnOperatorFailure(t *testing.T) {
	store, request := fixture(t)
	worker := &operator{err: errors.New("temporary")}
	reconciler := &Reconciler{Store: store, Operator: worker, Interval: time.Second}
	if err := reconciler.ReconcileAll(context.Background()); err == nil {
		t.Fatal("operator failure was hidden")
	}
	current, err := store.Get(context.Background(), request.Name)
	if err != nil || current.Status.Phase == cleanupapi.PhaseCompleted {
		t.Fatalf("failed cleanup was settled: %#v %v", current, err)
	}
}

func TestReconcileDoesNotSettleReceiptBeforeOperatorAcknowledgesTermination(t *testing.T) {
	store, request := fixture(t)
	executor := &cleanupapi.Executor{JobName: "job", JobUID: "job-uid", NodeName: request.Spec.Target.NodeName}
	if err := store.UpdateStatus(context.Background(), request.Name, request.UID, cleanupapi.Status{Phase: cleanupapi.PhaseRunning, Executor: executor}); err != nil {
		t.Fatal(err)
	}
	receipt := &cleanupapi.Receipt{
		OperationID: request.Spec.OperationID, ExecutorUID: executor.JobUID,
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Retired: true, Purged: true,
	}
	if err := store.UpdateStatus(context.Background(), request.Name, request.UID, cleanupapi.Status{Phase: cleanupapi.PhaseVerifying, Executor: executor, Receipt: receipt}); err != nil {
		t.Fatal(err)
	}
	worker := &operator{err: errors.New("executor is still running")}
	reconciler := &Reconciler{Store: store, Operator: worker, Interval: time.Second}
	if err := reconciler.ReconcileAll(context.Background()); err == nil {
		t.Fatal("Verifying cleanup bypassed executor termination acknowledgement")
	}
	current, err := store.Get(context.Background(), request.Name)
	if err != nil || current.Status.Phase != cleanupapi.PhaseVerifying || worker.calls != 1 {
		t.Fatalf("cleanup settled early: current=%#v calls=%d err=%v", current, worker.calls, err)
	}
}

func TestReconcileFailsClosedAcrossSettlementBoundaries(t *testing.T) {
	store, request := fixture(t)
	executor := &cleanupapi.Executor{JobName: "job", JobUID: "job-uid", NodeName: request.Spec.Target.NodeName}
	receipt := &cleanupapi.Receipt{
		OperationID: request.Spec.OperationID, ExecutorUID: executor.JobUID,
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Retired: true, Purged: true,
	}
	for name, result := range map[string]cleanupapi.Cleanup{
		"incomplete receipt": {Spec: request.Spec, Status: cleanupapi.Status{Phase: cleanupapi.PhaseVerifying, Executor: executor}},
		"already completed":  {Spec: request.Spec, Status: cleanupapi.Status{Phase: cleanupapi.PhaseCompleted, Executor: executor, Receipt: receipt, SettledAt: time.Now().UTC().Format(time.RFC3339Nano)}},
	} {
		t.Run(name, func(t *testing.T) {
			worker := &operator{result: &result}
			err := (&Reconciler{Store: store, Operator: worker, Interval: time.Second}).Reconcile(context.Background(), request)
			if name == "incomplete receipt" && err == nil {
				t.Fatal("incomplete receipt settled")
			}
			if name == "already completed" && err != nil {
				t.Fatalf("completed cleanup failed: %v", err)
			}
		})
	}
}

func TestReservationSettlementPreservesReplacementAndAPIFailures(t *testing.T) {
	_, request := fixture(t)
	request.Spec.Reason = "OrphanReclaim"
	request.Spec.ReservationUID = "expected-reservation"
	request.Status = cleanupapi.Status{
		Phase:    cleanupapi.PhaseVerifying,
		Executor: &cleanupapi.Executor{JobName: "job", JobUID: "job-uid", NodeName: request.Spec.Target.NodeName},
		Receipt:  &cleanupapi.Receipt{OperationID: request.Spec.OperationID, ExecutorUID: "job-uid", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Retired: true, Purged: true},
	}

	replacement := orphanKubernetesClient()
	_, err := replacement.CoreV1().ConfigMaps("system").Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: request.Spec.Target.VolumeID, Namespace: "system", UID: "replacement-reservation"},
		Data:       map[string]string{"volumeID": request.Spec.Target.VolumeID, "volumeUID": request.Spec.Target.VolumeUID},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Reconciler{Client: replacement, Namespace: "system"}).releaseReservation(context.Background(), request); err == nil {
		t.Fatal("replacement reservation was deleted")
	}

	getFailure := orphanKubernetesClient()
	getFailure.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("temporary get failure")
	})
	if err := (&Reconciler{Client: getFailure, Namespace: "system"}).releaseReservation(context.Background(), request); err == nil {
		t.Fatal("reservation read failure was hidden")
	}

	deleteFailure := orphanKubernetesClient()
	_, err = deleteFailure.CoreV1().ConfigMaps("system").Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: request.Spec.Target.VolumeID, Namespace: "system", UID: types.UID(request.Spec.ReservationUID)},
		Data:       map[string]string{"volumeID": request.Spec.Target.VolumeID, "volumeUID": request.Spec.Target.VolumeUID},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	deleteFailure.PrependReactor("delete", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("temporary delete failure")
	})
	if err := (&Reconciler{Client: deleteFailure, Namespace: "system"}).releaseReservation(context.Background(), request); err == nil {
		t.Fatal("reservation delete failure was hidden")
	}
}

func TestReconcileClassifiesUnapprovedIntentWithoutExecuting(t *testing.T) {
	store, existing := fixture(t)
	if err := store.UpdateStatus(context.Background(), existing.Name, existing.UID, cleanupapi.Status{Phase: cleanupapi.PhaseNeedsReview, Reason: "fixture"}); err != nil {
		t.Fatal(err)
	}
	target := existing.Spec.Target
	target.CopyID = "unapproved-copy"
	request, err := store.Ensure(context.Background(), cleanupapi.Spec{
		OperationID: "review-unapproved-copy", Target: target, Reason: "OrphanReclaim", Approved: false,
		Authority: cleanupapi.Authority{Kind: "Namespace", Name: "kube-system", UID: target.InstallationID},
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := &operator{}
	reconciler := &Reconciler{Store: store, Operator: worker, Interval: time.Second}
	if err := reconciler.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, err := store.Get(context.Background(), request.Name)
	if err != nil || current.Status.Phase != cleanupapi.PhaseNeedsReview || current.Status.Reason != "ApprovalRequired" || worker.calls != 0 {
		t.Fatalf("unapproved cleanup=%#v calls=%d err=%v", current, worker.calls, err)
	}
}

func TestDiscoverClassifiesExactOrphanAsReviewOnly(t *testing.T) {
	store, existing := fixture(t)
	// Keep the fixture cleanup from being executed during this discovery test.
	if err := store.UpdateStatus(context.Background(), existing.Name, existing.UID, cleanupapi.Status{Phase: cleanupapi.PhaseNeedsReview, Reason: "fixture"}); err != nil {
		t.Fatal(err)
	}
	orphan := existing.Spec.Target
	orphan.CopyID = "orphan-copy"
	pool := readyPool(orphan, volumeapi.CopyObservation{Marker: "orphan", Identity: &orphan, Present: true})
	worker := &operator{}
	reconciler := &Reconciler{Store: store, Operator: worker, Client: orphanKubernetesClient(), Namespace: "system", Inventory: inventory{pools: []volumeapi.Pool{pool}, volumes: map[string]volumeapi.State{}}, Interval: time.Second, Now: func() time.Time { return time.Unix(1, 0) }}
	if err := reconciler.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	requests, err := store.List(context.Background())
	if err != nil || len(requests) != 2 || worker.calls != 0 {
		t.Fatalf("requests=%#v calls=%d err=%v", requests, worker.calls, err)
	}
	var review cleanupapi.Cleanup
	for _, request := range requests {
		if request.Spec.Target == orphan {
			review = request
		}
	}
	if review.Name == "" || review.Spec.Approved || review.Status.Phase != cleanupapi.PhaseNeedsReview || review.Status.Reason != "OrphanPreserved" {
		t.Fatalf("review-only orphan=%#v", review)
	}
}

func TestDiscoverReopensReviewFenceWhenCompletedCopyReappears(t *testing.T) {
	store, request := fixture(t)
	worker := &operator{}
	reconciler := &Reconciler{Store: store, Operator: worker, Interval: time.Second, Now: func() time.Time { return time.Unix(1, 0) }}
	if err := reconciler.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	completed, err := store.Get(context.Background(), request.Name)
	if err != nil || completed.Status.Phase != cleanupapi.PhaseCompleted || completed.Status.Receipt == nil || completed.Status.SettledAt == "" {
		t.Fatalf("cleanup did not settle before reappearance: %#v err=%v", completed, err)
	}

	target := request.Spec.Target
	settledAt, err := time.Parse(time.RFC3339Nano, completed.Status.SettledAt)
	if err != nil {
		t.Fatal(err)
	}
	pool := readyPool(target, volumeapi.CopyObservation{Marker: "restored", Identity: &target, Present: true})
	pool.Status.Inventory.ObservedAt = metav1.NewTime(settledAt.Add(time.Second))
	reconciler.Client = orphanKubernetesClient()
	reconciler.Namespace = "system"
	reconciler.Inventory = inventory{
		pools:   []volumeapi.Pool{pool},
		volumes: map[string]volumeapi.State{},
	}
	if err := reconciler.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	review, err := store.Get(context.Background(), request.Name)
	if err != nil || review.Status.Phase != cleanupapi.PhaseNeedsReview || review.Status.Reason != "CopyReappeared" ||
		review.Status.Receipt == nil || review.Status.SettledAt != completed.Status.SettledAt || worker.calls != 1 {
		t.Fatalf("reappeared cleanup=%#v calls=%d err=%v", review, worker.calls, err)
	}
	if err := reconciler.ReconcileAll(context.Background()); err != nil || worker.calls != 1 {
		t.Fatalf("reappeared cleanup reran automatically: calls=%d err=%v", worker.calls, err)
	}
}

func TestDiscoverIgnoresPreSettlementCopyObservation(t *testing.T) {
	store, request := fixture(t)
	worker := &operator{}
	reconciler := &Reconciler{Store: store, Operator: worker, Interval: time.Second, Now: func() time.Time { return time.Unix(1, 0) }}
	if err := reconciler.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	completed, err := store.Get(context.Background(), request.Name)
	if err != nil || completed.Status.Phase != cleanupapi.PhaseCompleted {
		t.Fatalf("cleanup did not settle: %#v err=%v", completed, err)
	}
	target := request.Spec.Target
	reconciler.Client = orphanKubernetesClient()
	reconciler.Namespace = "system"
	reconciler.Inventory = inventory{
		pools:   []volumeapi.Pool{readyPool(target, volumeapi.CopyObservation{Marker: "pre-purge", Identity: &target, Present: true})},
		volumes: map[string]volumeapi.State{},
	}
	if err := reconciler.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, err := store.Get(context.Background(), request.Name)
	if err != nil || current.Status.Phase != cleanupapi.PhaseCompleted || current.Status.Reason == "CopyReappeared" || worker.calls != 1 {
		t.Fatalf("pre-settlement observation reopened cleanup: %#v calls=%d err=%v", current, worker.calls, err)
	}
}

func TestDiscoverFindsSupersededCopyWhilePersistentVolumeRemains(t *testing.T) {
	store, existing := fixture(t)
	if err := store.UpdateStatus(context.Background(), existing.Name, existing.UID, cleanupapi.Status{Phase: cleanupapi.PhaseNeedsReview, Reason: "fixture"}); err != nil {
		t.Fatal(err)
	}
	target := existing.Spec.Target
	target.CopyID, target.Role = "recovered-incoming-copy", volume.RoleIncoming
	current := target
	current.CopyID, current.NodeName, current.Role = "current-serving-copy", "other-node", volume.RoleServing
	client := orphanKubernetesClient()
	_, err := client.CoreV1().PersistentVolumes().Create(context.Background(), &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "live-pv"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{Driver: "csi.shiftpv.io", VolumeHandle: target.VolumeID},
		}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().ConfigMaps("system").Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: target.VolumeID, Namespace: "system", UID: "live-reservation", Labels: map[string]string{
			"app.kubernetes.io/name": "shiftpv", "app.kubernetes.io/component": "volume-reservation",
		}},
		Data: map[string]string{"volumeID": target.VolumeID, "volumeUID": target.VolumeUID, "nodeName": current.NodeName, "capacity": "64"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	reconciler := &Reconciler{
		Store: store, Operator: &operator{}, Client: client, Namespace: "system", Interval: time.Second,
		Now: func() time.Time { return time.Unix(1, 0) },
		Inventory: inventory{
			pools: []volumeapi.Pool{readyPool(target, volumeapi.CopyObservation{Marker: "orphan", Identity: &target, Present: true})},
			volumes: map[string]volumeapi.State{target.VolumeID: {
				UID: target.VolumeUID, Phase: volumeapi.PhaseReady, OwnerNode: current.NodeName, CurrentCopy: &current,
			}},
			moves: []volumeapi.Move{{Name: "recovered", Spec: volumeapi.MoveSpec{VolumeID: target.VolumeID}, Status: volumeapi.MoveStatus{
				Phase: "Blocked", RecoveryPhase: "Recovered", IncomingCopy: &target,
			}}},
		},
	}
	if err := reconciler.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	requests, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range requests {
		if request.Spec.Target == target {
			if request.Spec.ReservationUID != "" || request.Status.Phase != cleanupapi.PhaseNeedsReview || request.Status.Reason != "OrphanPreserved" {
				t.Fatalf("superseded copy review=%#v", request)
			}
			return
		}
	}
	t.Fatal("superseded copy was hidden by volume-wide PersistentVolume authority")
}

func TestApprovedOrphanConvergesAndReleasesExactReservation(t *testing.T) {
	store, existing := fixture(t)
	if err := store.UpdateStatus(context.Background(), existing.Name, existing.UID, cleanupapi.Status{Phase: cleanupapi.PhaseNeedsReview, Reason: "fixture"}); err != nil {
		t.Fatal(err)
	}
	target := existing.Spec.Target
	target.CopyID = "approved-orphan-copy"
	const reservationUID = "reservation-uid"
	client := orphanKubernetesClient()
	_, err := client.CoreV1().ConfigMaps("system").Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: target.VolumeID, Namespace: "system", UID: types.UID(reservationUID), Labels: map[string]string{
			"app.kubernetes.io/name": "shiftpv", "app.kubernetes.io/component": "volume-reservation",
		}},
		Data: map[string]string{"volumeID": target.VolumeID, "volumeUID": target.VolumeUID, "nodeName": target.NodeName, "capacity": "64"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	request, err := store.Ensure(context.Background(), cleanupapi.Spec{
		OperationID: "review-approved-orphan-copy", Target: target, Reason: "OrphanReclaim", ReservationUID: reservationUID,
		Authority: cleanupapi.Authority{Kind: "Namespace", Name: "kube-system", UID: target.InstallationID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateStatus(context.Background(), request.Name, request.UID, cleanupapi.Status{Phase: cleanupapi.PhaseNeedsReview, Reason: "OrphanPreserved"}); err != nil {
		t.Fatal(err)
	}
	approveCleanup(t, store, request.Name)
	worker := &operator{}
	reconciler := &Reconciler{
		Store: store, Operator: worker, Client: client, Namespace: "system", Interval: time.Second, Now: func() time.Time { return time.Unix(1, 0) },
		Inventory: inventory{pools: []volumeapi.Pool{readyPool(target, volumeapi.CopyObservation{Marker: "orphan", Identity: &target, Present: true})}, volumes: map[string]volumeapi.State{}},
	}
	if err := reconciler.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	settled, err := store.Get(context.Background(), request.Name)
	if err != nil || settled.Status.Phase != cleanupapi.PhaseCompleted || worker.calls != 1 {
		t.Fatalf("settled=%#v calls=%d err=%v", settled, worker.calls, err)
	}
	if _, err := client.CoreV1().ConfigMaps("system").Get(context.Background(), target.VolumeID, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("exact orphan reservation remains: %v", err)
	}
}

func TestApprovedOrphanConvergesWhilePoolDeregisters(t *testing.T) {
	store, existing := fixture(t)
	if err := store.UpdateStatus(context.Background(), existing.Name, existing.UID, cleanupapi.Status{Phase: cleanupapi.PhaseNeedsReview, Reason: "fixture"}); err != nil {
		t.Fatal(err)
	}
	target := existing.Spec.Target
	target.CopyID = "deregistering-pool-orphan"
	request, err := store.Ensure(context.Background(), cleanupapi.Spec{
		OperationID: "review-deregistering-pool-orphan", Target: target, Reason: "OrphanReclaim", Approved: true,
		Authority: cleanupapi.Authority{Kind: "Namespace", Name: "kube-system", UID: target.InstallationID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateStatus(context.Background(), request.Name, request.UID, cleanupapi.Status{Phase: cleanupapi.PhaseNeedsReview, Reason: "OrphanPreserved"}); err != nil {
		t.Fatal(err)
	}
	pool := readyPool(target, volumeapi.CopyObservation{Marker: "orphan", Identity: &target, Present: true})
	deletedAt := metav1.NewTime(time.Unix(1, 0))
	pool.DeletionTimestamp = &deletedAt
	pool.Status.Conditions = []metav1.Condition{
		{Type: volumeapi.PoolConditionReady, Status: metav1.ConditionFalse, ObservedGeneration: 1, LastTransitionTime: deletedAt, Reason: "PoolDeregistering", Message: "new placement is closed"},
		{Type: volumeapi.PoolConditionAccessible, Status: metav1.ConditionTrue, ObservedGeneration: 1, LastTransitionTime: deletedAt, Reason: "PathAccessible", Message: "accessible"},
		{Type: volumeapi.PoolConditionWritable, Status: metav1.ConditionTrue, ObservedGeneration: 1, LastTransitionTime: deletedAt, Reason: "PathWritable", Message: "writable"},
		{Type: volumeapi.PoolConditionCapacityReadable, Status: metav1.ConditionTrue, ObservedGeneration: 1, LastTransitionTime: deletedAt, Reason: "CapacityReadable", Message: "capacity readable"},
	}
	worker := &operator{}
	reconciler := &Reconciler{
		Store: store, Operator: worker, Client: orphanKubernetesClient(), Namespace: "system", Interval: time.Second,
		Now:       func() time.Time { return time.Unix(1, 0) },
		Inventory: inventory{pools: []volumeapi.Pool{pool}, volumes: map[string]volumeapi.State{}},
	}
	if err := reconciler.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	settled, err := store.Get(context.Background(), request.Name)
	if err != nil || settled.Status.Phase != cleanupapi.PhaseCompleted || worker.calls != 1 {
		t.Fatalf("deregistering Pool cleanup=%#v calls=%d err=%v", settled, worker.calls, err)
	}
}

func TestApprovedSupersededCopyPreservesLiveVolumeReservation(t *testing.T) {
	store, existing := fixture(t)
	if err := store.UpdateStatus(context.Background(), existing.Name, existing.UID, cleanupapi.Status{Phase: cleanupapi.PhaseNeedsReview, Reason: "fixture"}); err != nil {
		t.Fatal(err)
	}
	target := existing.Spec.Target
	target.CopyID, target.Role = "superseded-copy", volume.RoleRetired
	current := target
	current.PoolName, current.PoolUID, current.VolumeUID, current.CopyID, current.NodeName, current.Role = "current-pool", "current-pool-uid", "replacement-volume-uid", "current-copy", "current-node", volume.RoleServing
	state := volumeapi.State{UID: current.VolumeUID, Phase: volumeapi.PhaseReady, OwnerNode: current.NodeName, CurrentCopy: &current}
	const reservationUID = "live-reservation-uid"
	reservation := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: target.VolumeID, Namespace: "system", UID: types.UID(reservationUID), Labels: map[string]string{
			"app.kubernetes.io/name": "shiftpv", "app.kubernetes.io/component": "volume-reservation",
		}},
		Data: map[string]string{
			"requestName": "live-volume", "volumeID": target.VolumeID, "volumeUID": current.VolumeUID,
			"nodeName": target.NodeName, "capacity": "64",
		},
	}
	client := orphanKubernetesClient()
	if _, err := client.CoreV1().ConfigMaps("system").Create(context.Background(), reservation.DeepCopy(), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().PersistentVolumes().Create(context.Background(), &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "live-pv"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{Driver: "csi.shiftpv.io", VolumeHandle: target.VolumeID},
		}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	observed := inventory{
		pools:   []volumeapi.Pool{readyPool(target, volumeapi.CopyObservation{Marker: "superseded", Identity: &target, Present: true})},
		volumes: map[string]volumeapi.State{target.VolumeID: state},
	}
	worker := &operator{}
	reconciler := &Reconciler{
		Store: store, Operator: worker, Client: client, Namespace: "system", Inventory: observed,
		Interval: time.Second, Now: func() time.Time { return time.Unix(1, 0) },
	}
	if err := reconciler.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	requests, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var request cleanupapi.Cleanup
	for _, candidate := range requests {
		if candidate.Spec.Target == target {
			request = candidate
			break
		}
	}
	if request.Name == "" || request.Spec.ReservationUID != "" || request.Status.Reason != "OrphanPreserved" {
		t.Fatalf("superseded cleanup owns live reservation: %#v", request)
	}
	approveCleanup(t, store, request.Name)
	if err := reconciler.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	settled, err := store.Get(context.Background(), request.Name)
	if err != nil || settled.Status.Phase != cleanupapi.PhaseCompleted || worker.calls != 1 {
		t.Fatalf("superseded cleanup did not settle: cleanup=%#v calls=%d err=%v", settled, worker.calls, err)
	}
	liveReservation, err := client.CoreV1().ConfigMaps("system").Get(context.Background(), target.VolumeID, metav1.GetOptions{})
	if err != nil || string(liveReservation.UID) != reservationUID {
		t.Fatalf("live reservation was removed: reservation=%#v err=%v", liveReservation, err)
	}
	reserved, err := poolcapacity.ReservedBytes([]corev1.ConfigMap{*liveReservation}, map[string]volumeapi.State{target.VolumeID: state}, nil, current.NodeName)
	if err != nil || reserved != 64 {
		t.Fatalf("live capacity accounting failed after superseded cleanup: reserved=%d err=%v", reserved, err)
	}
}

func TestReconcileAllUsesOneAuthoritySnapshotPerCycle(t *testing.T) {
	store, existing := fixture(t)
	target := existing.Spec.Target
	target.CopyID = "one-snapshot-orphan"
	request, err := store.Ensure(context.Background(), cleanupapi.Spec{
		OperationID: "review-one-snapshot-orphan", Target: target, Reason: "OrphanReclaim", Approved: true,
		Authority: cleanupapi.Authority{Kind: "Namespace", Name: "kube-system", UID: target.InstallationID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateStatus(context.Background(), request.Name, request.UID, cleanupapi.Status{Phase: cleanupapi.PhaseNeedsReview, Reason: "OrphanPreserved"}); err != nil {
		t.Fatal(err)
	}
	observed := &countingInventory{inventory: inventory{
		pools:   []volumeapi.Pool{readyPool(target, volumeapi.CopyObservation{Marker: "orphan", Identity: &target, Present: true})},
		volumes: map[string]volumeapi.State{},
	}}
	reconciler := &Reconciler{
		Store: store, Operator: &operator{}, Client: orphanKubernetesClient(), Namespace: "system",
		Inventory: observed, Interval: time.Second, Now: func() time.Time { return time.Unix(1, 0) },
	}
	if err := reconciler.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if observed.poolCalls != 1 || observed.volumeCalls != 1 || observed.moveCalls != 1 {
		t.Fatalf("authority snapshots: pools=%d volumes=%d moves=%d", observed.poolCalls, observed.volumeCalls, observed.moveCalls)
	}
}

func TestApprovedOrphanIsPreservedWhenLiveAuthorityAppears(t *testing.T) {
	for name, mutate := range map[string]func(*fake.Clientset, *volumeapi.Pool, *inventory, volume.CopyIdentity){
		"persistent volume": func(client *fake.Clientset, _ *volumeapi.Pool, _ *inventory, target volume.CopyIdentity) {
			_, _ = client.CoreV1().PersistentVolumes().Create(context.Background(), &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "live-pv"}, Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "csi.shiftpv.io", VolumeHandle: target.VolumeID}}},
			}, metav1.CreateOptions{})
		},
		"published copy": func(_ *fake.Clientset, pool *volumeapi.Pool, _ *inventory, _ volume.CopyIdentity) {
			pool.Status.Inventory.Copies[0].Published = true
		},
		"volume incarnation": func(_ *fake.Clientset, _ *volumeapi.Pool, inventory *inventory, target volume.CopyIdentity) {
			inventory.volumes[target.VolumeID] = volumeapi.State{UID: target.VolumeUID, CurrentCopy: &target}
		},
	} {
		t.Run(name, func(t *testing.T) {
			store, existing := fixture(t)
			if err := store.UpdateStatus(context.Background(), existing.Name, existing.UID, cleanupapi.Status{Phase: cleanupapi.PhaseNeedsReview, Reason: "fixture"}); err != nil {
				t.Fatal(err)
			}
			target := existing.Spec.Target
			target.CopyID = "blocked-orphan-copy"
			request, err := store.Ensure(context.Background(), cleanupapi.Spec{
				OperationID: "review-blocked-orphan-copy", Target: target, Reason: "OrphanReclaim",
				Authority: cleanupapi.Authority{Kind: "Namespace", Name: "kube-system", UID: target.InstallationID},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.UpdateStatus(context.Background(), request.Name, request.UID, cleanupapi.Status{Phase: cleanupapi.PhaseNeedsReview, Reason: "OrphanPreserved"}); err != nil {
				t.Fatal(err)
			}
			approveCleanup(t, store, request.Name)
			client := orphanKubernetesClient()
			pool := readyPool(target, volumeapi.CopyObservation{Marker: "orphan", Identity: &target, Present: true})
			observed := inventory{pools: []volumeapi.Pool{pool}, volumes: map[string]volumeapi.State{}}
			mutate(client, &observed.pools[0], &observed, target)
			worker := &operator{}
			reconciler := &Reconciler{Store: store, Operator: worker, Client: client, Namespace: "system", Inventory: observed, Interval: time.Second, Now: func() time.Time { return time.Unix(1, 0) }}
			if err := reconciler.ReconcileAll(context.Background()); err != nil {
				t.Fatal(err)
			}
			preserved, err := store.Get(context.Background(), request.Name)
			if err != nil || preserved.Status.Phase != cleanupapi.PhaseNeedsReview || worker.calls != 0 {
				t.Fatalf("preserved=%#v calls=%d err=%v", preserved, worker.calls, err)
			}
		})
	}
}

func TestApprovedOrphanAutomaticallyResumesAfterMountClears(t *testing.T) {
	store, existing := fixture(t)
	if err := store.UpdateStatus(context.Background(), existing.Name, existing.UID, cleanupapi.Status{Phase: cleanupapi.PhaseNeedsReview, Reason: "fixture"}); err != nil {
		t.Fatal(err)
	}
	target := existing.Spec.Target
	target.CopyID = "mounted-orphan-copy"
	request, err := store.Ensure(context.Background(), cleanupapi.Spec{
		OperationID: "review-mounted-orphan-copy", Target: target, Reason: "OrphanReclaim", Approved: true,
		Authority: cleanupapi.Authority{Kind: "Namespace", Name: "kube-system", UID: target.InstallationID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateStatus(context.Background(), request.Name, request.UID, cleanupapi.Status{Phase: cleanupapi.PhaseNeedsReview, Reason: "CopyMounted"}); err != nil {
		t.Fatal(err)
	}
	client := orphanKubernetesClient()
	pool := readyPool(target, volumeapi.CopyObservation{Marker: "orphan", Identity: &target, Present: true, Published: true})
	observed := &inventory{pools: []volumeapi.Pool{pool}, volumes: map[string]volumeapi.State{}}
	worker := &operator{}
	reconciler := &Reconciler{Store: store, Operator: worker, Client: client, Namespace: "system", Inventory: observed, Interval: time.Second, Now: func() time.Time { return time.Unix(1, 0) }}
	if err := reconciler.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if worker.calls != 0 {
		t.Fatal("mounted orphan executed")
	}
	observed.pools[0].Status.Inventory.Copies[0].Published = false
	if err := reconciler.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	settled, err := store.Get(context.Background(), request.Name)
	if err != nil || settled.Status.Phase != cleanupapi.PhaseCompleted || worker.calls != 1 {
		t.Fatalf("cleared mount did not converge: settled=%#v calls=%d err=%v", settled, worker.calls, err)
	}
}

func TestOrphanClassificationPreservesEveryUnprovenBoundary(t *testing.T) {
	now := time.Unix(1, 0)
	target := volume.CopyIdentity{
		InstallationID: "installation", PoolName: "pool", PoolUID: "pool-uid", VolumeID: "shiftpv-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		VolumeUID: "volume-uid", CopyID: "copy-id", NodeName: "node", Role: volume.RoleServing,
	}
	base := func() orphanSnapshot {
		return orphanSnapshot{
			pools:   []volumeapi.Pool{readyPool(target, volumeapi.CopyObservation{Marker: "copy", Identity: &target, Present: true})},
			volumes: map[string]volumeapi.State{}, volumesByPV: map[string]struct{}{}, reservations: map[string]corev1.ConfigMap{},
		}
	}
	for name, test := range map[string]struct {
		mutate         func(*orphanSnapshot)
		reservationUID string
		ready          bool
		reason         string
	}{
		"safe":              {ready: true, reason: "OrphanReady"},
		"persistent volume": {mutate: func(s *orphanSnapshot) { s.volumesByPV[target.VolumeID] = struct{}{} }, reason: "PersistentVolumePresent"},
		"volume authority":  {mutate: func(s *orphanSnapshot) { s.volumes[target.VolumeID] = volumeapi.State{UID: target.VolumeUID} }, reason: "VolumeAuthorityPresent"},
		"move authority": {mutate: func(s *orphanSnapshot) {
			s.moves = []volumeapi.Move{{Name: "move", Status: volumeapi.MoveStatus{Phase: "Copying", SourceCopy: &target}}}
		}, reason: "MoveAuthorityPresent"},
		"recovered move released authority": {mutate: func(s *orphanSnapshot) {
			s.moves = []volumeapi.Move{{Name: "move", Status: volumeapi.MoveStatus{Phase: "Blocked", RecoveryPhase: "Recovered", SourceCopy: &target}}}
		}, ready: true, reason: "OrphanReady"},
		"superseded volume and persistent volume released exact copy": {mutate: func(s *orphanSnapshot) {
			current := target
			current.VolumeUID, current.CopyID, current.NodeName, current.Role = "replacement-volume-uid", "current-copy", "other-node", volume.RoleServing
			s.volumes[target.VolumeID] = volumeapi.State{UID: current.VolumeUID, CurrentCopy: &current}
			s.volumesByPV[target.VolumeID] = struct{}{}
			s.reservations[target.VolumeID] = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name: target.VolumeID, UID: "live-reservation", Labels: map[string]string{
					"app.kubernetes.io/name": "shiftpv", "app.kubernetes.io/component": "volume-reservation",
				},
			}, Data: map[string]string{"volumeID": target.VolumeID, "volumeUID": current.VolumeUID}}
		}, ready: true, reason: "OrphanReady"},
		"superseded copy missing live reservation": {mutate: func(s *orphanSnapshot) {
			current := target
			current.CopyID, current.NodeName, current.Role = "current-copy", "other-node", volume.RoleServing
			s.volumes[target.VolumeID] = volumeapi.State{UID: target.VolumeUID, CurrentCopy: &current}
		}, reason: "LiveReservationMissing"},
		"pool identity":         {mutate: func(s *orphanSnapshot) { s.pools = nil }, reason: "PoolIdentityUnavailable"},
		"pool readiness":        {mutate: func(s *orphanSnapshot) { s.pools[0].Status.Conditions = nil }, reason: "PoolUnavailable"},
		"inventory unavailable": {mutate: func(s *orphanSnapshot) { s.pools[0].Status.Inventory = nil }, reason: "ObservationUnavailable"},
		"inventory stale": {mutate: func(s *orphanSnapshot) {
			s.pools[0].Status.Inventory.ObservedAt = metav1.NewTime(now.Add(-time.Hour))
		}, reason: "ObservationStale"},
		"copy problem": {mutate: func(s *orphanSnapshot) { s.pools[0].Status.Inventory.Copies[0].Problem = "IdentityMismatch" }, reason: "CopyIdentityProblem"},
		"copy absent":  {mutate: func(s *orphanSnapshot) { s.pools[0].Status.Inventory.Copies[0].Present = false }, reason: "CopyNotObserved"},
		"copy mounted": {mutate: func(s *orphanSnapshot) { s.pools[0].Status.Inventory.Copies[0].Published = true }, reason: "CopyMounted"},
		"truncated before copy": {mutate: func(s *orphanSnapshot) {
			s.pools[0].Status.Inventory.Copies = nil
			s.pools[0].Status.Inventory.Truncated = true
		}, reason: "ObservationTruncated"},
		"copy not observed": {mutate: func(s *orphanSnapshot) { s.pools[0].Status.Inventory.Copies = nil }, reason: "CopyNotObserved"},
		"reservation unbound": {mutate: func(s *orphanSnapshot) {
			s.reservations[target.VolumeID] = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: target.VolumeID, UID: "reservation"}}
		}, reason: "ReservationIdentityUnknown"},
		"reservation replaced": {reservationUID: "expected", mutate: func(s *orphanSnapshot) {
			s.reservations[target.VolumeID] = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: target.VolumeID, UID: "replacement"}, Data: map[string]string{"volumeID": target.VolumeID, "volumeUID": target.VolumeUID}}
		}, reason: "ReservationIdentityChanged"},
	} {
		t.Run(name, func(t *testing.T) {
			snapshot := base()
			if test.mutate != nil {
				test.mutate(&snapshot)
			}
			ready, reason, message := snapshot.classify(target, test.reservationUID, now)
			if ready != test.ready || reason != test.reason || message == "" {
				t.Fatalf("classification ready=%v reason=%q message=%q", ready, reason, message)
			}
		})
	}
}

func TestDiscoverPreservesReferencedAndUnsafeObservations(t *testing.T) {
	store, existing := fixture(t)
	current := existing.Spec.Target
	current.CopyID = "current-copy"
	incoming := current
	incoming.CopyID, incoming.Role = "incoming-copy", volume.RoleIncoming
	destination := current
	destination.CopyID = "destination-copy"
	wrongPool := current
	wrongPool.CopyID, wrongPool.PoolUID = "wrong-pool-copy", "replacement-pool-uid"
	malformed := current
	malformed.CopyID = "../malformed"
	cleanupTarget := existing.Spec.Target
	pool := volumeapi.Pool{
		Name: current.PoolName, UID: current.PoolUID, NodeName: current.NodeName,
		Status: volumeapi.PoolStatus{Inventory: &volumeapi.PoolInventory{Valid: true, Copies: []volumeapi.CopyObservation{
			{Marker: "current", Identity: &current, Present: true},
			{Marker: "incoming", Identity: &incoming, Present: true},
			{Marker: "destination", Identity: &destination, Present: true},
			{Marker: "cleanup", Identity: &cleanupTarget, Present: true},
			{Marker: "wrong-pool", Identity: &wrongPool, Present: true},
			{Marker: "malformed", Identity: &malformed, Present: true},
			{Marker: "missing", Identity: &current, Present: false},
			{Marker: "problem", Identity: &current, Present: true, Problem: "IdentityMismatch"},
			{Marker: "unknown", Present: true, Problem: "UnrecordedPath"},
		}}},
	}
	move := volumeapi.Move{
		Name: "active-move", Spec: volumeapi.MoveSpec{VolumeID: current.VolumeID},
		Status: volumeapi.MoveStatus{SourceCopy: &current, IncomingCopy: &incoming, DestinationCopy: &destination},
	}
	reconciler := &Reconciler{Store: store, Operator: &operator{}, Inventory: inventory{
		pools:   []volumeapi.Pool{{Name: "empty"}, pool},
		volumes: map[string]volumeapi.State{current.VolumeID: {CurrentCopy: &current, ActiveMove: move.Name}},
		moves:   []volumeapi.Move{move},
	}, Client: orphanKubernetesClient(), Namespace: "system", Interval: time.Second}
	if err := reconciler.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	requests, err := store.List(context.Background())
	if err != nil || len(requests) != 1 || requests[0].Name != existing.Name {
		t.Fatalf("unsafe or referenced observation created cleanup: %#v err=%v", requests, err)
	}
}

func TestDiscoverFailsClosedOnInventoryError(t *testing.T) {
	store, _ := fixture(t)
	for _, failure := range []string{"pools", "volumes", "moves"} {
		t.Run(failure, func(t *testing.T) {
			reconciler := &Reconciler{Store: store, Operator: &operator{}, Client: orphanKubernetesClient(), Namespace: "system", Inventory: inventory{fail: failure}, Interval: time.Second}
			if err := reconciler.Discover(context.Background()); err == nil {
				t.Fatalf("%s inventory failure was hidden", failure)
			}
		})
	}
	if err := (&Reconciler{Store: store, Operator: &operator{}, Interval: time.Second}).Discover(context.Background()); err != nil {
		t.Fatalf("nil inventory should be inert: %v", err)
	}
}

func fixture(t *testing.T) (*cleanupapi.Store, cleanupapi.Cleanup) {
	t.Helper()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{cleanupapi.Resource: "ShiftPVCleanupList"})
	client.PrependReactor("create", "shiftpvcleanups", func(action k8stesting.Action) (bool, runtime.Object, error) {
		object := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		object.SetUID("cleanup-uid")
		return false, nil, nil
	})
	store := &cleanupapi.Store{Client: client}
	target := volume.CopyIdentity{InstallationID: "installation", PoolName: "pool", PoolUID: "pool-uid", VolumeID: "shiftpv-0123456789abcdef0123456789abcdef", VolumeUID: "volume-uid", CopyID: "copy", NodeName: "node", Role: volume.RoleServing}
	request, err := store.Ensure(context.Background(), cleanupapi.Spec{OperationID: "cleanup-volume", Target: target, Reason: "VolumeDelete", Approved: true, Authority: cleanupapi.Authority{Kind: "ShiftPVVolume", Name: target.VolumeID, UID: target.VolumeUID}})
	if err != nil {
		t.Fatal(err)
	}
	return store, request
}

func approveCleanup(t *testing.T, store *cleanupapi.Store, name string) {
	t.Helper()
	resource := store.Client.Resource(cleanupapi.Resource)
	object, err := resource.Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(object.Object, true, "spec", "approved"); err != nil {
		t.Fatal(err)
	}
	if _, err := resource.Update(context.Background(), object, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}
