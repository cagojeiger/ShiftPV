package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/cagojeiger/ShiftPV/src/volume"
)

type fakeDirectoryOperator struct {
	createdNode string
	createdID   string
	deletedNode string
	deletedID   string
	createCalls int
	deleteCalls int
	createErr   error
	deleteErr   error
}

type retryableDirectoryError struct{ error }

func (retryableDirectoryError) Retryable() bool { return true }

func (f *fakeDirectoryOperator) Create(_ context.Context, node, id string) error {
	f.createCalls++
	f.createdNode = node
	f.createdID = id
	return f.createErr
}

func (f *fakeDirectoryOperator) Delete(_ context.Context, node, id string) error {
	f.deleteCalls++
	f.deletedNode = node
	f.deletedID = id
	return f.deleteErr
}

func TestCreateVolumeIsIdempotent(t *testing.T) {
	operator := &fakeDirectoryOperator{}
	service := &Service{Client: fake.NewClientset(), Namespace: "shiftpv-system", Operator: operator}
	req := validCreateRequest("worker-a")

	first, err := service.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Volume.VolumeId != second.Volume.VolumeId {
		t.Fatalf("volume ID changed: %q != %q", first.Volume.VolumeId, second.Volume.VolumeId)
	}
	if operator.createdNode != "worker-a" || operator.createdID != first.Volume.VolumeId {
		t.Fatalf("unexpected directory operation: node=%q id=%q", operator.createdNode, operator.createdID)
	}
}

func TestCreateVolumeRejectsChangedSelectedNode(t *testing.T) {
	service := &Service{Client: fake.NewClientset(), Namespace: "shiftpv-system", Operator: &fakeDirectoryOperator{}}
	if _, err := service.CreateVolume(context.Background(), validCreateRequest("worker-a")); err != nil {
		t.Fatal(err)
	}
	_, err := service.CreateVolume(context.Background(), validCreateRequest("worker-b"))
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("expected AlreadyExists, got %v", err)
	}
}

func TestCreateVolumeRequiresNoCapacityEnforcement(t *testing.T) {
	service := &Service{Client: fake.NewClientset(), Namespace: "shiftpv-system", Operator: &fakeDirectoryOperator{}}
	req := validCreateRequest("worker-a")
	delete(req.Parameters, CapacityEnforcementKey)
	_, err := service.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestCreateVolumeRejectsInvalidRequests(t *testing.T) {
	tests := map[string]func(*csi.CreateVolumeRequest){
		"missing name":     func(req *csi.CreateVolumeRequest) { req.Name = "" },
		"missing capacity": func(req *csi.CreateVolumeRequest) { req.CapacityRange = nil },
		"zero capacity": func(req *csi.CreateVolumeRequest) {
			req.CapacityRange.RequiredBytes = 0
		},
		"capacity above limit": func(req *csi.CreateVolumeRequest) {
			req.CapacityRange.LimitBytes = req.CapacityRange.RequiredBytes - 1
		},
		"missing capabilities": func(req *csi.CreateVolumeRequest) {
			req.VolumeCapabilities = nil
		},
		"raw block": func(req *csi.CreateVolumeRequest) {
			req.VolumeCapabilities[0].AccessType = &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}}
		},
		"unsupported access mode": func(req *csi.CreateVolumeRequest) {
			req.VolumeCapabilities[0].AccessMode.Mode = csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
		},
		"missing topology": func(req *csi.CreateVolumeRequest) {
			req.AccessibilityRequirements = nil
		},
		"topology without ShiftPV key": func(req *csi.CreateVolumeRequest) {
			req.AccessibilityRequirements.Preferred[0].Segments = map[string]string{"other": "worker-a"}
		},
		"unknown parameter": func(req *csi.CreateVolumeRequest) {
			req.Parameters["unknown"] = "value"
		},
		"unsupported enforcement": func(req *csi.CreateVolumeRequest) {
			req.Parameters[CapacityEnforcementKey] = "hard"
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			req := validCreateRequest("worker-a")
			mutate(req)
			service := &Service{Client: fake.NewClientset(), Namespace: "shiftpv-system", Operator: &fakeDirectoryOperator{}}
			_, err := service.CreateVolume(context.Background(), req)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("expected InvalidArgument, got %v", err)
			}
		})
	}
}

