package controller

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	poolcapacity "github.com/cagojeiger/ShiftPV/src/pool/capacity"
	"github.com/cagojeiger/ShiftPV/src/volume"
)

type fakePoolCapacityRegistry struct {
	pool    volumeapi.Pool
	volumes map[string]volumeapi.State
	moves   []volumeapi.Move
	err     error
}

func (f *fakePoolCapacityRegistry) PoolForNode(context.Context, string) (volumeapi.Pool, error) {
	return f.pool, f.err
}

func (f *fakePoolCapacityRegistry) ListVolumes(context.Context) (map[string]volumeapi.State, error) {
	return f.volumes, f.err
}

func (f *fakePoolCapacityRegistry) ListMoves(context.Context) ([]volumeapi.Move, error) {
	return f.moves, f.err
}

type fakePoolCapacityProbe struct {
	mu    sync.Mutex
	stats poolcapacity.Filesystem
	err   error
	calls int
}

func (f *fakePoolCapacityProbe) StatFS(context.Context, string) (poolcapacity.Filesystem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.stats, f.err
}

func (f *fakePoolCapacityProbe) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestCreateVolumeUsesPoolLimitAndFilesystemCapacity(t *testing.T) {
	for name, test := range map[string]struct {
		limit     string
		available int64
		wantCode  codes.Code
	}{
		"accepted":             {limit: "128Mi", available: 128 << 20, wantCode: codes.OK},
		"logical limit":        {limit: "32Mi", available: 128 << 20, wantCode: codes.ResourceExhausted},
		"filesystem available": {limit: "128Mi", available: 32 << 20, wantCode: codes.ResourceExhausted},
		"missing limit":        {available: 128 << 20, wantCode: codes.FailedPrecondition},
		"invalid limit":        {limit: "not-a-quantity", available: 128 << 20, wantCode: codes.FailedPrecondition},
	} {
		t.Run(name, func(t *testing.T) {
			probe := &fakePoolCapacityProbe{stats: poolcapacity.Filesystem{AvailableBytes: test.available}}
			service := capacityService(fake.NewClientset(), test.limit, nil, probe)
			_, err := service.CreateVolume(context.Background(), validCreateRequest("worker-a"))
			if got := status.Code(err); got != test.wantCode {
				t.Fatalf("code = %s, want %s: %v", got, test.wantCode, err)
			}
		})
	}
}

func TestCreateVolumeCountsReservationsAtCurrentVolumeOwner(t *testing.T) {
	existingID := "shiftpv-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	client := fake.NewClientset(capacityReservation(existingID, "worker-a", 64<<20))
	registry := &fakePoolCapacityRegistry{
		pool: volumeapi.Pool{Name: "pool-b", NodeName: "worker-b", MountPath: "/pool", CapacityLimit: "64Mi"},
		volumes: map[string]volumeapi.State{
			existingID: {Phase: volumeapi.PhaseReady, OwnerNode: "worker-b"},
		},
	}
	service := &Service{
		Client: client, Namespace: "shiftpv-system", Operator: &fakeDirectoryOperator{},
		CapacityPools: registry,
		CapacityProbe: &fakePoolCapacityProbe{stats: poolcapacity.Filesystem{AvailableBytes: 1 << 30}},
	}
	req := validCreateRequest("worker-b")
	req.Name = "new-pvc"
	_, err := service.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("moved reservation was not charged to current owner: %v", err)
	}
}

func TestCreateVolumeCountsCapacityApprovedMoveAtDestination(t *testing.T) {
	existingID := "shiftpv-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	client := fake.NewClientset(capacityReservation(existingID, "worker-a", 64<<20))
	registry := &fakePoolCapacityRegistry{
		pool: volumeapi.Pool{Name: "pool-b", NodeName: "worker-b", MountPath: "/pool", CapacityLimit: "64Mi"},
		volumes: map[string]volumeapi.State{
			existingID: {Phase: volumeapi.PhaseMoving, OwnerNode: "worker-a", ActiveMove: "move-a"},
		},
		moves: []volumeapi.Move{{
			Name: "move-a", Spec: volumeapi.MoveSpec{VolumeID: existingID, SourceNode: "worker-a"},
			Status: volumeapi.MoveStatus{DestinationNode: "worker-b", CapacityApproved: true, SourceBytes: 1},
		}},
	}
	service := &Service{
		Client: client, Namespace: "shiftpv-system", Operator: &fakeDirectoryOperator{},
		CapacityPools: registry,
		CapacityProbe: &fakePoolCapacityProbe{stats: poolcapacity.Filesystem{AvailableBytes: 1 << 30}},
	}
	_, err := service.CreateVolume(context.Background(), validCreateRequest("worker-b"))
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("approved move was not charged to destination: %v", err)
	}
}

