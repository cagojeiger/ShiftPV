package uninstall

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/cleanupapi"
	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/volume"
)

type memoryRepository struct {
	volumes    map[string]volumeapi.State
	moves      []volumeapi.Move
	pools      []volumeapi.Pool
	volumesErr error
	movesErr   error
	poolsErr   error
}

type cleanupRepository struct {
	items []cleanupapi.Cleanup
	err   error
}

func (r cleanupRepository) List(context.Context) ([]cleanupapi.Cleanup, error) { return r.items, r.err }

func TestCheckBlocksUnsettledCleanupContract(t *testing.T) {
	items := []cleanupapi.Cleanup{
		{Name: "pending", Spec: cleanupapi.Spec{OperationID: "cleanup-a", Target: volume.CopyIdentity{VolumeID: "shiftpv-0123456789abcdef0123456789abcdef"}}, Status: cleanupapi.Status{Phase: cleanupapi.PhaseRunning}},
		{Name: "settled", Status: cleanupapi.Status{Phase: cleanupapi.PhaseCompleted}},
	}
	checker := &Checker{Client: fake.NewClientset(), Volumes: &memoryRepository{}, Cleanups: cleanupRepository{items: items}, StorageClassName: "shiftpv", Namespace: "shiftpv-system"}
	report, err := checker.Check(context.Background())
	if err != nil || len(report.Blockers) != 1 || report.Blockers[0].Kind != "ShiftPVCleanup" || report.Blockers[0].Name != "pending" {
		t.Fatalf("cleanup blockers=%#v err=%v", report.Blockers, err)
	}
	checker.Cleanups = cleanupRepository{err: errors.New("cleanup api unavailable")}
	if _, err := checker.Check(context.Background()); err == nil {
		t.Fatal("cleanup API failure did not close uninstall gate")
	}
}

func (m *memoryRepository) ListVolumes(context.Context) (map[string]volumeapi.State, error) {
	return m.volumes, m.volumesErr
}

func (m *memoryRepository) ListMoves(context.Context) ([]volumeapi.Move, error) {
	return m.moves, m.movesErr
}

func (m *memoryRepository) ListPools(context.Context) ([]volumeapi.Pool, error) {
	return m.pools, m.poolsErr
}

func TestCheckAllowsEmptyCluster(t *testing.T) {
	checker := &Checker{Client: fake.NewClientset(), Volumes: &memoryRepository{}, Cleanups: cleanupRepository{}, StorageClassName: "shiftpv", Namespace: "shiftpv-system"}
	report, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !report.Safe() {
		t.Fatalf("Check() blockers = %#v", report.Blockers)
	}
}

func TestCheckBlocksReservationUntilItIsReleased(t *testing.T) {
	reservation := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "shiftpv-a", Namespace: "shiftpv-system", Labels: map[string]string{
			"app.kubernetes.io/name": "shiftpv", "app.kubernetes.io/component": "volume-reservation",
		},
	}, Data: map[string]string{"volumeID": "shiftpv-a", "volumeUID": "volume-uid", "nodeName": "node-a"}}
	unrelated := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "shiftpv-system"}}
	client := fake.NewClientset(reservation, unrelated)
	checker := &Checker{Client: client, Volumes: &memoryRepository{}, Cleanups: cleanupRepository{}, StorageClassName: "shiftpv", Namespace: "shiftpv-system"}

	report, err := checker.Check(context.Background())
	if err != nil || len(report.Blockers) != 1 || report.Blockers[0].Kind != "VolumeReservation" || report.Blockers[0].Name != reservation.Name {
		t.Fatalf("reservation blockers=%#v err=%v", report.Blockers, err)
	}
	if err := client.CoreV1().ConfigMaps(reservation.Namespace).Delete(context.Background(), reservation.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	report, err = checker.Check(context.Background())
	if err != nil || !report.Safe() {
		t.Fatalf("released reservation remained blocked: %#v err=%v", report.Blockers, err)
	}
}

func TestCheckBlocksUnfinishedCompletionWithoutVolumeLock(t *testing.T) {
	checker := &Checker{Client: fake.NewClientset(), StorageClassName: "shiftpv", Namespace: "shiftpv-system", Cleanups: cleanupRepository{}, Volumes: &memoryRepository{
		moves: []volumeapi.Move{{Name: "finishing", Spec: volumeapi.MoveSpec{VolumeID: "volume"}, Status: volumeapi.MoveStatus{Phase: "Completing"}}},
	}}
	report, err := checker.Check(context.Background())
	if err != nil || report.Safe() || len(report.Blockers) != 1 || report.Blockers[0].Kind != "ShiftPVMove" {
		t.Fatalf("unfinished completion did not block uninstall: %+v, %v", report, err)
	}
}