func TestCreateVolumeRejectsUnconfiguredController(t *testing.T) {
	service := &Service{}
	_, err := service.CreateVolume(context.Background(), validCreateRequest("worker-a"))
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal, got %v", err)
	}
}

func TestCreateVolumeReportsDirectoryFailure(t *testing.T) {
	operator := &fakeDirectoryOperator{createErr: errors.New("mkdir failed")}
	client := fake.NewClientset()
	service := &Service{Client: client, Namespace: "shiftpv-system", Operator: operator}
	req := validCreateRequest("worker-a")

	_, err := service.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal, got %v", err)
	}
	id, idErr := volume.IDFromName(req.Name)
	if idErr != nil {
		t.Fatal(idErr)
	}
	if _, getErr := client.CoreV1().ConfigMaps("shiftpv-system").Get(context.Background(), id, metav1.GetOptions{}); getErr != nil {
		t.Fatalf("expected reservation to remain retryable: %v", getErr)
	}
}

func TestCreateVolumeMapsRetryableDirectoryFailureToUnavailable(t *testing.T) {
	operator := &fakeDirectoryOperator{createErr: retryableDirectoryError{errors.New("filesystem unavailable")}}
	service := &Service{Client: fake.NewClientset(), Namespace: "shiftpv-system", Operator: operator}

	_, err := service.CreateVolume(context.Background(), validCreateRequest("worker-a"))
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable, got %v", err)
	}
}

func TestCreateVolumeRetriesAfterAmbiguousReservationTimeout(t *testing.T) {
	client := fake.NewClientset()
	operator := &fakeDirectoryOperator{}
	service := &Service{Client: client, Namespace: "shiftpv-system", Operator: operator}
	req := validCreateRequest("worker-a")
	timedOut := false
	client.PrependReactor("create", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if timedOut {
			return false, nil, nil
		}
		timedOut = true
		create := action.(k8stesting.CreateAction)
		cm := create.GetObject().(*corev1.ConfigMap).DeepCopy()
		if err := client.Tracker().Create(action.GetResource(), cm, action.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewTimeoutError("reservation response timed out", 1)
	})

	if _, err := service.CreateVolume(context.Background(), req); status.Code(err) != codes.Unavailable {
		t.Fatalf("expected retryable Unavailable, got %v", err)
	}
	if operator.createCalls != 0 {
		t.Fatalf("directory operation ran after ambiguous reservation response: %d calls", operator.createCalls)
	}
	response, err := service.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if response.GetVolume().GetVolumeId() == "" || operator.createCalls != 1 {
		t.Fatalf("retry did not converge: response=%#v createCalls=%d", response, operator.createCalls)
	}
}

func TestCreateVolumePreservesDeadlineExceededCode(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("create", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})
	service := &Service{Client: client, Namespace: "shiftpv-system", Operator: &fakeDirectoryOperator{}}

	if _, err := service.CreateVolume(context.Background(), validCreateRequest("worker-a")); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
}

func TestKubernetesAPIErrorPreservesRetryAndContextCodes(t *testing.T) {
	tests := map[string]struct {
		err  error
		code codes.Code
	}{
		"canceled":          {err: context.Canceled, code: codes.Canceled},
		"deadline exceeded": {err: context.DeadlineExceeded, code: codes.DeadlineExceeded},
		"timeout":           {err: apierrors.NewTimeoutError("timed out", 1), code: codes.Unavailable},
		"throttled":         {err: apierrors.NewTooManyRequests("slow down", 1), code: codes.Unavailable},
		"unavailable":       {err: apierrors.NewServiceUnavailable("offline"), code: codes.Unavailable},
		"other":             {err: errors.New("invalid response"), code: codes.Internal},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := status.Code(kubernetesAPIError("call Kubernetes", test.err)); got != test.code {
				t.Fatalf("expected %s, got %s", test.code, got)
			}
		})
	}
}

