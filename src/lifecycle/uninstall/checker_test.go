package uninstall

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
)

type memoryRepository struct {
	volumes    map[string]volumeapi.State
	moves      []volumeapi.Move
	volumesErr error
	movesErr   error
}

func (m *memoryRepository) ListVolumes(context.Context) (map[string]volumeapi.State, error) {
	return m.volumes, m.volumesErr
}

func (m *memoryRepository) ListMoves(context.Context) ([]volumeapi.Move, error) {
	return m.moves, m.movesErr
}

func TestCheckAllowsEmptyCluster(t *testing.T) {
	checker := &Checker{Client: fake.NewClientset(), Volumes: &memoryRepository{}, StorageClassName: "shiftpv"}
	report, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !report.Safe() {
		t.Fatalf("Check() blockers = %#v", report.Blockers)
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

	report, err := (&Checker{Client: client, Volumes: repository, StorageClassName: storageClassName}).Check(context.Background())
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if report.Safe() {
		t.Fatal("Check() unexpectedly allowed uninstall")
	}
	if len(report.Blockers) != 4 {
		t.Fatalf("len(blockers) = %d, want 4: %#v", len(report.Blockers), report.Blockers)
	}
	joined := blockersText(report.Blockers)
	for _, expected := range []string{
		"PersistentVolume//pv-data/driver=csi.shiftpv.io volumeHandle=shiftpv-a claim=app/data",
		"PersistentVolumeClaim/app/data/references the ShiftPV StorageClass volume=pv-data",
		"ShiftPVMove//move-a/phase=Copying volume=shiftpv-a",
		"ShiftPVVolume//shiftpv-a/phase=Moving owner=node-a activeMove=move-a publishedNodes=node-a",
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
	checker := &Checker{Client: client, Volumes: &memoryRepository{}, StorageClassName: "shiftpv"}
	_, err := checker.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "api unavailable") {
		t.Fatalf("Check() error = %v", err)
	}
}

func TestCheckFailsClosedOnRepositoryError(t *testing.T) {
	checker := &Checker{Client: fake.NewClientset(), Volumes: &memoryRepository{volumesErr: errors.New("crd unavailable")}, StorageClassName: "shiftpv"}
	_, err := checker.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "crd unavailable") {
		t.Fatalf("Check() error = %v", err)
	}
}

func TestCheckValidatesConfiguration(t *testing.T) {
	_, err := (&Checker{}).Check(context.Background())
	if err == nil {
		t.Fatal("Check() unexpectedly accepted an empty configuration")
	}

	checker := &Checker{Client: fake.NewClientset(), Volumes: &memoryRepository{}}
	_, err = checker.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "StorageClass name") {
		t.Fatalf("Check() empty StorageClass error = %v", err)
	}
}

func blockersText(blockers []Blocker) string {
	var lines []string
	for _, blocker := range blockers {
		lines = append(lines, blocker.Kind+"/"+blocker.Namespace+"/"+blocker.Name+"/"+blocker.Reason)
	}
	return strings.Join(lines, "\n")
}
