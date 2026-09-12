package poolcontroller

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	uninstallcheck "github.com/cagojeiger/ShiftPV/src/lifecycle/uninstall"
)

type memoryPools struct {
	pools     []volumeapi.Pool
	ensured   []string
	removed   []string
	approved  []string
	listError error
}

func (m *memoryPools) ListPools(context.Context) ([]volumeapi.Pool, error) {
	return m.pools, m.listError
}

func (m *memoryPools) EnsurePoolFinalizer(_ context.Context, name, uid string) error {
	m.ensured = append(m.ensured, name+"/"+uid)
	return nil
}

func (m *memoryPools) RemovePoolFinalizer(_ context.Context, name, uid string) error {
	m.removed = append(m.removed, name+"/"+uid)
	return nil
}

func (m *memoryPools) ApprovePoolIdentityRelease(_ context.Context, name, uid string) error {
	m.approved = append(m.approved, name+"/"+uid)
	return nil
}

type memorySafety struct {
	report uninstallcheck.Report
	err    error
	after  time.Time
}

type memoryQuiesce struct {
	quiescing bool
	err       error
}

func (m memoryQuiesce) Quiescing(context.Context) (string, bool, error) {
	return "attempt", m.quiescing, m.err
}

func (m *memorySafety) CheckPoolDeleteAfter(_ context.Context, _ string, _ types.UID, after time.Time) (uninstallcheck.Report, error) {
	m.after = after
	return m.report, m.err
}

func TestReconcileProtectsActivePool(t *testing.T) {
	pools := &memoryPools{pools: []volumeapi.Pool{{Name: "pool", UID: "pool-uid"}}}
	reconciler := &Reconciler{Pools: pools, Safety: &memorySafety{}, Quiesce: memoryQuiesce{}, Interval: time.Second}
	if err := reconciler.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pools.ensured) != 1 || pools.ensured[0] != "pool/pool-uid" || len(pools.removed) != 0 {
		t.Fatalf("ensure=%v remove=%v", pools.ensured, pools.removed)
	}
}

func TestReconcileDoesNotReinstallProtectionDuringUninstallQuiesce(t *testing.T) {
	pools := &memoryPools{pools: []volumeapi.Pool{{Name: "pool", UID: "pool-uid"}}}
	reconciler := &Reconciler{Pools: pools, Safety: &memorySafety{}, Quiesce: memoryQuiesce{quiescing: true}, Interval: time.Second}
	if err := reconciler.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pools.ensured) != 0 {
		t.Fatalf("protection reinstalled during quiesce: %v", pools.ensured)
	}
}

func TestReconcileWaitsForPostDeleteSafety(t *testing.T) {
	deletedAt := metav1.NewTime(time.Date(2026, time.September, 12, 8, 0, 0, 0, time.UTC))
	pool := volumeapi.Pool{Name: "pool", UID: "pool-uid", DeletionTimestamp: &deletedAt, Finalizers: []string{volumeapi.PoolProtectionFinalizer}}
	pools := &memoryPools{pools: []volumeapi.Pool{pool}}
	safety := &memorySafety{report: uninstallcheck.Report{Blockers: []uninstallcheck.Blocker{{Kind: uninstallcheck.PoolInventoryBlockerKind, Name: pool.Name}}}}
	reconciler := &Reconciler{Pools: pools, Safety: safety, Quiesce: memoryQuiesce{}, Interval: time.Second}

	if err := reconciler.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !safety.after.Equal(deletedAt.Time) || len(pools.removed) != 0 {
		t.Fatalf("after=%s remove=%v", safety.after, pools.removed)
	}

	safety.report = uninstallcheck.Report{}
	if err := reconciler.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pools.approved) != 1 || pools.approved[0] != "pool/pool-uid" || len(pools.removed) != 0 {
		t.Fatalf("approve=%v remove=%v", pools.approved, pools.removed)
	}
	pools.pools[0].IdentityReleaseApproval = pool.UID
	pools.pools[0].Status.Conditions = []metav1.Condition{{
		Type: volumeapi.PoolConditionIdentityReleased, Status: metav1.ConditionTrue, ObservedGeneration: pool.Generation, Reason: "PoolIdentityReleased",
	}}
	if err := reconciler.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pools.removed) != 1 || pools.removed[0] != "pool/pool-uid" {
		t.Fatalf("remove=%v", pools.removed)
	}
}

func TestReconcileFailsClosedOnObservationError(t *testing.T) {
	deletedAt := metav1.Now()
	pool := volumeapi.Pool{Name: "pool", UID: "pool-uid", DeletionTimestamp: &deletedAt, Finalizers: []string{volumeapi.PoolProtectionFinalizer}}
	pools := &memoryPools{pools: []volumeapi.Pool{pool}}
	safety := &memorySafety{err: errors.New("API unavailable")}
	reconciler := &Reconciler{Pools: pools, Safety: safety, Quiesce: memoryQuiesce{}, Interval: time.Second}
	if err := reconciler.ReconcileAll(context.Background()); err == nil || len(pools.removed) != 0 {
		t.Fatalf("error=%v remove=%v", err, pools.removed)
	}
}