func TestCreateWaitsForConcurrentDeleteOfSameVolume(t *testing.T) {
	req := validCreateRequest("worker-a")
	id, err := volume.IDFromName(req.Name)
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientset(reservationForCreateRequest(id, req))
	operator := newBlockingDeleteOperator()
	service := &Service{Client: client, Namespace: "shiftpv-system", Operator: operator}
	deleteDone := make(chan error, 1)
	go func() {
		_, deleteErr := service.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: id})
		deleteDone <- deleteErr
	}()
	<-operator.deleteStarted

	createDone := make(chan error, 1)
	createCalled := make(chan struct{})
	go func() {
		close(createCalled)
		_, createErr := service.CreateVolume(context.Background(), req)
		createDone <- createErr
	}()
	<-createCalled

	select {
	case <-operator.createStarted:
		close(operator.allowDelete)
		<-deleteDone
		<-createDone
		t.Fatal("CreateVolume reached the directory while DeleteVolume still owned the same volume lifecycle")
	case <-time.After(100 * time.Millisecond):
	}

	close(operator.allowDelete)
	if err := <-deleteDone; err != nil {
		t.Fatal(err)
	}
	if err := <-createDone; err != nil {
		t.Fatal(err)
	}
	if !operator.exists() {
		t.Fatal("serialized CreateVolume did not leave the directory present")
	}
	if _, err := client.CoreV1().ConfigMaps("shiftpv-system").Get(context.Background(), id, metav1.GetOptions{}); err != nil {
		t.Fatalf("serialized CreateVolume did not restore the reservation: %v", err)
	}
}

func TestCreateVolumesWithDifferentIDsRunConcurrently(t *testing.T) {
	client := fake.NewClientset()
	operator := newBlockingCreateOperator()
	service := &Service{Client: client, Namespace: "shiftpv-system", Operator: operator}
	requests := []*csi.CreateVolumeRequest{validCreateRequest("worker-a"), validCreateRequest("worker-b")}
	requests[0].Name = "pvc-a"
	requests[1].Name = "pvc-b"
	done := make(chan error, len(requests))
	for _, req := range requests {
		req := req
		go func() {
			_, err := service.CreateVolume(context.Background(), req)
			done <- err
		}()
	}

	for range requests {
		select {
		case <-operator.createStarted:
		case <-time.After(time.Second):
			close(operator.allowCreate)
			t.Fatal("different volume IDs were serialized")
		}
	}
	close(operator.allowCreate)
	for range requests {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestConcurrentCreatesForSameIDAreSerialized(t *testing.T) {
	client := fake.NewClientset()
	operator := newBlockingCreateOperator()
	service := &Service{Client: client, Namespace: "shiftpv-system", Operator: operator}
	req := validCreateRequest("worker-a")
	done := make(chan error, 2)
	go func() {
		_, err := service.CreateVolume(context.Background(), req)
		done <- err
	}()
	<-operator.createStarted
	secondCalled := make(chan struct{})
	go func() {
		close(secondCalled)
		_, err := service.CreateVolume(context.Background(), req)
		done <- err
	}()
	<-secondCalled

	secondEnteredEarly := false
	select {
	case <-operator.createStarted:
		secondEnteredEarly = true
	case <-time.After(100 * time.Millisecond):
	}
	close(operator.allowCreate)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if secondEnteredEarly {
		t.Fatal("concurrent CreateVolume calls entered the same volume lifecycle together")
	}
}

func TestConcurrentDeletesForSameIDRunDirectoryDeleteOnce(t *testing.T) {
	id, err := volume.IDFromName("pvc-uid")
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientset(reservation(id, "worker-a"))
	operator := newBlockingDeleteOperator()
	service := &Service{Client: client, Namespace: "shiftpv-system", Operator: operator}
	done := make(chan error, 2)
	deleteRequest := func() {
		_, deleteErr := service.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: id})
		done <- deleteErr
	}
	go deleteRequest()
	<-operator.deleteStarted
	secondCalled := make(chan struct{})
	go func() {
		close(secondCalled)
		deleteRequest()
	}()
	<-secondCalled

	secondEnteredEarly := false
	select {
	case <-operator.deleteStarted:
		secondEnteredEarly = true
	case <-time.After(100 * time.Millisecond):
	}
	close(operator.allowDelete)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if secondEnteredEarly {
		t.Fatal("concurrent DeleteVolume calls entered the same volume lifecycle together")
	}
	if operator.exists() {
		t.Fatal("directory remained after serialized deletion")
	}
}