func TestCreateVolumeDoesNotCountRecoveredMoveAtDestination(t *testing.T) {
	existingID := "shiftpv-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	client := fake.NewClientset(capacityReservation(existingID, "worker-a", 64<<20))
	registry := &fakePoolCapacityRegistry{
		pool: volumeapi.Pool{Name: "pool-b", NodeName: "worker-b", MountPath: "/pool", CapacityLimit: "64Mi"},
		volumes: map[string]volumeapi.State{
			existingID: {Phase: volumeapi.PhaseReady, OwnerNode: "worker-a"},
		},
		moves: []volumeapi.Move{{
			Name: "move-a", Spec: volumeapi.MoveSpec{VolumeID: existingID, SourceNode: "worker-a"},
			Status: volumeapi.MoveStatus{
				Phase: "Blocked", DestinationNode: "worker-b", CapacityApproved: true,
				SourceBytes: 1, RecoveryPhase: "Recovered",
			},
		}},
	}
	service := &Service{
		Client: client, Namespace: "shiftpv-system", Operator: &fakeDirectoryOperator{},
		CapacityPools: registry,
		CapacityProbe: &fakePoolCapacityProbe{stats: poolcapacity.Filesystem{AvailableBytes: 1 << 30}},
	}
	if _, err := service.CreateVolume(context.Background(), validCreateRequest("worker-b")); err != nil {
		t.Fatalf("recovered move retained destination capacity: %v", err)
	}
}

func TestCreateVolumeSerializesPoolReservationAdmission(t *testing.T) {
	probe := &fakePoolCapacityProbe{stats: poolcapacity.Filesystem{AvailableBytes: 1 << 30}}
	service := capacityService(fake.NewClientset(), "64Mi", nil, probe)
	requests := []string{"pvc-a", "pvc-b"}
	results := make(chan codes.Code, len(requests))
	for _, name := range requests {
		name := name
		go func() {
			req := validCreateRequest("worker-a")
			req.Name = name
			_, err := service.CreateVolume(context.Background(), req)
			results <- status.Code(err)
		}()
	}
	counts := map[codes.Code]int{}
	for range requests {
		counts[<-results]++
	}
	if counts[codes.OK] != 1 || counts[codes.ResourceExhausted] != 1 {
		t.Fatalf("result codes = %v", counts)
	}
}

func TestCreateVolumeReusesExistingReservationWithoutNewProbe(t *testing.T) {
	req := validCreateRequest("worker-a")
	id, idErr := volume.IDFromName(req.Name)
	if idErr != nil {
		t.Fatal(idErr)
	}
	client := fake.NewClientset(reservationForCreateRequest(id, req))
	probe := &fakePoolCapacityProbe{stats: poolcapacity.Filesystem{AvailableBytes: 0}}
	service := capacityService(client, "1Mi", nil, probe)

	response, err := service.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if response.GetVolume().GetVolumeId() != id || probe.callCount() != 0 {
		t.Fatalf("response=%#v probe calls=%d", response, probe.callCount())
	}
}

func TestCreateVolumeFailsClosedOnCapacityProbeError(t *testing.T) {
	probe := &fakePoolCapacityProbe{err: retryableDirectoryError{errors.New("statfs unavailable")}}
	client := fake.NewClientset()
	service := capacityService(client, "1Gi", nil, probe)

	_, err := service.CreateVolume(context.Background(), validCreateRequest("worker-a"))
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code = %s, want Unavailable: %v", status.Code(err), err)
	}
	reservations, listErr := client.CoreV1().ConfigMaps("shiftpv-system").List(context.Background(), metav1.ListOptions{})
	if listErr != nil || len(reservations.Items) != 0 {
		t.Fatalf("probe failure created reservations: items=%d err=%v", len(reservations.Items), listErr)
	}
}

func TestCapacityProbeErrorPreservesContextCode(t *testing.T) {
	for name, test := range map[string]struct {
		err      error
		wantCode codes.Code
	}{
		"canceled": {err: context.Canceled, wantCode: codes.Canceled},
		"deadline": {err: context.DeadlineExceeded, wantCode: codes.DeadlineExceeded},
	} {
		t.Run(name, func(t *testing.T) {
			if got := status.Code(capacityProbeError("probe", test.err)); got != test.wantCode {
				t.Fatalf("code = %s, want %s", got, test.wantCode)
			}
		})
	}
}

func TestCreateVolumeReportsInvalidPoolAsFailedPrecondition(t *testing.T) {
	registry := &fakePoolCapacityRegistry{err: volumeapi.ErrPoolConfiguration}
	service := &Service{
		Client: fake.NewClientset(), Namespace: "shiftpv-system", Operator: &fakeDirectoryOperator{},
		CapacityPools: registry,
		CapacityProbe: &fakePoolCapacityProbe{stats: poolcapacity.Filesystem{AvailableBytes: 1 << 30}},
	}
	_, err := service.CreateVolume(context.Background(), validCreateRequest("worker-a"))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %s, want FailedPrecondition: %v", status.Code(err), err)
	}
}

func capacityService(client *fake.Clientset, limit string, volumes map[string]volumeapi.State, probe PoolCapacityProbe) *Service {
	registry := &fakePoolCapacityRegistry{
		pool:    volumeapi.Pool{Name: "pool-a", NodeName: "worker-a", MountPath: "/pool", CapacityLimit: limit},
		volumes: volumes,
	}
	return &Service{
		Client: client, Namespace: "shiftpv-system", Operator: &fakeDirectoryOperator{},
		CapacityPools: registry, CapacityProbe: probe,
	}
}

func capacityReservation(id, node string, capacityBytes int64) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: id, Namespace: "shiftpv-system",
			Labels: map[string]string{
				"app.kubernetes.io/name":      "shiftpv",
				"app.kubernetes.io/component": "volume-reservation",
			},
		},
		Data: map[string]string{
			"requestName": "existing", "volumeID": id, "nodeName": node,
			"capacity": strconv.FormatInt(capacityBytes, 10),
		},
	}
}
