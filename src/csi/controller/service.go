package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/volume"
)

const (
	TopologyKey             = "topology.csi.shiftpv.io/node"
	NodeContextKey          = "shiftpv.io/node"
	CapacityEnforcementKey  = "shiftpv.io/capacity-enforcement"
	PVCNameKey              = "csi.storage.k8s.io/pvc/name"
	PVCNamespaceKey         = "csi.storage.k8s.io/pvc/namespace"
	PVNameKey               = "csi.storage.k8s.io/pv/name"
	MobilityAdmissionLabel  = "shiftpv.io/admission"
	capacityEnforcementNone = "none"
	mobilityEnabledValue    = "enabled"
)

type DirectoryOperator interface {
	Create(context.Context, string, string) error
	Delete(context.Context, string, string) error
}

type VolumeRegistry interface {
	Ensure(context.Context, string, string) error
	Get(context.Context, string) (volumeapi.State, error)
	Delete(context.Context, string) error
	PoolNodes(context.Context) ([]string, error)
}

type ProvisioningGate interface {
	Enter() (func(), error)
}

type Service struct {
	csi.UnimplementedControllerServer
	Client           kubernetes.Interface
	Namespace        string
	Operator         DirectoryOperator
	Volumes          VolumeRegistry
	ProvisioningGate ProvisioningGate
	lifecycles       volumeLifecycles
}

func (s *Service) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if err := validateCapabilities(req.GetVolumeCapabilities()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := validateParameters(req.GetParameters()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	capacity, err := requestedCapacity(req.GetCapacityRange())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	nodeName, err := selectedNode(req.GetAccessibilityRequirements())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	id, err := volume.IDFromName(req.GetName())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if s.ProvisioningGate != nil {
		leave, err := s.ProvisioningGate.Enter()
		if err != nil {
			return nil, status.Error(codes.Unavailable, err.Error())
		}
		defer leave()
	}
	unlock := s.lifecycles.lock(id)
	defer unlock()

	if err := s.reserve(ctx, id, req.GetName(), nodeName, capacity); err != nil {
		return nil, err
	}
	if err := s.Operator.Create(ctx, nodeName, id); err != nil {
		return nil, directoryOperationError("prepare volume directory", err)
	}
	accessibleNodes := []string{nodeName}
	if s.Volumes != nil {
		if err := s.Volumes.Ensure(ctx, id, nodeName); err != nil {
			return nil, kubernetesAPIError("register volume state", err)
		}
		poolNodes, poolErr := s.Volumes.PoolNodes(ctx)
		if poolErr != nil {
			return nil, kubernetesAPIError("list volume topology", poolErr)
		}
		if !contains(poolNodes, nodeName) {
			return nil, status.Errorf(codes.FailedPrecondition, "selected node %q has no registered ShiftPVPool", nodeName)
		}
		accessibleNodes, err = s.accessibleNodes(ctx, req.GetParameters(), nodeName, poolNodes)
		if err != nil {
			return nil, err
		}
	}

	return volumeResponse(id, nodeName, accessibleNodes, capacity), nil
}

func (s *Service) accessibleNodes(ctx context.Context, parameters map[string]string, owner string, poolNodes []string) ([]string, error) {
	namespaceName := parameters[PVCNamespaceKey]
	if namespaceName == "" {
		return []string{owner}, nil
	}
	namespace, err := s.Client.CoreV1().Namespaces().Get(ctx, namespaceName, metav1.GetOptions{})
	if err != nil {
		return nil, kubernetesAPIError("read PVC namespace for mobility topology", err)
	}
	if namespace.Labels[MobilityAdmissionLabel] != mobilityEnabledValue {
		return []string{owner}, nil
	}
	return poolNodes, nil
}

func (s *Service) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if err := volume.ValidateID(req.GetVolumeId()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if s.Client == nil || s.Operator == nil || s.Namespace == "" {
		return nil, status.Error(codes.Internal, "controller is not configured")
	}
	unlock := s.lifecycles.lock(req.GetVolumeId())
	defer unlock()
	cm, err := s.Client.CoreV1().ConfigMaps(s.Namespace).Get(ctx, req.GetVolumeId(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return &csi.DeleteVolumeResponse{}, nil
	}
	if err != nil {
		return nil, kubernetesAPIError("read volume reservation", err)
	}
	nodeName := cm.Data["nodeName"]
	if s.Volumes != nil {
		state, stateErr := s.Volumes.Get(ctx, req.GetVolumeId())
		if stateErr != nil && !apierrors.IsNotFound(stateErr) {
			return nil, kubernetesAPIError("read volume state", stateErr)
		}
		if stateErr == nil && state.OwnerNode != "" {
			if state.Phase != volumeapi.PhaseReady || state.ActiveMove != "" {
				return nil, status.Errorf(codes.FailedPrecondition, "volume is phase=%q activeMove=%q", state.Phase, state.ActiveMove)
			}
			nodeName = state.OwnerNode
		}
	}
	if nodeName == "" {
		return nil, status.Error(codes.FailedPrecondition, "volume reservation has no owner node")
	}
	if err := s.Operator.Delete(ctx, nodeName, req.GetVolumeId()); err != nil {
		return nil, directoryOperationError("delete volume directory", err)
	}
	if err := s.Client.CoreV1().ConfigMaps(s.Namespace).Delete(ctx, req.GetVolumeId(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return nil, kubernetesAPIError("delete volume reservation", err)
	}
	if s.Volumes != nil {
		if err := s.Volumes.Delete(ctx, req.GetVolumeId()); err != nil {
			return nil, kubernetesAPIError("delete volume state", err)
		}
	}
	return &csi.DeleteVolumeResponse{}, nil
}

func (s *Service) ControllerGetCapabilities(context.Context, *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: []*csi.ControllerServiceCapability{{
		Type: &csi.ControllerServiceCapability_Rpc{Rpc: &csi.ControllerServiceCapability_RPC{Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME}},
	}}}, nil
}

func (s *Service) ValidateVolumeCapabilities(_ context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if err := volume.ValidateID(req.GetVolumeId()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := validateCapabilities(req.GetVolumeCapabilities()); err != nil {
		return &csi.ValidateVolumeCapabilitiesResponse{Message: err.Error()}, nil
	}
	return &csi.ValidateVolumeCapabilitiesResponse{Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
		VolumeCapabilities: req.GetVolumeCapabilities(),
		Parameters:         req.GetParameters(),
		VolumeContext:      req.GetVolumeContext(),
	}}, nil
}

func (s *Service) reserve(ctx context.Context, id, requestName, nodeName string, capacity int64) error {
	if s.Client == nil || s.Operator == nil || s.Namespace == "" {
		return status.Error(codes.Internal, "controller is not configured")
	}
	data := map[string]string{
		"requestName": requestName,
		"volumeID":    id,
		"nodeName":    nodeName,
		"capacity":    strconv.FormatInt(capacity, 10),
	}
	_, err := s.Client.CoreV1().ConfigMaps(s.Namespace).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: id,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "shiftpv",
				"app.kubernetes.io/component": "volume-reservation",
			},
		},
		Data: data,
	}, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return kubernetesAPIError("reserve volume", err)
	}
	existing, getErr := s.Client.CoreV1().ConfigMaps(s.Namespace).Get(ctx, id, metav1.GetOptions{})
	if getErr != nil {
		return kubernetesAPIError("read existing volume reservation", getErr)
	}
	for key, value := range data {
		if existing.Data[key] != value {
			return status.Errorf(codes.AlreadyExists, "volume %q already exists with incompatible %s", requestName, key)
		}
	}
	return nil
}