func TestDeleteVolumeRemovesDirectoryAndReservation(t *testing.T) {
	id, err := volume.IDFromName("pvc-uid")
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientset(reservation(id, "worker-a"))
	operator := &fakeDirectoryOperator{}
	service := &Service{Client: client, Namespace: "shiftpv-system", Operator: operator}

	if _, err := service.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: id}); err != nil {
		t.Fatal(err)
	}
	if operator.deletedNode != "worker-a" || operator.deletedID != id {
		t.Fatalf("unexpected delete operation: node=%q id=%q", operator.deletedNode, operator.deletedID)
	}
	_, err = client.CoreV1().ConfigMaps("shiftpv-system").Get(context.Background(), id, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected reservation deletion, got %v", err)
	}
}

func TestDeleteVolumeIsIdempotentWhenReservationIsMissing(t *testing.T) {
	id, err := volume.IDFromName("missing")
	if err != nil {
		t.Fatal(err)
	}
	operator := &fakeDirectoryOperator{}
	service := &Service{Client: fake.NewClientset(), Namespace: "shiftpv-system", Operator: operator}
	if _, err := service.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: id}); err != nil {
		t.Fatal(err)
	}
	if operator.deletedID != "" {
		t.Fatal("directory deletion was called for a missing reservation")
	}
}

func TestDeleteVolumePreservesReservationOnDirectoryFailure(t *testing.T) {
	id, err := volume.IDFromName("pvc-uid")
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientset(reservation(id, "worker-a"))
	operator := &fakeDirectoryOperator{deleteErr: errors.New("rm failed")}
	service := &Service{Client: client, Namespace: "shiftpv-system", Operator: operator}

	_, err = service.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: id})
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal, got %v", err)
	}
	if _, getErr := client.CoreV1().ConfigMaps("shiftpv-system").Get(context.Background(), id, metav1.GetOptions{}); getErr != nil {
		t.Fatalf("reservation was removed after directory failure: %v", getErr)
	}
}

func TestDeleteVolumeMapsRetryableDirectoryFailureToUnavailable(t *testing.T) {
	id, err := volume.IDFromName("pvc-uid")
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientset(reservation(id, "worker-a"))
	operator := &fakeDirectoryOperator{deleteErr: retryableDirectoryError{errors.New("filesystem unavailable")}}
	service := &Service{Client: client, Namespace: "shiftpv-system", Operator: operator}

	_, err = service.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: id})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable, got %v", err)
	}
	if _, getErr := client.CoreV1().ConfigMaps("shiftpv-system").Get(context.Background(), id, metav1.GetOptions{}); getErr != nil {
		t.Fatalf("reservation was removed after retryable directory failure: %v", getErr)
	}
}

func TestDeleteVolumeRetriesAfterReservationReadTimeout(t *testing.T) {
	id, err := volume.IDFromName("pvc-uid")
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientset(reservation(id, "worker-a"))
	operator := &fakeDirectoryOperator{}
	service := &Service{Client: client, Namespace: "shiftpv-system", Operator: operator}
	timedOut := false
	client.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if timedOut {
			return false, nil, nil
		}
		timedOut = true
		return true, nil, apierrors.NewServiceUnavailable("API server unavailable")
	})

	request := &csi.DeleteVolumeRequest{VolumeId: id}
	if _, err := service.DeleteVolume(context.Background(), request); status.Code(err) != codes.Unavailable {
		t.Fatalf("expected retryable Unavailable, got %v", err)
	}
	if operator.deleteCalls != 0 {
		t.Fatalf("directory was deleted before reading its reservation: %d calls", operator.deleteCalls)
	}
	if _, err := service.DeleteVolume(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if operator.deleteCalls != 1 {
		t.Fatalf("retry did not delete the directory exactly once: %d calls", operator.deleteCalls)
	}
}

func TestDeleteVolumeConvergesAfterAmbiguousReservationDeleteTimeout(t *testing.T) {
	id, err := volume.IDFromName("pvc-uid")
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientset(reservation(id, "worker-a"))
	operator := &fakeDirectoryOperator{}
	service := &Service{Client: client, Namespace: "shiftpv-system", Operator: operator}
	timedOut := false
	client.PrependReactor("delete", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if timedOut {
			return false, nil, nil
		}
		timedOut = true
		deleteAction := action.(k8stesting.DeleteAction)
		if err := client.Tracker().Delete(action.GetResource(), action.GetNamespace(), deleteAction.GetName()); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewTimeoutError("reservation delete response timed out", 1)
	})

	request := &csi.DeleteVolumeRequest{VolumeId: id}
	if _, err := service.DeleteVolume(context.Background(), request); status.Code(err) != codes.Unavailable {
		t.Fatalf("expected retryable Unavailable, got %v", err)
	}
	if operator.deleteCalls != 1 {
		t.Fatalf("directory delete calls after ambiguous response: %d", operator.deleteCalls)
	}
	if _, err := service.DeleteVolume(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if operator.deleteCalls != 1 {
		t.Fatalf("idempotent retry repeated directory deletion: %d calls", operator.deleteCalls)
	}
}

