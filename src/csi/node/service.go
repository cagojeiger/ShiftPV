package node

import (
	"context"
	"fmt"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	controllercsi "github.com/cagojeiger/ShiftPV/src/csi/controller"
	shiftmount "github.com/cagojeiger/ShiftPV/src/node/mount"
	"github.com/cagojeiger/ShiftPV/src/volume"
)

type Binder interface {
	Publish(source, target string) error
	Unpublish(target string) error
}

type Service struct {
	csi.UnimplementedNodeServer
	NodeName   string
	PoolRoot   string
	TargetRoot string
	Binder     Binder
}

func (s *Service) NodePublishVolume(_ context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
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
	if ownerNode == "" {
		return nil, status.Error(codes.FailedPrecondition, "volume context has no owner node")
	}
	if ownerNode != s.NodeName {
		return nil, status.Errorf(codes.FailedPrecondition, "volume is owned by node %q, not %q", ownerNode, s.NodeName)
	}
	source, err := volume.Path(s.PoolRoot, req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.Binder.Publish(source, req.GetTargetPath()); err != nil {
		return nil, status.Errorf(codes.Internal, "publish volume: %v", err)
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

func (s *Service) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
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
	if s.NodeName == "" || s.PoolRoot == "" || s.TargetRoot == "" || s.Binder == nil {
		return fmt.Errorf("node service is not configured")
	}
	return nil
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
