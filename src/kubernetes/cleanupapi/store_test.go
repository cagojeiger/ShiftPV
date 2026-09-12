package cleanupapi

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestEnsureIsIdempotentAndBindsOperationIdentity(t *testing.T) {
	store := newStore()
	spec := validSpec()
	first, err := store.Ensure(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Ensure(context.Background(), spec)
	if err != nil || first.Name != second.Name {
		t.Fatalf("idempotent ensure: %#v %v", second, err)
	}
	changed := spec
	changed.Target.CopyID = "other-copy"
	changed.OperationID = spec.OperationID
	if _, err := store.Ensure(context.Background(), changed); err != nil {
		t.Fatalf("cleanup UID keeps operation identity independent: %v", err)
	}
	rebound := spec
	rebound.OperationID = "replacement-operation"
	if _, err := store.Ensure(context.Background(), rebound); !errors.Is(err, ErrConflict) {
		t.Fatalf("copy identity was rebound: %v", err)
	}
	items, err := store.List(context.Background())
	if err != nil || len(items) != 2 {
		t.Fatalf("list=%#v err=%v", items, err)
	}
	found := false
	for _, item := range items {
		found = found || reflect.DeepEqual(item.Spec, spec)
	}
	if !found {
		t.Fatalf("original intent missing from %#v", items)
	}
	forVolume, err := store.ListForVolume(context.Background(), spec.Target.VolumeID)
	if err != nil || len(forVolume) != 2 {
		t.Fatalf("volume-indexed cleanup list=%#v err=%v", forVolume, err)
	}
	for _, item := range forVolume {
		if item.Spec.Target.VolumeID != spec.Target.VolumeID {
			t.Fatalf("volume-indexed cleanup leaked another volume: %#v", item)
		}
	}
}

func TestSpecRejectsUnsafeAndIncompleteIdentity(t *testing.T) {
	for name, mutate := range map[string]func(*Spec){
		"path operation":               func(s *Spec) { s.OperationID = "../escape" },
		"root pool":                    func(s *Spec) { s.Target.PoolName = "/" },
		"invalid volume":               func(s *Spec) { s.Target.VolumeID = "volume" },
		"invalid role":                 func(s *Spec) { s.Target.Role = "Unknown" },
		"invalid reason":               func(s *Spec) { s.Reason = "Age" },
		"invalid owner":                func(s *Spec) { s.Authority.Kind = "Label" },
		"invalid reservation UID":      func(s *Spec) { s.ReservationUID = "../reservation" },
		"orphan authority mismatch":    func(s *Spec) { s.Authority.Name = "default" },
		"orphan installation mismatch": func(s *Spec) { s.Authority.UID = "replacement" },
		"unapproved move": func(s *Spec) {
			s.Reason, s.Authority.Kind, s.Authority.Name, s.Authority.UID, s.Approved = "MoveSource", "ShiftPVMove", "move", "move-uid", false
		},
		"unapproved volume delete": func(s *Spec) {
			s.Reason, s.Authority.Kind, s.Authority.Name, s.Authority.UID, s.Approved = "VolumeDelete", "ShiftPVVolume", s.Target.VolumeID, s.Target.VolumeUID, false
		},
	} {
		t.Run(name, func(t *testing.T) {
			spec := validSpec()
			mutate(&spec)
			if err := spec.Validate(); err == nil {
				t.Fatal("invalid cleanup intent accepted")
			}
		})
	}
	move := validSpec()
	move.Reason, move.Authority = "MoveSource", Authority{Kind: "ShiftPVMove", Name: "move", UID: "move-uid"}
	if err := move.Validate(); err != nil {
		t.Fatalf("valid move cleanup rejected: %v", err)
	}
	deletion := validSpec()
	deletion.Reason = "VolumeDelete"
	deletion.Authority = Authority{Kind: "ShiftPVVolume", Name: deletion.Target.VolumeID, UID: deletion.Target.VolumeUID}
	if err := deletion.Validate(); err != nil {
		t.Fatalf("valid volume cleanup rejected: %v", err)
	}
}

func TestStatusRequiresExecutorReceiptAndSettlement(t *testing.T) {
	store := newStore()
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	store.Now = func() time.Time { return now }
	cleanup, err := store.Ensure(context.Background(), validSpec())
	if err != nil {
		t.Fatal(err)
	}
	setUID(t, store, cleanup.Name, "cleanup-uid")
	cleanup, _ = store.Get(context.Background(), cleanup.Name)
	executor := &Executor{JobName: "cleanup-job", JobUID: "job-uid", NodeName: "node-a"}
	if err := store.UpdateStatus(context.Background(), cleanup.Name, cleanup.UID, Status{Phase: PhaseRunning, Executor: executor}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateStatus(context.Background(), cleanup.Name, cleanup.UID, Status{Phase: PhaseCompleted, Executor: executor}); !errors.Is(err, ErrConflict) {
		t.Fatalf("completion without receipt accepted: %v", err)
	}
	receipt := &Receipt{OperationID: cleanup.Spec.OperationID, ExecutorUID: executor.JobUID, ObservedAt: now.Format(time.RFC3339Nano), Retired: true, Purged: true}
	if err := store.UpdateStatus(context.Background(), cleanup.Name, cleanup.UID, Status{Phase: PhaseVerifying, Executor: executor, Receipt: receipt}); err != nil {
		t.Fatal(err)
	}
	settled := now.Add(time.Second).Format(time.RFC3339Nano)
	if err := store.UpdateStatus(context.Background(), cleanup.Name, cleanup.UID, Status{Phase: PhaseCompleted, Executor: executor, Receipt: receipt, SettledAt: settled}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), cleanup.Name)
	if err != nil || got.Status.Phase != PhaseCompleted || got.Status.SettledAt != settled {
		t.Fatalf("completed=%#v err=%v", got, err)
	}
	if err := store.UpdateStatus(context.Background(), cleanup.Name, cleanup.UID, Status{
		Phase: PhaseNeedsReview, Reason: "IdentityMismatch", Executor: executor, Receipt: receipt, SettledAt: settled,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("completed cleanup reopened without reappearance evidence: %v", err)
	}
	if err := store.UpdateStatus(context.Background(), cleanup.Name, cleanup.UID, Status{
		Phase: PhaseNeedsReview, Reason: "CopyReappeared", Executor: executor, Receipt: receipt, SettledAt: settled,
	}); err != nil {
		t.Fatalf("reappeared exact copy did not reopen a review fence: %v", err)
	}
	review, err := store.Get(context.Background(), cleanup.Name)
	if err != nil || review.Status.Phase != PhaseNeedsReview || review.Status.Receipt == nil || review.Status.SettledAt != settled {
		t.Fatalf("reappearance review lost settlement evidence: %#v err=%v", review, err)
	}
	if err := store.UpdateStatus(context.Background(), cleanup.Name, cleanup.UID, Status{
		Phase: PhasePending, Executor: executor, Receipt: receipt, SettledAt: settled,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("settled cleanup execution was restarted: %v", err)
	}
}

func TestStatusRejectsStaleUIDReplacementAndTerminalReopen(t *testing.T) {
	store := newStore()
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	store.Now = func() time.Time { return now }
	cleanup, _ := store.Ensure(context.Background(), validSpec())
	setUID(t, store, cleanup.Name, "cleanup-uid")
	cleanup, _ = store.Get(context.Background(), cleanup.Name)
	executor := &Executor{JobName: "job", JobUID: "job-uid", NodeName: "node-a"}
	if err := store.UpdateStatus(context.Background(), cleanup.Name, "replaced", Status{Phase: PhaseRunning, Executor: executor}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale UID accepted: %v", err)
	}
	if err := store.UpdateStatus(context.Background(), cleanup.Name, cleanup.UID, Status{Phase: PhaseNeedsReview, Reason: "IdentityMismatch"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateStatus(context.Background(), cleanup.Name, cleanup.UID, Status{Phase: PhasePending}); err != nil {
		t.Fatalf("approved orphan did not resume from review: %v", err)
	}
}

func TestStatusRejectsExecutionBeforeOrphanApproval(t *testing.T) {
	store := newStore()
	spec := validSpec()
	spec.Approved = false
	cleanup, err := store.Ensure(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	setUID(t, store, cleanup.Name, "cleanup-uid")
	cleanup, _ = store.Get(context.Background(), cleanup.Name)
	executor := &Executor{JobName: "job", JobUID: "job-uid", NodeName: cleanup.Spec.Target.NodeName}
	if err := store.UpdateStatus(context.Background(), cleanup.Name, cleanup.UID, Status{Phase: PhaseRunning, Executor: executor}); !errors.Is(err, ErrConflict) {
		t.Fatalf("unapproved orphan executor started: %v", err)
	}
}

func TestStoreRejectsInvalidConfigurationVolumeAndPersistedObjects(t *testing.T) {
	var unavailable *Store
	if _, err := unavailable.Ensure(context.Background(), validSpec()); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := newStore().ListForVolume(context.Background(), "not-a-shiftpv-volume"); err == nil {
		t.Fatal("invalid volume selector accepted")
	}

	specMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(func() *Spec { value := validSpec(); return &value }())
	if err != nil {
		t.Fatal(err)
	}
	invalidSpec := validSpec()
	invalidSpec.OperationID = "../invalid"
	invalidSpecMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&invalidSpec)
	if err != nil {
		t.Fatal(err)
	}
	for name, object := range map[string]*unstructured.Unstructured{
		"missing spec":   {Object: map[string]any{"metadata": map[string]any{"name": "missing"}}},
		"malformed spec": {Object: map[string]any{"metadata": map[string]any{"name": "malformed"}, "spec": "invalid"}},
		"invalid spec":   {Object: map[string]any{"metadata": map[string]any{"name": "invalid"}, "spec": invalidSpecMap}},
		"malformed status": {Object: map[string]any{
			"metadata": map[string]any{"name": "status"}, "spec": specMap, "status": "invalid",
		}},
		"invalid status fields": {Object: map[string]any{
			"metadata": map[string]any{"name": "status-fields"}, "spec": specMap, "status": map[string]any{"executor": "invalid"},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := fromUnstructured(object); err == nil {
				t.Fatal("corrupt persisted cleanup accepted")
			}
		})
	}
}

func validSpec() Spec {
	return Spec{
		OperationID: "cleanup-operation-a",
		Target: CopyIdentity{
			InstallationID: "installation-a", PoolName: "pool-a", PoolUID: "pool-uid-a",
			VolumeID: "shiftpv-11111111111111111111111111111111", VolumeUID: "volume-uid-a",
			CopyID: "copy-a", NodeName: "node-a", Role: "Retired",
		},
		Reason: "OrphanReclaim", Authority: Authority{Kind: "Namespace", Name: "kube-system", UID: "installation-a"},
		Approved: true,
	}
}

func newStore() *Store {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{Resource: "ShiftPVCleanupList"})
	return &Store{Client: client}
}

func setUID(t *testing.T, store *Store, name, uid string) {
	t.Helper()
	resource := store.Client.Resource(Resource)
	object, err := resource.Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	object.SetUID(types.UID(uid))
	if _, err := resource.Update(context.Background(), object, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}
