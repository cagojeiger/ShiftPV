package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/cagojeiger/ShiftPV/src/csi/identity"
	nodecsi "github.com/cagojeiger/ShiftPV/src/csi/node"
	csiserver "github.com/cagojeiger/ShiftPV/src/csi/server"
	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/metrics"
	shiftmount "github.com/cagojeiger/ShiftPV/src/node/mount"
	nodeobservation "github.com/cagojeiger/ShiftPV/src/node/observation"
	poolreadiness "github.com/cagojeiger/ShiftPV/src/pool/readiness"
)

var version = "dev"

func main() {
	var (
		endpoint              = flag.String("endpoint", "unix:///csi/csi.sock", "CSI Unix socket endpoint")
		nodeName              = flag.String("node-name", os.Getenv("NODE_NAME"), "Kubernetes node name")
		hostRoot              = flag.String("host-root", "/host", "host filesystem root mounted into the node plugin")
		targetRoot            = flag.String("target-root", "/var/lib/kubelet/pods", "allowed kubelet publish target root")
		poolReadinessInterval = flag.Duration("pool-readiness-interval", time.Minute, "interval between local Pool mount and write probes")
		metricsAddress        = flag.String("metrics-listen-address", "", "metrics HTTP address; empty disables observation")
	)
	klog.InitFlags(nil)
	flag.Parse()
	if *poolReadinessInterval <= 0 {
		klog.Fatalf("pool readiness interval must be positive")
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("load in-cluster configuration: %v", err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		klog.Fatalf("create dynamic Kubernetes client: %v", err)
	}
	registry := &volumeapi.Registry{Client: dynamicClient}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	binder := shiftmount.NewBinder()
	nodeService := &nodecsi.Service{
		NodeName:   *nodeName,
		HostRoot:   *hostRoot,
		TargetRoot: *targetRoot,
		Binder:     binder,
		Volumes:    registry,
		Pools:      registry,
	}
	identityService := &identity.Service{Version: version}
	readinessReconciler := &poolreadiness.Reconciler{
		NodeName: *nodeName, Pools: registry, Inspector: poolreadiness.NewProbe(*hostRoot), Interval: *poolReadinessInterval,
	}
	inventoryScanner := &nodeobservation.Scanner{
		HostRoot: *hostRoot, TargetRoot: *targetRoot, Installation: registry, Publications: binder, Limit: 256,
	}
	readinessReconciler.Inventory = inventoryScanner.Scan
	readinessReconciler.Release = inventoryScanner.ReleasePool
	var exporter *metrics.Exporter
	if *metricsAddress != "" {
		exporter = metrics.New("filesystem")
		exporter.Start(ctx, *metricsAddress)
		readinessReconciler.Observe = exporter.ObservePool
	}

	klog.Infof("starting ShiftPV node plugin %s on %s", version, *nodeName)
	errCh := make(chan error, 2)
	go func() { errCh <- readinessReconciler.Run(ctx) }()
	go func() {
		errCh <- csiserver.ServeContext(ctx, *endpoint, func(server *grpc.Server) {
			csi.RegisterIdentityServer(server, identityService)
			csi.RegisterNodeServer(server, nodeService)
		}, exporter.ServerOptions()...)
	}()
	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil {
			klog.Errorf("ShiftPV node component stopped: %v", err)
		}
		stop()
	}
}
