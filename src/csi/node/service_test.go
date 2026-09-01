package node

import (
	"context"
	"errors"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	controllercsi "github.com/cagojeiger/ShiftPV/src/csi/controller"
)

type fakeBinder struct {
	publishedSource string
	publishedTarget string
	unpublished     string
	publishErr      error
	unpublishErr    error
}

func (f *fakeBinder) Publish(source, target string) error {
	f.publishedSource = source
	f.publishedTarget = target
	return f.publishErr
}
func (f *fakeBinder) Unpublish(target string) error {
	f.unpublished = target
	return f.unpublishErr
}

func TestNodePublishUsesCanonicalOwnerPath(t *testing.T) {
	binder := &fakeBinder{}
	service := configuredService(binder)
	request := validPublishRequest()

	if _, err := service.NodePublishVolume(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	expectedSource := "/mnt/shiftpv/volumes/shiftpv-0123456789abcdef0123456789abcdef"
	if binder.publishedSource != expectedSource || binder.publishedTarget != request.TargetPath {
		t.Fatalf("unexpected publish: source=%q target=%q", binder.publishedSource, binder.publishedTarget)
	}
}

func TestNodePublishRejectsWrongOwner(t *testing.T) {
	service := &Service{NodeName: "worker-a", PoolRoot: "/mnt/shiftpv", TargetRoot: "/var/lib/kubelet/pods", Binder: &fakeBinder{}}
	_, err := service.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "shiftpv-0123456789abcdef0123456789abcdef",
		TargetPath:       "/var/lib/kubelet/pods/uid/volumes/csi/mount",
		VolumeContext:    map[string]string{controllercsi.NodeContextKey: "worker-b"},
		VolumeCapability: singleNodeWriterCapability(),
	})
	if err == nil {
		t.Fatal("expected wrong owner node to fail")
	}
}

func TestNodeGetInfoPublishesTopology(t *testing.T) {
	service := &Service{NodeName: "worker-a", PoolRoot: "/mnt/shiftpv", TargetRoot: "/var/lib/kubelet/pods", Binder: &fakeBinder{}}
	response, err := service.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := response.AccessibleTopology.Segments[controllercsi.TopologyKey]; got != "worker-a" {
		t.Fatalf("unexpected topology: %q", got)
	}
}

func TestNodePublishRejectsInvalidRequests(t *testing.T) {
	tests := map[string]struct {
		mutate func(*csi.NodePublishVolumeRequest)
		code   codes.Code
	}{
		"missing volume ID": {
			mutate: func(req *csi.NodePublishVolumeRequest) { req.VolumeId = "" },
			code:   codes.InvalidArgument,
		},
		"read only": {
			mutate: func(req *csi.NodePublishVolumeRequest) { req.Readonly = true },
			code:   codes.InvalidArgument,
		},
		"raw block": {
			mutate: func(req *csi.NodePublishVolumeRequest) {
				req.VolumeCapability.AccessType = &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}}
			},
			code: codes.InvalidArgument,
		},
		"unsupported access mode": {
			mutate: func(req *csi.NodePublishVolumeRequest) {
				req.VolumeCapability.AccessMode.Mode = csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
			},
			code: codes.InvalidArgument,
		},
		"outside kubelet root": {
			mutate: func(req *csi.NodePublishVolumeRequest) { req.TargetPath = "/tmp/escape" },
			code:   codes.InvalidArgument,
		},
		"missing owner": {
			mutate: func(req *csi.NodePublishVolumeRequest) { delete(req.VolumeContext, controllercsi.NodeContextKey) },
			code:   codes.FailedPrecondition,
		},
		"invalid volume ID": {
			mutate: func(req *csi.NodePublishVolumeRequest) { req.VolumeId = "../escape" },
			code:   codes.InvalidArgument,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			request := validPublishRequest()
			test.mutate(request)
			_, err := configuredService(&fakeBinder{}).NodePublishVolume(context.Background(), request)
			if status.Code(err) != test.code {
				t.Fatalf("expected %s, got %v", test.code, err)
			}
		})
	}
}

func TestNodePublishReportsBinderFailure(t *testing.T) {
	service := configuredService(&fakeBinder{publishErr: errors.New("mount failed")})
	_, err := service.NodePublishVolume(context.Background(), validPublishRequest())
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal, got %v", err)
	}
}

func TestNodePublishRejectsUnconfiguredService(t *testing.T) {
	_, err := (&Service{}).NodePublishVolume(context.Background(), validPublishRequest())
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal, got %v", err)
	}
}

func TestNodeUnpublishValidatesAndDelegates(t *testing.T) {
	binder := &fakeBinder{}
	service := configuredService(binder)
	request := &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "shiftpv-0123456789abcdef0123456789abcdef",
		TargetPath: "/var/lib/kubelet/pods/uid/volumes/csi/mount",
	}
	if _, err := service.NodeUnpublishVolume(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if binder.unpublished != request.TargetPath {
		t.Fatalf("unexpected target: %q", binder.unpublished)
	}
}

func TestNodeUnpublishRejectsUnsafeTarget(t *testing.T) {
	service := configuredService(&fakeBinder{})
	_, err := service.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "shiftpv-0123456789abcdef0123456789abcdef",
		TargetPath: "/var/lib/kubelet/pods-evil/uid",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestNodeUnpublishReportsBinderFailure(t *testing.T) {
	service := configuredService(&fakeBinder{unpublishErr: errors.New("unmount failed")})
	_, err := service.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "shiftpv-0123456789abcdef0123456789abcdef",
		TargetPath: "/var/lib/kubelet/pods/uid/volumes/csi/mount",
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal, got %v", err)
	}
}

func TestNodeUnpublishRejectsUnconfiguredService(t *testing.T) {
	_, err := (&Service{}).NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "shiftpv-0123456789abcdef0123456789abcdef",
		TargetPath: "/var/lib/kubelet/pods/uid/volumes/csi/mount",
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal, got %v", err)
	}
}

func TestNodeCapabilitiesAdvertiseNoOptionalServices(t *testing.T) {
	response, err := (&Service{}).NodeGetCapabilities(context.Background(), &csi.NodeGetCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Capabilities) != 0 {
		t.Fatalf("unexpected capabilities: %#v", response.Capabilities)
	}
}

func configuredService(binder Binder) *Service {
	return &Service{
		NodeName:   "worker-a",
		PoolRoot:   "/mnt/shiftpv",
		TargetRoot: "/var/lib/kubelet/pods",
		Binder:     binder,
	}
}

func validPublishRequest() *csi.NodePublishVolumeRequest {
	return &csi.NodePublishVolumeRequest{
		VolumeId:         "shiftpv-0123456789abcdef0123456789abcdef",
		TargetPath:       "/var/lib/kubelet/pods/uid/volumes/csi/mount",
		VolumeContext:    map[string]string{controllercsi.NodeContextKey: "worker-a"},
		VolumeCapability: singleNodeWriterCapability(),
	}
}

func singleNodeWriterCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
}