func TestDeleteVolumeRejectsReservationWithoutOwner(t *testing.T) {
	id, err := volume.IDFromName("pvc-uid")
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{
		Client:    fake.NewClientset(reservation(id, "")),
		Namespace: "shiftpv-system",
		Operator:  &fakeDirectoryOperator{},
	}
	_, err = service.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: id})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
}

func TestDeleteVolumeRejectsUnconfiguredController(t *testing.T) {
	id, err := volume.IDFromName("pvc-uid")
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Service{}).DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: id})
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal, got %v", err)
	}
}

func TestControllerCapabilitiesAdvertiseCreateDeleteOnly(t *testing.T) {
	response, err := (&Service{}).ControllerGetCapabilities(context.Background(), &csi.ControllerGetCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Capabilities) != 1 || response.Capabilities[0].GetRpc().GetType() != csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME {
		t.Fatalf("unexpected capabilities: %#v", response.Capabilities)
	}
}

func TestValidateVolumeCapabilitiesReturnsMessageForUnsupportedMode(t *testing.T) {
	id, err := volume.IDFromName("pvc-uid")
	if err != nil {
		t.Fatal(err)
	}
	capability := validCreateRequest("worker-a").VolumeCapabilities[0]
	capability.AccessMode.Mode = csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
	response, err := (&Service{}).ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           id,
		VolumeCapabilities: []*csi.VolumeCapability{capability},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Confirmed != nil || response.Message == "" {
		t.Fatalf("expected unsupported response message, got %#v", response)
	}
}

func reservation(id, node string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: "shiftpv-system"},
		Data:       map[string]string{"nodeName": node},
	}
}

func reservationForCreateRequest(id string, req *csi.CreateVolumeRequest) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: "shiftpv-system"},
		Data: map[string]string{
			"requestName": req.Name,
			"volumeID":    id,
			"nodeName":    req.AccessibilityRequirements.Preferred[0].Segments[TopologyKey],
			"capacity":    "67108864",
		},
	}
}

type blockingDirectoryOperator struct {
	mu            sync.Mutex
	directory     bool
	deleteStarted chan struct{}
	createStarted chan struct{}
	allowDelete   chan struct{}
	allowCreate   chan struct{}
}

func newBlockingDeleteOperator() *blockingDirectoryOperator {
	return &blockingDirectoryOperator{
		directory:     true,
		deleteStarted: make(chan struct{}, 1),
		createStarted: make(chan struct{}, 1),
		allowDelete:   make(chan struct{}),
	}
}

func newBlockingCreateOperator() *blockingDirectoryOperator {
	return &blockingDirectoryOperator{
		createStarted: make(chan struct{}, 2),
		allowCreate:   make(chan struct{}),
	}
}

func (o *blockingDirectoryOperator) Create(context.Context, string, string) error {
	o.createStarted <- struct{}{}
	if o.allowCreate != nil {
		<-o.allowCreate
	}
	o.mu.Lock()
	o.directory = true
	o.mu.Unlock()
	return nil
}

func (o *blockingDirectoryOperator) Delete(context.Context, string, string) error {
	o.deleteStarted <- struct{}{}
	<-o.allowDelete
	o.mu.Lock()
	o.directory = false
	o.mu.Unlock()
	return nil
}

func (o *blockingDirectoryOperator) exists() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.directory
}

func validCreateRequest(node string) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name:          "pvc-uid",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 64 << 20},
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		}},
		Parameters: map[string]string{CapacityEnforcementKey: capacityEnforcementNone},
		AccessibilityRequirements: &csi.TopologyRequirement{Preferred: []*csi.Topology{{
			Segments: map[string]string{TopologyKey: node},
		}}},
	}
}
