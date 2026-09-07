package controller

import (
	"context"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	poolcapacity "github.com/cagojeiger/ShiftPV/src/pool/capacity"
)

type fakeMoveCapacityProbe struct {
	usage int64
	stats poolcapacity.Filesystem
}

func (f fakeMoveCapacityProbe) StatFS(context.Context, string) (poolcapacity.Filesystem, error) {
	return f.stats, nil
}

func (f fakeMoveCapacityProbe) VolumeUsage(context.Context, string, string) (int64, error) {
	return f.usage, nil
}

func TestEnsureCapacityApprovesOrBlocksBeforeCopy(t *testing.T) {
	for name, test := range map[string]struct {
		limit     string
		usage     int64
		available int64
		want      string
	}{
		"approved":            {limit: "128Mi", usage: 32 << 20, available: 64 << 20},
		"logical reservation": {limit: "16Mi", usage: 8 << 20, available: 64 << 20, want: "DestinationReservationLimit"},
		"physical space":      {limit: "128Mi", usage: 80 << 20, available: 64 << 20, want: "DestinationFilesystemSpace"},
	} {
		t.Run(name, func(t *testing.T) {
			volumeID := "shiftpv-0123456789abcdef0123456789abcdef"
			move := volumeapi.Move{Name: "move-test", Spec: volumeapi.MoveSpec{VolumeID: volumeID, SourceNode: "source"}}
			repository := &memoryRepository{
				volumes: map[string]volumeapi.State{volumeID: {Phase: volumeapi.PhaseMoving, OwnerNode: "source", ActiveMove: move.Name}},
				pools: []volumeapi.Pool{
					{Name: "source", NodeName: "source", MountPath: "/source", CapacityLimit: "128Mi"},
					{Name: "destination", NodeName: "destination", MountPath: "/destination", CapacityLimit: test.limit},
				},
				moves: []volumeapi.Move{move},
			}
			client := fake.NewSimpleClientset(moveReservation(volumeID, "source", 32<<20))
			reconciler := &Reconciler{
				Client: client, Repository: repository, Namespace: "system", HelperImage: "helper",
				CapacityProbe: fakeMoveCapacityProbe{usage: test.usage, stats: poolcapacity.Filesystem{AvailableBytes: test.available}},
				PoolLocks:     &poolcapacity.Locker{},
			}
			if err := reconciler.ensureCapacity(context.Background(), &move, observation{DestinationNode: "destination"}); err != nil {
				t.Fatal(err)
			}
			if move.Status.CapacityApproved != (test.want == "") || move.Status.CapacityReason != test.want || move.Status.SourceBytes != test.usage {
				t.Fatalf("capacity status = %+v", move.Status)
			}
			if _, err := client.BatchV1().Jobs("system").Get(context.Background(), namesFor(move.Name).CopyJob, metav1.GetOptions{}); err == nil {
				t.Fatal("capacity admission created a copy Job")
			}
		})
	}
}

func TestDestinationCapacityDoesNotCountRecoveredMove(t *testing.T) {
	currentID := "shiftpv-0123456789abcdef0123456789abcdef"
	recoveredID := "shiftpv-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	current := volumeapi.Move{Name: "move-current", Spec: volumeapi.MoveSpec{VolumeID: currentID, SourceNode: "source"}}
	recovered := volumeapi.Move{
		Name: "move-recovered", Spec: volumeapi.MoveSpec{VolumeID: recoveredID, SourceNode: "source"},
		Status: volumeapi.MoveStatus{
			Phase: "Blocked", DestinationNode: "destination", CapacityApproved: true,
			SourceBytes: 32 << 20, RecoveryPhase: "Recovered",
		},
	}
	repository := &memoryRepository{
		volumes: map[string]volumeapi.State{
			currentID:   {Phase: volumeapi.PhaseMoving, OwnerNode: "source", ActiveMove: current.Name},
			recoveredID: {Phase: volumeapi.PhaseReady, OwnerNode: "source"},
		},
		pools: []volumeapi.Pool{
			{Name: "source", NodeName: "source", MountPath: "/source", CapacityLimit: "64Mi"},
			{Name: "destination", NodeName: "destination", MountPath: "/destination", CapacityLimit: "64Mi"},
		},
		moves: []volumeapi.Move{current, recovered},
	}
	client := fake.NewSimpleClientset(
		moveReservation(currentID, "source", 32<<20),
		moveReservation(recoveredID, "source", 32<<20),
	)
	reconciler := &Reconciler{Client: client, Repository: repository, Namespace: "system"}
	requested, logicalReserved, physicalPending, limit, err := reconciler.destinationCapacity(context.Background(), current, "destination")
	if err != nil {
		t.Fatal(err)
	}
	if requested != 32<<20 || logicalReserved != 0 || physicalPending != 0 || limit != 64<<20 {
		t.Fatalf("capacity = requested=%d logical=%d physical=%d limit=%d", requested, logicalReserved, physicalPending, limit)
	}
}

