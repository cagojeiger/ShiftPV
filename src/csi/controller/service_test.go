package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/cagojeiger/ShiftPV/src/volume"
)

type fakeDirectoryOperator struct {
	createdNode string
	createdID   string
	deletedNode string
	deletedID   string
	createErr   error
	deleteErr   error
}

func (f *fakeDirectoryOperator) Create(_ context.Context, node, id string) error {
	f.createdNode = node
	f.createdID = id
	return f.createErr
}

func (f *fakeDirectoryOperator) Delete(_ context.Context, node, id string) error {
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
