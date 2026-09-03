package main

import (
	"flag"
	"os"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/cagojeiger/ShiftPV/src/csi/identity"
	nodecsi "github.com/cagojeiger/ShiftPV/src/csi/node"
	csiserver "github.com/cagojeiger/ShiftPV/src/csi/server"
	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	shiftmount "github.com/cagojeiger/ShiftPV/src/node/mount"
)

var version = "dev"

func main() {
	var (
		endpoint   = flag.String("endpoint", "unix:///csi/csi.sock", "CSI Unix socket endpoint")
		nodeName   = flag.String("node-name", os.Getenv("NODE_NAME"), "Kubernetes node name")
		hostRoot   = flag.String("host-root", "/host", "host filesystem root mounted into the node plugin")
		targetRoot = flag.String("target-root", "/var/lib/kubelet/pods", "allowed kubelet publish target root")
	)
	klog.InitFlags(nil)
	flag.Parse()

	config, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("load in-cluster configuration: %v", err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		klog.Fatalf("create dynamic Kubernetes client: %v", err)
	}
	registry := &volumeapi.Registry{Client: dynamicClient}

	nodeService := &nodecsi.Service{
		NodeName:   *nodeName,
		HostRoot:   *hostRoot,
		TargetRoot: *targetRoot,
		Binder:     shiftmount.NewBinder(),
		Volumes:    registry,
		Pools:      registry,
	}
	identityService := &identity.Service{Version: version}

	klog.Infof("starting ShiftPV node plugin %s on %s", version, *nodeName)
	if err := csiserver.Serve(*endpoint, func(server *grpc.Server) {
		csi.RegisterIdentityServer(server, identityService)
		csi.RegisterNodeServer(server, nodeService)
	}); err != nil {
		klog.Fatalf("serve CSI node plugin: %v", err)
	}
}
