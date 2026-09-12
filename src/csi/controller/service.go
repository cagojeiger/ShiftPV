package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/cleanupapi"
	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	poolcapacity "github.com/cagojeiger/ShiftPV/src/pool/capacity"
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
	CreateCopy(context.Context, volume.CopyIdentity) error
	FinalizeCreate(context.Context, volume.CopyIdentity) error
}

type VolumeRegistry interface {
	Get(context.Context, string) (volumeapi.State, error)
	Delete(context.Context, string, string) error
	PoolNodes(context.Context) ([]string, error)
	BeginCreate(context.Context, string, string) (volumeapi.State, error)
	CompleteCreate(context.Context, string, string, volume.CopyIdentity) error
	BeginDelete(context.Context, string, string, volume.CopyIdentity) (volumeapi.State, error)
}

type ProvisioningGate interface {
	Enter() (func(), error)
}

type cleanupOperator interface {
	Reclaim(context.Context, cleanupapi.Cleanup, *cleanupapi.Store) (cleanupapi.Cleanup, error)
}

type Service struct {
	csi.UnimplementedControllerServer
	Client           kubernetes.Interface
	Namespace        string
	Operator         DirectoryOperator
	Volumes          VolumeRegistry
	CapacityPools    PoolCapacityRegistry
	CapacityProbe    PoolCapacityProbe
	PoolLocks        *poolcapacity.Locker
	ProvisioningGate ProvisioningGate
	Cleanups         *cleanupapi.Store
	CleanupOperator  cleanupOperator
	lifecycles       volumeLifecycles
	poolLifecycles   volumeLifecycles
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
	if err := s.validate(); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
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
	if err := s.ensureNoCleanupFence(ctx, id); err != nil {
		return nil, err
	}

	if err := s.reserve(ctx, id, req.GetName(), nodeName, capacity); err != nil {
		return nil, err
	}
	state, beginErr := s.Volumes.BeginCreate(ctx, id, nodeName)
	if beginErr != nil {
		if errors.Is(beginErr, volumeapi.ErrPoolCopyConflict) {
			return nil, status.Error(codes.FailedPrecondition, beginErr.Error())
		}
		return nil, kubernetesAPIError("record volume creation intent", beginErr)
	}
	if state.CurrentCopy == nil {
		return nil, status.Error(codes.FailedPrecondition, "volume creation has no copy identity")
	}
	if bindErr := s.bindReservation(ctx, id, state.UID); bindErr != nil {
		return nil, bindErr
	}
	if createErr := s.Operator.CreateCopy(ctx, *state.CurrentCopy); createErr != nil {
		return nil, directoryOperationError("prepare identified volume directory", createErr)
	}
	if completeErr := s.Volumes.CompleteCreate(ctx, id, state.UID, *state.CurrentCopy); completeErr != nil {
		return nil, kubernetesAPIError("complete volume creation", completeErr)
	}
	if finalizeErr := s.Operator.FinalizeCreate(ctx, *state.CurrentCopy); finalizeErr != nil {
		return nil, directoryOperationError("settle volume creation helper", finalizeErr)
	}
	poolNodes, poolErr := s.Volumes.PoolNodes(ctx)
	if poolErr != nil {
		return nil, kubernetesAPIError("list volume topology", poolErr)
	}
	if !contains(poolNodes, nodeName) {
		return nil, status.Errorf(codes.FailedPrecondition, "selected node %q has no registered ShiftPVPool", nodeName)
	}
	accessibleNodes, err := s.accessibleNodes(ctx, req.GetParameters(), nodeName, poolNodes)
	if err != nil {
		return nil, err
	}

	return volumeResponse(id, nodeName, accessibleNodes, capacity), nil
}