func TestCheckReportsEveryShiftPVDependency(t *testing.T) {
	storageClassName := "shiftpv"
	client := fake.NewClientset(
		&corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-data"}, Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: DriverName, VolumeHandle: "shiftpv-a"}},
			ClaimRef:               &corev1.ObjectReference{Namespace: "app", Name: "data"},
		}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "app"}, Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &storageClassName, VolumeName: "pv-data"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "shiftpv-a", Namespace: "shiftpv-system", Labels: map[string]string{
			"app.kubernetes.io/name": "shiftpv", "app.kubernetes.io/component": "volume-reservation",
		}}, Data: map[string]string{"volumeID": "shiftpv-a", "volumeUID": "volume-uid", "nodeName": "node-a"}},
		&corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-other"}, Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "other.csi.example", VolumeHandle: "other"}},
		}},
	)
	repository := &memoryRepository{
		volumes: map[string]volumeapi.State{"shiftpv-a": {Phase: volumeapi.PhaseMoving, OwnerNode: "node-a", ActiveMove: "move-a", PublishedNodes: []string{"node-a"}}},
		moves: []volumeapi.Move{
			{Name: "move-a", Spec: volumeapi.MoveSpec{VolumeID: "shiftpv-a"}, Status: volumeapi.MoveStatus{Phase: "Copying"}},
			{Name: "move-complete", Spec: volumeapi.MoveSpec{VolumeID: "shiftpv-b"}, Status: volumeapi.MoveStatus{Phase: "Succeeded"}},
			{Name: "move-blocked", Spec: volumeapi.MoveSpec{VolumeID: "shiftpv-c"}, Status: volumeapi.MoveStatus{Phase: "Blocked"}},
		},
	}

	report, err := (&Checker{Client: client, Volumes: repository, Cleanups: cleanupRepository{}, StorageClassName: storageClassName, Namespace: "shiftpv-system"}).Check(context.Background())
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if report.Safe() {
		t.Fatal("Check() unexpectedly allowed uninstall")
	}
	if len(report.Blockers) != 5 {
		t.Fatalf("len(blockers) = %d, want 5: %#v", len(report.Blockers), report.Blockers)
	}
	joined := blockersText(report.Blockers)
	for _, expected := range []string{
		"PersistentVolume//pv-data/driver=csi.shiftpv.io volumeHandle=shiftpv-a claim=app/data",
		"PersistentVolumeClaim/app/data/references the ShiftPV StorageClass volume=pv-data",
		"ShiftPVMove//move-a/phase=Copying volume=shiftpv-a",
		"ShiftPVVolume//shiftpv-a/phase=Moving owner=node-a activeMove=move-a publishedNodes=node-a",
		"VolumeReservation/shiftpv-system/shiftpv-a/volume=shiftpv-a volumeUID=volume-uid node=node-a",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("blockers do not contain %q: %s", expected, joined)
		}
	}
	if strings.Contains(joined, "pv-other") || strings.Contains(joined, "move-complete") || strings.Contains(joined, "move-blocked") {
		t.Fatalf("blockers include unrelated or terminal resources: %s", joined)
	}
}

func TestCheckFailsClosedOnAPIError(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("list", "persistentvolumes", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("api unavailable")
	})
	checker := &Checker{Client: client, Volumes: &memoryRepository{}, Cleanups: cleanupRepository{}, StorageClassName: "shiftpv", Namespace: "shiftpv-system"}
	_, err := checker.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "api unavailable") {
		t.Fatalf("Check() error = %v", err)
	}
}

func TestCheckFailsClosedWhenReservationsCannotBeListed(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("list", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("reservation API unavailable")
	})
	checker := &Checker{Client: client, Volumes: &memoryRepository{}, Cleanups: cleanupRepository{}, StorageClassName: "shiftpv", Namespace: "shiftpv-system"}
	_, err := checker.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "list ShiftPV volume reservations") {
		t.Fatalf("Check() error = %v", err)
	}
}

func TestCheckFailsClosedOnRepositoryError(t *testing.T) {
	checker := &Checker{Client: fake.NewClientset(), Volumes: &memoryRepository{volumesErr: errors.New("crd unavailable")}, Cleanups: cleanupRepository{}, StorageClassName: "shiftpv", Namespace: "shiftpv-system"}
	_, err := checker.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "crd unavailable") {
		t.Fatalf("Check() error = %v", err)
	}
}

