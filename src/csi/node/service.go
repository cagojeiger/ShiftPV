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

	controllercsi "github.com/cagojeiger/ShiftPV/src/csi/controller"
	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	shiftmount "github.com/cagojeiger/ShiftPV/src/node/mount"
	"github.com/cagojeiger/ShiftPV/src/node/ownership"
	"github.com/cagojeiger/ShiftPV/src/volume"
)

type Binder interface {
	Publish(source, target string) error
	Unpublish(target string) error
	HasPublishedTarget(source, targetRoot string) (bool, error)
}

type VolumeRegistry interface {
	Get(context.Context, string) (volumeapi.State, error)
	BeginPublish(context.Context, string, string, volume.CopyIdentity) error
	ReconcilePublished(context.Context, string, string, volume.CopyIdentity, bool) error
}

type PoolRegistry interface {
	PoolForNode(context.Context, string) (volumeapi.Pool, error)
}

var errPublicationIdentityUnavailable = errors.New("publication identity is no longer verifiable")

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
	if !copyMatchesPool(copy, pool) {
		return nil, status.Error(codes.FailedPrecondition, "serving copy identity does not match the registered node pool")
	}
	source, err := volume.Path(poolRoot, req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	err = ownership.WithLock(ctx, poolRoot, ownership.PoolIdentity{InstallationID: copy.InstallationID, PoolUID: copy.PoolUID}, req.GetVolumeId(), func(store *ownership.Store) error {
		freshPool, freshPoolRoot, poolErr := s.poolRoot(ctx)
		if poolErr != nil {
			return fmt.Errorf("refresh node pool before publish: %w", poolErr)
		}
		if freshPoolRoot != poolRoot || !copyMatchesPool(copy, freshPool) {
			return fmt.Errorf("registered node pool changed before publish: %w", volumeapi.ErrStateConflict)
		}
		fresh, getErr := s.Volumes.Get(ctx, req.GetVolumeId())
		if getErr != nil || fresh.CurrentCopy == nil || *fresh.CurrentCopy != copy || fresh.Phase != volumeapi.PhaseReady || fresh.OwnerNode != s.NodeName {
			return fmt.Errorf("volume publish authority changed: %w", errors.Join(getErr, volumeapi.ErrStateConflict))
		}
		if verifyErr := store.VerifyServing(copy); verifyErr != nil {
			return verifyErr
		}
		if publishErr := s.Volumes.BeginPublish(ctx, req.GetVolumeId(), s.NodeName, copy); publishErr != nil {
			return publishErr
		}
		if publishErr := s.Binder.Publish(source, req.GetTargetPath()); publishErr != nil {
			stillPublished, inspectErr := s.Binder.HasPublishedTarget(source, s.TargetRoot)
			if inspectErr != nil {
				return errors.Join(publishErr, inspectErr)
			}
			if reconcileErr := s.Volumes.ReconcilePublished(ctx, req.GetVolumeId(), s.NodeName, copy, stillPublished); reconcileErr != nil {
				return errors.Join(publishErr, reconcileErr)
			}
			return publishErr
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, volumeapi.ErrStateConflict) || errors.Is(err, ownership.ErrIdentity) || errors.Is(err, ownership.ErrNeedsReview) {
			return nil, status.Errorf(codes.FailedPrecondition, "publish identified volume: %v", err)
		}
		if errors.Is(err, ownership.ErrBusy) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) || apierrors.IsServiceUnavailable(err) {
			return nil, status.Errorf(codes.Unavailable, "publish identified volume: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "publish identified volume: %v", err)
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
	if err != nil || !copyMatchesPool(copy, pool) {
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}
	source, err := volume.Path(poolRoot, req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	enteredLock := false
	err = ownership.WithLock(ctx, poolRoot, ownership.PoolIdentity{InstallationID: copy.InstallationID, PoolUID: copy.PoolUID}, req.GetVolumeId(), func(*ownership.Store) error {
		enteredLock = true
		freshPool, freshPoolRoot, poolErr := s.poolRoot(ctx)
		if poolErr != nil || freshPoolRoot != poolRoot || !copyMatchesPool(copy, freshPool) {
			return errPublicationIdentityUnavailable
		}
		fresh, getErr := s.Volumes.Get(ctx, req.GetVolumeId())
		if getErr == nil && fresh.CurrentCopy != nil && fresh.CurrentCopy.Role == volume.RoleServing && fresh.CurrentCopy.NodeName != s.NodeName {
			return nil
		}
		if getErr != nil || fresh.UID != state.UID || fresh.CurrentCopy == nil || *fresh.CurrentCopy != copy || fresh.OwnerNode != s.NodeName {
			return fmt.Errorf("volume unpublish authority changed: %w", errors.Join(getErr, volumeapi.ErrStateConflict))
		}
		stillPublished, inspectErr := s.Binder.HasPublishedTarget(source, s.TargetRoot)
		if inspectErr != nil {
			return inspectErr
		}
		return s.Volumes.ReconcilePublished(ctx, req.GetVolumeId(), s.NodeName, copy, stillPublished)
	})
	if err != nil {
		if errors.Is(err, errPublicationIdentityUnavailable) || (!enteredLock && !errors.Is(err, ownership.ErrBusy)) {
			return &csi.NodeUnpublishVolumeResponse{}, nil
		}
		if errors.Is(err, volumeapi.ErrStateConflict) || errors.Is(err, ownership.ErrIdentity) || errors.Is(err, ownership.ErrNeedsReview) {
			return nil, status.Errorf(codes.FailedPrecondition, "unpublish identified volume: %v", err)
		}
		if apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) || apierrors.IsServiceUnavailable(err) {
			return nil, status.Errorf(codes.Unavailable, "unpublish identified volume: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "unpublish identified volume: %v", err)
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (s *Service) NodeGetInfo(context.Context, *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	if err := s.validate(); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeGetInfoResponse{
		NodeId: s.NodeName,
		AccessibleTopology: &csi.Topology{Segments: map[string]string{
			controllercsi.TopologyKey: s.NodeName,
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

func copyMatchesPool(copy volume.CopyIdentity, pool volumeapi.Pool) bool {
	return copy.PoolName == pool.Name && copy.PoolUID == pool.UID && copy.NodeName == pool.NodeName
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