func (s *Service) ensureNoCleanupFence(ctx context.Context, volumeID string) error {
	cleanups, err := s.Cleanups.ListForVolume(ctx, volumeID)
	if err != nil {
		return kubernetesAPIError("list volume cleanup fences", err)
	}
	for _, cleanup := range cleanups {
		if cleanup.Spec.Target.VolumeID == volumeID && cleanup.Status.Phase != cleanupapi.PhaseCompleted {
			return status.Errorf(codes.FailedPrecondition, "volume %q has unresolved cleanup %q in phase %q", volumeID, cleanup.Name, cleanup.Status.Phase)
		}
	}
	return nil
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
	if err := s.validate(); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	unlock := s.lifecycles.lock(req.GetVolumeId())
	defer unlock()
	cm, err := s.Client.CoreV1().ConfigMaps(s.Namespace).Get(ctx, req.GetVolumeId(), metav1.GetOptions{})
	reservationMissing := apierrors.IsNotFound(err)
	if err != nil && !reservationMissing {
		return nil, kubernetesAPIError("read volume reservation", err)
	}
	nodeName := ""
	if !reservationMissing {
		nodeName = cm.Data["nodeName"]
	}
	volumeStateExists := false
	var volumeState volumeapi.State
	state, stateErr := s.Volumes.Get(ctx, req.GetVolumeId())
	if stateErr != nil && !apierrors.IsNotFound(stateErr) {
		return nil, kubernetesAPIError("read volume state", stateErr)
	}
	if stateErr == nil {
		volumeStateExists = true
		volumeState = state
	}
	if volumeStateExists && state.OwnerNode != "" {
		nodeName = state.OwnerNode
	}
	if reservationMissing && !volumeStateExists {
		return &csi.DeleteVolumeResponse{}, nil
	}
	if nodeName == "" {
		return nil, status.Error(codes.FailedPrecondition, "volume reservation has no owner node")
	}
	if !volumeStateExists || volumeState.UID == "" || volumeState.CurrentCopy == nil || volumeState.CurrentCopy.Role != volume.RoleServing {
		return nil, status.Error(codes.FailedPrecondition, "identified volume state is required for cleanup")
	}
	fenced, fenceErr := s.Volumes.BeginDelete(ctx, req.GetVolumeId(), volumeState.UID, *volumeState.CurrentCopy)
	if fenceErr != nil {
		if errors.Is(fenceErr, volumeapi.ErrStateConflict) {
			return nil, status.Errorf(codes.FailedPrecondition, "fence volume deletion: %v", fenceErr)
		}
		return nil, kubernetesAPIError("fence volume deletion", fenceErr)
	}
	reservationUID := ""
	if !reservationMissing {
		reservationUID = string(cm.UID)
	} else {
		existing, getErr := s.Cleanups.Get(ctx, cleanupapi.Name(*fenced.CurrentCopy))
		if getErr == nil {
			reservationUID = existing.Spec.ReservationUID
		} else if !apierrors.IsNotFound(getErr) {
			return nil, kubernetesAPIError("read existing volume cleanup", getErr)
		}
	}
	intent, ensureErr := s.Cleanups.Ensure(ctx, cleanupapi.Spec{
		OperationID:    fenced.DeletionOperationID,
		Target:         *fenced.CurrentCopy,
		Reason:         "VolumeDelete",
		ReservationUID: reservationUID,
		Approved:       true,
		Authority: cleanupapi.Authority{
			Kind: "ShiftPVVolume", Name: req.GetVolumeId(), UID: fenced.UID,
		},
	})
	if ensureErr != nil {
		return nil, kubernetesAPIError("record volume cleanup intent", ensureErr)
	}
	completed, reclaimErr := s.CleanupOperator.Reclaim(ctx, intent, s.Cleanups)
	if reclaimErr != nil {
		return nil, directoryOperationError("reclaim identified volume copy", reclaimErr)
	}
	if completed.Status.Phase != cleanupapi.PhaseVerifying && completed.Status.Phase != cleanupapi.PhaseCompleted {
		return nil, status.Errorf(codes.FailedPrecondition, "cleanup is phase=%q", completed.Status.Phase)
	}
	if !reservationMissing {
		uid := cm.UID
		if err := s.Client.CoreV1().ConfigMaps(s.Namespace).Delete(ctx, req.GetVolumeId(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
			return nil, kubernetesAPIError("delete volume reservation", err)
		}
	}
	completed, completeErr := s.Cleanups.Get(ctx, cleanupapi.Name(*volumeState.CurrentCopy))
	if completeErr != nil {
		return nil, kubernetesAPIError("read verified volume cleanup", completeErr)
	}
	if completed.Status.Phase == cleanupapi.PhaseVerifying {
		completion := completed.Status
		completion.Phase = cleanupapi.PhaseCompleted
		completion.SettledAt = time.Now().UTC().Format(time.RFC3339Nano)
		if completeErr := s.Cleanups.UpdateStatus(ctx, completed.Name, completed.UID, completion); completeErr != nil {
			return nil, kubernetesAPIError("record verified volume cleanup", completeErr)
		}
	} else if completed.Status.Phase != cleanupapi.PhaseCompleted {
		return nil, status.Errorf(codes.FailedPrecondition, "cleanup is phase=%q", completed.Status.Phase)
	}
	if err := s.Volumes.Delete(ctx, req.GetVolumeId(), fenced.UID); err != nil {
		return nil, kubernetesAPIError("delete volume state", err)
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
	data := map[string]string{
		"requestName": requestName,
		"volumeID":    id,
		"nodeName":    nodeName,
		"capacity":    strconv.FormatInt(capacity, 10),
	}
	if s.CapacityPools != nil || s.CapacityProbe != nil {
		if s.CapacityPools == nil || s.CapacityProbe == nil {
			return status.Error(codes.Internal, "Pool capacity admission is incompletely configured")
		}
		return s.reserveWithinPool(ctx, id, requestName, nodeName, capacity, data)
	}
	return s.createReservation(ctx, id, requestName, data)
}

func (s *Service) validate() error {
	if s == nil || s.Client == nil || s.Namespace == "" || s.Operator == nil || s.Volumes == nil || s.Cleanups == nil || s.CleanupOperator == nil {
		return fmt.Errorf("controller exact lifecycle is not configured")
	}
	return nil
}

func (s *Service) createReservation(ctx context.Context, id, requestName string, data map[string]string) error {
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
	return validateReservation(existing, requestName, data)
}

func (s *Service) bindReservation(ctx context.Context, id, volumeUID string) error {
	if !volume.ValidIdentityToken(volumeUID) {
		return status.Error(codes.FailedPrecondition, "volume reservation cannot be bound without a volume UID")
	}
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		reservations := s.Client.CoreV1().ConfigMaps(s.Namespace)
		current, err := reservations.Get(ctx, id, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if current.Data["volumeID"] != id || current.Labels["app.kubernetes.io/name"] != "shiftpv" || current.Labels["app.kubernetes.io/component"] != "volume-reservation" {
			return status.Error(codes.FailedPrecondition, "volume reservation identity changed")
		}
		if current.Data["volumeUID"] == volumeUID {
			return nil
		}
		if current.Data["volumeUID"] != "" {
			return status.Error(codes.AlreadyExists, "volume reservation belongs to another volume incarnation")
		}
		current.Data["volumeUID"] = volumeUID
		_, err = reservations.Update(ctx, current, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		if _, ok := status.FromError(err); ok {
			return err
		}
		return kubernetesAPIError("bind volume reservation", err)
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
			NodeContextKey: nodeName,
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
		case PVCNameKey, PVCNamespaceKey, PVNameKey:
			// Added by csi-provisioner --extra-create-metadata, not by the StorageClass.
		case CapacityEnforcementKey:
			// Kept as a no-op because StorageClass parameters are immutable.
			if value != capacityEnforcementNone {
				return fmt.Errorf("unsupported StorageClass parameter %q value %q", key, value)
			}
		default:
			return fmt.Errorf("unsupported StorageClass parameter %q", key)
		}
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
