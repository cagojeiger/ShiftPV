package node

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/keymutex"

	"github.com/project-jelly/ShiftPV/src/kubernetes/volumeapi"
	shiftmount "github.com/project-jelly/ShiftPV/src/node/mount"
	"github.com/project-jelly/ShiftPV/src/node/ownership"
	"github.com/project-jelly/ShiftPV/src/node/publication"
	"github.com/project-jelly/ShiftPV/src/volume"
)

// Binder and VolumeRegistry are the node-local effect contracts; the adapter
// shares one declaration with the publication package.
type (
	Binder         = publication.Binder
	VolumeRegistry = publication.VolumeRegistry
)

type PoolRegistry interface {
	PoolForNode(context.Context, string) (volumeapi.Pool, error)
}

type Service struct {
	csi.UnimplementedNodeServer
	NodeName   string
	HostRoot   string
	Pools      PoolRegistry
	TargetRoot string
	Binder     Binder
	Volumes    VolumeRegistry

	publicationLocksOnce sync.Once
	publicationLocks     keymutex.KeyMutex
}

func (s *Service) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if err := s.validate(); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if req.GetVolumeId() == "" || req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID and target path are required")
	}
	if req.GetReadonly() {
		return nil, status.Error(codes.InvalidArgument, "read-only publish is not supported")
	}
	if err := validateCapability(req.GetVolumeCapability()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := shiftmount.ValidateTarget(s.TargetRoot, req.GetTargetPath()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	unlock := s.lockPublication(req.GetVolumeId())
	defer unlock()
	state, err := s.Volumes.Get(ctx, req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "read volume state: %v", err)
	}
	if state.Phase != volumeapi.PhaseReady {
		return nil, status.Errorf(codes.FailedPrecondition, "volume state is %q", state.Phase)
	}
	ownerNode := state.OwnerNode
	if ownerNode == "" {
		return nil, status.Error(codes.FailedPrecondition, "volume context has no owner node")
	}
	if ownerNode != s.NodeName {
		return nil, status.Errorf(codes.FailedPrecondition, "volume is owned by node %q, not %q", ownerNode, s.NodeName)
	}
	if state.CurrentCopy == nil {
		return nil, status.Error(codes.FailedPrecondition, "identified volume state is required for publish")
	}
	copy := *state.CurrentCopy
	if copy.NodeName != s.NodeName || copy.Role != volume.RoleServing {
		return nil, status.Error(codes.FailedPrecondition, "serving copy identity does not match this node")
	}
	pool, poolRoot, err := s.poolRoot(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "resolve node pool: %v", err)
	}
	if !publication.CopyMatchesPool(copy, pool) {
		return nil, status.Error(codes.FailedPrecondition, "serving copy identity does not match the registered node pool")
	}
	source, err := volume.Path(poolRoot, req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.publisher().Publish(ctx, publication.PublishRequest{
		VolumeID:   req.GetVolumeId(),
		TargetPath: req.GetTargetPath(),
		Source:     source,
		PoolRoot:   poolRoot,
		Copy:       copy,
	}); err != nil {
		return nil, publicationError("publish", err, true)
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

func (s *Service) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if err := s.validate(); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if req.GetVolumeId() == "" || req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID and target path are required")
	}
	if err := volume.ValidateID(req.GetVolumeId()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := shiftmount.ValidateTarget(s.TargetRoot, req.GetTargetPath()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	unlock := s.lockPublication(req.GetVolumeId())
	defer unlock()
	if err := s.Binder.Unpublish(req.GetTargetPath()); err != nil {
		return nil, status.Errorf(codes.Internal, "unmount target: %v", err)
	}
	state, err := s.Volumes.Get(ctx, req.GetVolumeId())
	if apierrors.IsNotFound(err) {
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "read volume state before unpublish: %v", err)
	}
	if state.CurrentCopy == nil || state.CurrentCopy.Role != volume.RoleServing {
		return nil, status.Error(codes.FailedPrecondition, "identified serving copy is required for unpublish")
	}
	if state.CurrentCopy.NodeName != s.NodeName {
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}
	copy := *state.CurrentCopy
	pool, poolRoot, err := s.poolRoot(ctx)
	if err != nil {
		if publication.PoolIdentityUnavailable(err) {
			return &csi.NodeUnpublishVolumeResponse{}, nil
		}
		return nil, status.Errorf(codes.Unavailable, "resolve node pool after unpublish: %v", err)
	}
	if !publication.CopyMatchesPool(copy, pool) {
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}
	source, err := volume.Path(poolRoot, req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	enteredLock, err := s.publisher().Unpublish(ctx, publication.UnpublishRequest{
		VolumeID: req.GetVolumeId(),
		Source:   source,
		PoolRoot: poolRoot,
		StateUID: state.UID,
		Copy:     copy,
	})
	if err != nil {
		if errors.Is(err, publication.ErrIdentityUnavailable) || !enteredLock &&
			(errors.Is(err, ownership.ErrIdentity) || errors.Is(err, ownership.ErrNeedsReview)) {
			return &csi.NodeUnpublishVolumeResponse{}, nil
		}
		return nil, publicationError("unpublish", err, enteredLock)
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// publicationError maps one publication failure to its gRPC status. enteredLock
// is false only when the storage lock was never entered, which unpublish
// reports as a retryable reconcile failure; publish never reaches the lock
// without the callback running, so it always passes true.
func publicationError(op string, err error, enteredLock bool) error {
	if !enteredLock {
		return status.Errorf(codes.Unavailable, "reconcile publication after %s: %v", op, err)
	}
	if errors.Is(err, volumeapi.ErrStateConflict) || errors.Is(err, ownership.ErrIdentity) || errors.Is(err, ownership.ErrNeedsReview) {
		return status.Errorf(codes.FailedPrecondition, "%s identified volume: %v", op, err)
	}
	if errors.Is(err, publication.ErrObservationRetry) || errors.Is(err, ownership.ErrBusy) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) || apierrors.IsServiceUnavailable(err) {
		return status.Errorf(codes.Unavailable, "%s identified volume: %v", op, err)
	}
	return status.Errorf(codes.Internal, "%s identified volume: %v", op, err)
}

func (s *Service) publisher() publication.Publisher {
	return publication.Publisher{
		NodeName:   s.NodeName,
		TargetRoot: s.TargetRoot,
		Binder:     s.Binder,
		Volumes:    s.Volumes,
		Pools:      s.poolRoot,
	}
}

func (s *Service) NodeGetInfo(context.Context, *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	if err := s.validate(); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeGetInfoResponse{
		NodeId: s.NodeName,
		AccessibleTopology: &csi.Topology{Segments: map[string]string{
			volume.TopologyKey: s.NodeName,
		}},
	}, nil
}

func (s *Service) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{}, nil
}

func (s *Service) validate() error {
	if s.NodeName == "" || s.HostRoot == "" || s.Pools == nil || s.TargetRoot == "" || s.Binder == nil || s.Volumes == nil {
		return fmt.Errorf("node service is not configured")
	}
	return nil
}

func (s *Service) lockPublication(volumeID string) func() {
	s.publicationLocksOnce.Do(func() {
		s.publicationLocks = keymutex.NewHashed(32)
	})
	s.publicationLocks.LockKey(volumeID)
	return func() { _ = s.publicationLocks.UnlockKey(volumeID) }
}

func (s *Service) poolRoot(ctx context.Context) (volumeapi.Pool, string, error) {
	pool, err := s.Pools.PoolForNode(ctx, s.NodeName)
	if err != nil {
		return volumeapi.Pool{}, "", err
	}
	mountPath := filepath.Clean(pool.MountPath)
	if !filepath.IsAbs(mountPath) || mountPath == string(filepath.Separator) {
		return volumeapi.Pool{}, "", fmt.Errorf("pool mountPath %q must be an absolute non-root path", pool.MountPath)
	}
	hostRoot := filepath.Clean(s.HostRoot)
	if !filepath.IsAbs(hostRoot) {
		return volumeapi.Pool{}, "", fmt.Errorf("host root %q must be absolute", s.HostRoot)
	}
	return pool, filepath.Join(hostRoot, strings.TrimPrefix(mountPath, string(filepath.Separator))), nil
}

func validateCapability(capability *csi.VolumeCapability) error {
	if capability == nil || capability.GetMount() == nil {
		return fmt.Errorf("only filesystem volumes are supported")
	}
	if capability.GetAccessMode() == nil || capability.GetAccessMode().GetMode() != csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER {
		return fmt.Errorf("only SINGLE_NODE_WRITER is supported")
	}
	return nil
}