func TestCheckBlocksPhysicalPoolCopiesAndUncertainInventory(t *testing.T) {
	now := time.Date(2026, time.September, 12, 5, 0, 0, 0, time.UTC)
	identity := volume.CopyIdentity{
		InstallationID: "installation", PoolName: "pool-a", PoolUID: "pool-uid", VolumeID: "shiftpv-0123456789abcdef0123456789abcdef",
		VolumeUID: "volume-uid", CopyID: "copy-id", NodeName: "node-a", Role: volume.RoleServing,
	}
	repository := &memoryRepository{pools: []volumeapi.Pool{{
		Name: "pool-a", UID: "pool-uid", NodeName: "node-a", MountPath: "/var/lib/shiftpv", Generation: 2,
		Status: volumeapi.PoolStatus{ObservedGeneration: 1, Inventory: &volumeapi.PoolInventory{
			ObservedAt: metav1.NewTime(now.Add(-4 * time.Minute)), Valid: false, Truncated: true, Message: "CopyObservationProblem",
			Copies: []volumeapi.CopyObservation{{Marker: "copy-copy-id.json", Identity: &identity, Present: true, Published: true}},
		}},
	}}}
	checker := &Checker{
		Client: fake.NewClientset(), Volumes: repository, Cleanups: cleanupRepository{}, StorageClassName: "shiftpv", Namespace: "shiftpv-system",
		Now: func() time.Time { return now },
	}

	report, err := checker.Check(context.Background())
	if err != nil || report.Safe() || len(report.Blockers) != 2 {
		t.Fatalf("unsafe Pool inventory report=%#v err=%v", report, err)
	}
	joined := blockersText(report.Blockers)
	for _, expected := range []string{
		"ShiftPVPoolInventory//pool-a/generation=2 observedGeneration=1 valid=false truncated=true message=CopyObservationProblem observedAt=stale",
		"ShiftPVPoolCopy//pool-a/marker=copy-copy-id.json present=true role=Serving volume=shiftpv-0123456789abcdef0123456789abcdef copy=copy-id published=true",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("Pool inventory blockers do not contain %q: %s", expected, joined)
		}
	}
}

func TestCheckRequiresEmptyInventoryObservedAfterQuiesce(t *testing.T) {
	now := time.Date(2026, time.September, 12, 5, 0, 0, 0, time.UTC)
	pool := volumeapi.Pool{
		Name: "pool-a", UID: "pool-uid", NodeName: "node-a", MountPath: "/var/lib/shiftpv", Generation: 1,
		Status: volumeapi.PoolStatus{ObservedGeneration: 1, Inventory: &volumeapi.PoolInventory{ObservedAt: metav1.NewTime(now.Add(-time.Second)), Valid: true}},
	}
	repository := &memoryRepository{pools: []volumeapi.Pool{pool}}
	checker := &Checker{
		Client: fake.NewClientset(), Volumes: repository, Cleanups: cleanupRepository{}, StorageClassName: "shiftpv", Namespace: "shiftpv-system",
		Now: func() time.Time { return now },
	}

	report, err := checker.CheckAfter(context.Background(), now)
	if err != nil || report.Safe() || !report.WaitingForInventory() {
		t.Fatalf("pre-quiesce inventory report=%#v err=%v", report, err)
	}
	repository.pools[0].Status.Inventory.ObservedAt = metav1.NewTime(now.Add(time.Second))
	checker.Now = func() time.Time { return now.Add(2 * time.Second) }
	report, err = checker.CheckAfter(context.Background(), now)
	if err != nil || !report.Safe() || report.WaitingForInventory() {
		t.Fatalf("post-quiesce empty inventory report=%#v err=%v", report, err)
	}

	repository.poolsErr = errors.New("Pool API unavailable")
	if _, err := checker.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "Pool API unavailable") {
		t.Fatalf("Pool API failure did not close uninstall gate: %v", err)
	}
}

func TestCheckValidatesConfiguration(t *testing.T) {
	_, err := (&Checker{}).Check(context.Background())
	if err == nil {
		t.Fatal("Check() unexpectedly accepted an empty configuration")
	}

	checker := &Checker{Client: fake.NewClientset(), Volumes: &memoryRepository{}, Cleanups: cleanupRepository{}, Namespace: "shiftpv-system"}
	_, err = checker.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "StorageClass name") {
		t.Fatalf("Check() empty StorageClass error = %v", err)
	}

	checker.StorageClassName = "shiftpv"
	checker.Namespace = ""
	_, err = checker.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "namespace") {
		t.Fatalf("Check() empty namespace error = %v", err)
	}
}

func blockersText(blockers []Blocker) string {
	var lines []string
	for _, blocker := range blockers {
		lines = append(lines, blocker.Kind+"/"+blocker.Namespace+"/"+blocker.Name+"/"+blocker.Reason)
	}
	return strings.Join(lines, "\n")
}
