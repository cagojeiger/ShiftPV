package readiness

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
)

var testTime = time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)

type fakeInspector struct{ result Result }

func (f fakeInspector) Inspect(volumeapi.Pool) Result { return f.result }

type fakeRepository struct {
	pool       volumeapi.Pool
	err        error
	status     volumeapi.PoolStatus
	statusSets int
}

func (f *fakeRepository) PoolForNode(context.Context, string) (volumeapi.Pool, error) {
	return f.pool, f.err
}

func (f *fakeRepository) SetPoolStatus(_ context.Context, name, node string, status volumeapi.PoolStatus) error {
	if name != f.pool.Name || node != f.pool.NodeName {
		return errors.New("identity mismatch")
	}
	f.status = status
	f.statusSets++
	return nil
}

func TestReconcilePersistsReadyConditionsAndPreservesTransitionTime(t *testing.T) {
	old := metav1.NewTime(testTime.Add(-time.Hour))
	repository := &fakeRepository{pool: volumeapi.Pool{
		Name: "pool-a", NodeName: "node-a", Generation: 3,
		Status: volumeapi.PoolStatus{Conditions: []metav1.Condition{
			{
				Type: volumeapi.PoolConditionAccessible, Status: metav1.ConditionTrue,
				ObservedGeneration: 2, LastTransitionTime: old, Reason: "DirectoryAccessible", Message: "old",
			},
			{
				Type: volumeapi.PoolConditionMounted, Status: metav1.ConditionTrue,
				ObservedGeneration: 2, LastTransitionTime: old, Reason: "Mounted", Message: "legacy",
			},
		}},
	}}
	ok := Check{OK: true, Known: true, Reason: "DirectoryAccessible", Message: "old"}
	reconciler := &Reconciler{
		NodeName: "node-a", Pools: repository, Inspector: fakeInspector{Result{Accessible: ok,
			Writable:         Check{OK: true, Known: true, Reason: "Writable", Message: "write"},
			CapacityReadable: Check{OK: true, Known: true, Reason: "CapacityReadable", Message: "capacity"}}},
		Interval: time.Minute, Now: func() time.Time { return testTime },
	}
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repository.status.ObservedGeneration != 3 || !repository.status.LastProbeTime.Equal(&metav1.Time{Time: testTime}) {
		t.Fatalf("status = %#v", repository.status)
	}
	ready := meta.FindStatusCondition(repository.status.Conditions, volumeapi.PoolConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.Reason != "PoolReady" {
		t.Fatalf("ready = %#v", ready)
	}
	accessible := meta.FindStatusCondition(repository.status.Conditions, volumeapi.PoolConditionAccessible)
	if accessible == nil || !accessible.LastTransitionTime.Equal(&old) || accessible.ObservedGeneration != 3 {
		t.Fatalf("accessible = %#v", accessible)
	}
	if mounted := meta.FindStatusCondition(repository.status.Conditions, volumeapi.PoolConditionMounted); mounted != nil {
		t.Fatalf("legacy mounted condition was not removed: %#v", mounted)
	}
}

func TestReconcileRecordsFailureAndAllowsMissingRegistration(t *testing.T) {
	repository := &fakeRepository{pool: volumeapi.Pool{Name: "pool-a", NodeName: "node-a", Generation: 1}}
	reconciler := &Reconciler{
		NodeName: "node-a", Pools: repository, Inspector: fakeInspector{Result{
			Accessible:       Check{OK: true, Known: true, Reason: "DirectoryAccessible", Message: "accessible"},
			Writable:         Check{Known: true, Reason: "PermissionDenied", Message: "denied"},
			CapacityReadable: Check{OK: true, Known: true, Reason: "CapacityReadable", Message: "capacity"},
		}}, Interval: time.Minute, Now: func() time.Time { return testTime },
	}
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	ready := meta.FindStatusCondition(repository.status.Conditions, volumeapi.PoolConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "PermissionDenied" {
		t.Fatalf("ready = %#v", ready)
	}
	repository.err = volumeapi.ErrPoolNotFound
	if err := reconciler.Reconcile(context.Background()); err != nil || repository.statusSets != 1 {
		t.Fatalf("missing Pool: sets=%d err=%v", repository.statusSets, err)
	}
}

func TestReconcilerValidatesConfiguration(t *testing.T) {
	if err := (&Reconciler{}).Reconcile(context.Background()); err == nil {
		t.Fatal("invalid configuration accepted")
	}
}