func kubernetesAPIError(operation string, err error) error {
	code := codes.Internal
	switch {
	case errors.Is(err, context.Canceled):
		code = codes.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		code = codes.DeadlineExceeded
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), apierrors.IsTooManyRequests(err), apierrors.IsServiceUnavailable(err):
		code = codes.Unavailable
	}
	return status.Errorf(code, "%s: %v", operation, err)
}

func directoryOperationError(operation string, err error) error {
	code := codes.Internal
	var retryable interface{ Retryable() bool }
	if errors.As(err, &retryable) && retryable.Retryable() {
		code = codes.Unavailable
	}
	return status.Errorf(code, "%s: %v", operation, err)
}

func volumeResponse(id, nodeName string, poolNodes []string, capacity int64) *csi.CreateVolumeResponse {
	topologies := make([]*csi.Topology, 0, len(poolNodes))
	for _, poolNode := range poolNodes {
		topologies = append(topologies, &csi.Topology{Segments: map[string]string{TopologyKey: poolNode}})
	}
	return &csi.CreateVolumeResponse{Volume: &csi.Volume{
		VolumeId:      id,
		CapacityBytes: capacity,
		VolumeContext: map[string]string{
			NodeContextKey:         nodeName,
			CapacityEnforcementKey: capacityEnforcementNone,
		},
		AccessibleTopology: topologies,
	}}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func requestedCapacity(capacityRange *csi.CapacityRange) (int64, error) {
	if capacityRange == nil {
		return 0, fmt.Errorf("capacity range is required")
	}
	required := capacityRange.GetRequiredBytes()
	limit := capacityRange.GetLimitBytes()
	if required <= 0 {
		return 0, fmt.Errorf("required capacity must be greater than zero")
	}
	if limit > 0 && required > limit {
		return 0, fmt.Errorf("required capacity exceeds the limit")
	}
	return required, nil
}

func validateParameters(parameters map[string]string) error {
	for key, value := range parameters {
		switch key {
		case CapacityEnforcementKey:
			if value != capacityEnforcementNone {
				return fmt.Errorf("%s must be %q", CapacityEnforcementKey, capacityEnforcementNone)
			}
		case PVCNameKey, PVCNamespaceKey, PVNameKey:
			// Added by csi-provisioner --extra-create-metadata, not by the StorageClass.
		default:
			return fmt.Errorf("unsupported StorageClass parameter %q", key)
		}
	}
	if parameters[CapacityEnforcementKey] != capacityEnforcementNone {
		return fmt.Errorf("%s=%s is required", CapacityEnforcementKey, capacityEnforcementNone)
	}
	return nil
}

func validateCapabilities(capabilities []*csi.VolumeCapability) error {
	if len(capabilities) == 0 {
		return fmt.Errorf("at least one volume capability is required")
	}
	for _, capability := range capabilities {
		if capability.GetMount() == nil {
			return fmt.Errorf("only filesystem volumes are supported")
		}
		if capability.GetAccessMode() == nil || capability.GetAccessMode().GetMode() != csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER {
			return fmt.Errorf("only SINGLE_NODE_WRITER is supported")
		}
	}
	return nil
}

func selectedNode(requirements *csi.TopologyRequirement) (string, error) {
	if requirements == nil {
		return "", fmt.Errorf("selected topology is required; use WaitForFirstConsumer")
	}
	for _, candidates := range [][]*csi.Topology{requirements.GetPreferred(), requirements.GetRequisite()} {
		for _, topology := range candidates {
			if node := topology.GetSegments()[TopologyKey]; node != "" {
				return node, nil
			}
		}
	}
	return "", fmt.Errorf("selected topology does not contain %q", TopologyKey)
}
