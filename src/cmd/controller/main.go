package main

import (
	"flag"
	"os"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	controllercsi "github.com/cagojeiger/ShiftPV/src/csi/controller"
	"github.com/cagojeiger/ShiftPV/src/csi/identity"
	csiserver "github.com/cagojeiger/ShiftPV/src/csi/server"
	"github.com/cagojeiger/ShiftPV/src/kubernetes/helperpod"
)

var version = "dev"

func main() {
	var (
		endpoint            = flag.String("endpoint", "unix:///run/csi/csi.sock", "CSI Unix socket endpoint")
		namespace           = flag.String("namespace", os.Getenv("POD_NAMESPACE"), "namespace for reservations and helper Pods")
		poolRoot            = flag.String("pool-root", "/mnt/shiftpv", "host pool root mounted on storage nodes")
		helperImage         = flag.String("helper-image", "busybox:1.37", "helper Pod image")
		helperWait          = flag.Duration("helper-timeout", 2*time.Minute, "helper Pod completion timeout")
		helperCPURequest    = flag.String("helper-cpu-request", "10m", "helper Pod CPU request")
		helperMemoryRequest = flag.String("helper-memory-request", "16Mi", "helper Pod memory request")
		helperCPULimit      = flag.String("helper-cpu-limit", "100m", "helper Pod CPU limit")
		helperMemoryLimit   = flag.String("helper-memory-limit", "64Mi", "helper Pod memory limit")
	)
	klog.InitFlags(nil)
	flag.Parse()

	config, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("load in-cluster configuration: %v", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("create Kubernetes client: %v", err)
	}
	operator := &helperpod.Runner{
		Client:    client,
		Namespace: *namespace,
		PoolRoot:  *poolRoot,
		Image:     *helperImage,
		Timeout:   *helperWait,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    mustQuantity("helper CPU request", *helperCPURequest),
				corev1.ResourceMemory: mustQuantity("helper memory request", *helperMemoryRequest),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    mustQuantity("helper CPU limit", *helperCPULimit),
				corev1.ResourceMemory: mustQuantity("helper memory limit", *helperMemoryLimit),
			},
		},
	}
	controllerService := &controllercsi.Service{Client: client, Namespace: *namespace, Operator: operator}
	identityService := &identity.Service{Version: version}

	klog.Infof("starting ShiftPV controller %s", version)
	if err := csiserver.Serve(*endpoint, func(server *grpc.Server) {
		csi.RegisterIdentityServer(server, identityService)
		csi.RegisterControllerServer(server, controllerService)
	}); err != nil {
		klog.Fatalf("serve CSI controller: %v", err)
	}
}

func mustQuantity(name, value string) resource.Quantity {
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		klog.Fatalf("parse %s %q: %v", name, value, err)
	}
	return quantity
}
