package node

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	controllercsi "github.com/cagojeiger/ShiftPV/src/csi/controller"
	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	shiftmount "github.com/cagojeiger/ShiftPV/src/node/mount"
	"github.com/cagojeiger/ShiftPV/src/volume"
)

type Binder interface {
	Publish(source, target string) error
	Unpublish(target string) error
}

type VolumeRegistry interface {
	Get(context.Context, string) (volumeapi.State, error)
	SetPublished(context.Context, string, string, bool) error
}

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
	ownerNode := req.GetVolumeContext()[controllercsi.NodeContextKey]
	if s.Volumes != nil {
		state, err := s.Volumes.Get(ctx, req.GetVolumeId())
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "read volume state: %v", err)
		}
		if state.Phase != volumeapi.PhaseReady {
			return nil, status.Errorf(codes.FailedPrecondition, "volume state is %q", state.Phase)
		}
		ownerNode = state.OwnerNode
	}
	if ownerNode == "" {
		return nil, status.Error(codes.FailedPrecondition, "volume context has no owner node")
	}
	if ownerNode != s.NodeName {
		return nil, status.Errorf(codes.FailedPrecondition, "volume is owned by node %q, not %q", ownerNode, s.NodeName)
	}
	poolRoot, err := s.poolRoot(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "resolve node pool: %v", err)
	}
	source, err := volume.Path(poolRoot, req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.Binder.Publish(source, req.GetTargetPath()); err != nil {
		return nil, status.Errorf(codes.Internal, "publish volume: %v", err)
	}
	if s.Volumes != nil {
		if err := s.Volumes.SetPublished(ctx, req.GetVolumeId(), s.NodeName, true); err != nil {
			return nil, status.Errorf(codes.Unavailable, "record published volume: %v", err)
		}
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
	if err := s.Binder.Unpublish(req.GetTargetPath()); err != nil {
		return nil, status.Errorf(codes.Internal, "unpublish volume: %v", err)
	}
	if s.Volumes != nil {
		if err := s.Volumes.SetPublished(ctx, req.GetVolumeId(), s.NodeName, false); err != nil {
			return nil, status.Errorf(codes.Unavailable, "record unpublished volume: %v", err)
		}
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
	if s.NodeName == "" || s.HostRoot == "" || s.Pools == nil || s.TargetRoot == "" || s.Binder == nil {
		return fmt.Errorf("node service is not configured")
	}
	return nil
}

func (s *Service) poolRoot(ctx context.Context) (string, error) {
	pool, err := s.Pools.PoolForNode(ctx, s.NodeName)
	if err != nil {
		return "", err
	}
	mountPath := filepath.Clean(pool.MountPath)
	if !filepath.IsAbs(mountPath) || mountPath == string(filepath.Separator) {
		return "", fmt.Errorf("pool mountPath %q must be an absolute non-root path", pool.MountPath)
	}
	hostRoot := filepath.Clean(s.HostRoot)
	if !filepath.IsAbs(hostRoot) {
		return "", fmt.Errorf("host root %q must be absolute", s.HostRoot)
	}
	return filepath.Join(hostRoot, strings.TrimPrefix(mountPath, string(filepath.Separator))), nil
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