func TestDestinationCapacityIgnoresMoveAfterVolumeAndReservationDeletion(t *testing.T) {
	currentID := "shiftpv-0123456789abcdef0123456789abcdef"
	current := volumeapi.Move{Name: "move-current", Spec: volumeapi.MoveSpec{VolumeID: currentID, SourceNode: "source"}}
	deleted := volumeapi.Move{
		Name: "move-deleted", Spec: volumeapi.MoveSpec{VolumeID: "shiftpv-deleted", SourceNode: "source"},
		Status: volumeapi.MoveStatus{
			Phase: "Succeeded", DestinationNode: "destination", CapacityApproved: true, SourceBytes: 32 << 20,
		},
	}
	repository := &memoryRepository{
		volumes: map[string]volumeapi.State{
			currentID: {Phase: volumeapi.PhaseMoving, OwnerNode: "source", ActiveMove: current.Name},
		},
		pools: []volumeapi.Pool{
			{Name: "source", NodeName: "source", MountPath: "/source", CapacityLimit: "64Mi"},
			{Name: "destination", NodeName: "destination", MountPath: "/destination", CapacityLimit: "64Mi"},
		},
		moves: []volumeapi.Move{current, deleted},
	}
	reconciler := &Reconciler{
		Client:     fake.NewSimpleClientset(moveReservation(currentID, "source", 32<<20)),
		Repository: repository, Namespace: "system",
	}
	requested, logicalReserved, physicalPending, _, err := reconciler.destinationCapacity(context.Background(), current, "destination")
	if err != nil {
		t.Fatalf("deleted volume's Move blocked destination admission: %v", err)
	}
	if requested != 32<<20 || logicalReserved != 0 || physicalPending != 0 {
		t.Fatalf("capacity = requested=%d logical=%d physical=%d", requested, logicalReserved, physicalPending)
	}
}

func TestDestinationCapacityRejectsMoveWithReservationButNoVolume(t *testing.T) {
	currentID := "shiftpv-0123456789abcdef0123456789abcdef"
	orphanID := "shiftpv-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	current := volumeapi.Move{Name: "move-current", Spec: volumeapi.MoveSpec{VolumeID: currentID, SourceNode: "source"}}
	incomplete := volumeapi.Move{
		Name: "move-incomplete", Spec: volumeapi.MoveSpec{VolumeID: orphanID, SourceNode: "source"},
		Status: volumeapi.MoveStatus{DestinationNode: "destination", CapacityApproved: true, SourceBytes: 1},
	}
	repository := &memoryRepository{
		volumes: map[string]volumeapi.State{
			currentID: {Phase: volumeapi.PhaseMoving, OwnerNode: "source", ActiveMove: current.Name},
		},
		pools: []volumeapi.Pool{
			{Name: "source", NodeName: "source", MountPath: "/source", CapacityLimit: "64Mi"},
			{Name: "destination", NodeName: "destination", MountPath: "/destination", CapacityLimit: "64Mi"},
		},
		moves: []volumeapi.Move{current, incomplete},
	}
	reconciler := &Reconciler{
		Client: fake.NewSimpleClientset(
			moveReservation(currentID, "source", 8<<20),
			moveReservation(orphanID, "source", 8<<20),
		),
		Repository: repository, Namespace: "system",
	}
	_, _, _, _, err := reconciler.destinationCapacity(context.Background(), current, "destination")
	if err == nil || !strings.Contains(err.Error(), "has no volume state") {
		t.Fatalf("incomplete move was not rejected: %v", err)
	}
}

func TestEnsureCapacityFailsClosedWhenDestinationPoolLimitIsMissing(t *testing.T) {
	volumeID := "shiftpv-0123456789abcdef0123456789abcdef"
	move := volumeapi.Move{Name: "move-test", Spec: volumeapi.MoveSpec{VolumeID: volumeID, SourceNode: "source"}}
	repository := &memoryRepository{
		volumes: map[string]volumeapi.State{
			volumeID: {Phase: volumeapi.PhaseMoving, OwnerNode: "source", ActiveMove: move.Name},
		},
		pools: []volumeapi.Pool{
			{Name: "source", NodeName: "source", MountPath: "/source", CapacityLimit: "64Mi"},
			{Name: "destination", NodeName: "destination", MountPath: "/destination"},
		},
		moves: []volumeapi.Move{move},
	}
	reconciler := &Reconciler{
		Client:     fake.NewSimpleClientset(moveReservation(volumeID, "source", 32<<20)),
		Repository: repository, Namespace: "system",
		CapacityProbe: fakeMoveCapacityProbe{usage: 1, stats: poolcapacity.Filesystem{AvailableBytes: 1 << 30}},
		PoolLocks:     &poolcapacity.Locker{},
	}
	err := reconciler.ensureCapacity(context.Background(), &move, observation{DestinationNode: "destination"})
	if err == nil || !strings.Contains(err.Error(), "capacity limit") {
		t.Fatalf("missing Pool limit did not fail closed: %v", err)
	}
	if move.Status.CapacityApproved {
		t.Fatal("missing Pool limit approved destination capacity")
	}
}

func moveReservation(volumeID, node string, bytes int64) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: volumeID, Namespace: "system", Labels: map[string]string{
			"app.kubernetes.io/name": "shiftpv", "app.kubernetes.io/component": "volume-reservation",
		}},
		Data: map[string]string{"volumeID": volumeID, "nodeName": node, "capacity": strconv.FormatInt(bytes, 10)},
	}
}
