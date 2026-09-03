package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	controllercsi "github.com/cagojeiger/ShiftPV/src/csi/controller"
	"github.com/cagojeiger/ShiftPV/src/csi/identity"
	csiserver "github.com/cagojeiger/ShiftPV/src/csi/server"
	"github.com/cagojeiger/ShiftPV/src/kubernetes/helperpod"
	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/mobility/admission"
	mobilitycontroller "github.com/cagojeiger/ShiftPV/src/mobility/controller"
)

var version = "dev"

func main() {
	var (
		endpoint            = flag.String("endpoint", "unix:///run/csi/csi.sock", "CSI Unix socket endpoint")
		namespace           = flag.String("namespace", os.Getenv("POD_NAMESPACE"), "namespace for reservations and helper Pods")
		helperImage         = flag.String("helper-image", "busybox:1.37", "directory helper Pod image")
		helperWait          = flag.Duration("helper-timeout", 2*time.Minute, "helper Pod completion timeout")
		helperCPURequest    = flag.String("helper-cpu-request", "10m", "helper Pod CPU request")
		helperMemoryRequest = flag.String("helper-memory-request", "16Mi", "helper Pod memory request")
		helperCPULimit      = flag.String("helper-cpu-limit", "100m", "helper Pod CPU limit")
		helperMemoryLimit   = flag.String("helper-memory-limit", "64Mi", "helper Pod memory limit")
		mobilityEnabled     = flag.Bool("mobility-enabled", true, "run the automatic cordon mobility reconciler and admission webhook")
		mobilityInterval    = flag.Duration("mobility-interval", 2*time.Second, "mobility reconciliation interval")
		mobilityImage       = flag.String("mobility-helper-image", "shiftpv-rsync-helper:dev", "rsync mobility helper image")
		webhookAddress      = flag.String("webhook-listen-address", ":9443", "mobility admission HTTPS listen address")
		webhookCert         = flag.String("webhook-tls-cert-file", "/var/run/shiftpv-webhook/tls.crt", "mobility admission TLS certificate")
		webhookKey          = flag.String("webhook-tls-key-file", "/var/run/shiftpv-webhook/tls.key", "mobility admission TLS key")
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
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		klog.Fatalf("create dynamic Kubernetes client: %v", err)
	}
	volumeRegistry := &volumeapi.Registry{Client: dynamicClient}
	operator := &helperpod.Runner{
		Client: client, Namespace: *namespace, Pools: volumeRegistry, Image: *helperImage, Timeout: *helperWait,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU: mustQuantity("helper CPU request", *helperCPURequest), corev1.ResourceMemory: mustQuantity("helper memory request", *helperMemoryRequest),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU: mustQuantity("helper CPU limit", *helperCPULimit), corev1.ResourceMemory: mustQuantity("helper memory limit", *helperMemoryLimit),
			},
		},
	}
	controllerService := &controllercsi.Service{Client: client, Namespace: *namespace, Operator: operator, Volumes: volumeRegistry}
	identityService := &identity.Service{Version: version}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 3)
	go func() {
		errCh <- csiserver.ServeContext(ctx, *endpoint, func(server *grpc.Server) {
			csi.RegisterIdentityServer(server, identityService)
			csi.RegisterControllerServer(server, controllerService)
		})
	}()

	var webhookServer *http.Server
	if *mobilityEnabled {
		reconciler := &mobilitycontroller.Reconciler{Client: client, Repository: volumeRegistry, Namespace: *namespace, HelperImage: *mobilityImage, Interval: *mobilityInterval}
		go func() { errCh <- reconciler.Run(ctx) }()
		mux := http.NewServeMux()
		mux.Handle("/mutate", &admission.Handler{Client: client, Volumes: volumeRegistry})
		mux.HandleFunc("/healthz", func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusOK) })
		webhookServer = &http.Server{Addr: *webhookAddress, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			err := webhookServer.ListenAndServeTLS(*webhookCert, *webhookKey)
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			errCh <- err
		}()
	}

	klog.Infof("starting ShiftPV controller %s", version)
	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil {
			klog.Errorf("ShiftPV controller component stopped: %v", err)
		}
		stop()
	}
	if webhookServer != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := webhookServer.Shutdown(shutdownCtx); err != nil {
			klog.Errorf("shut down mobility webhook: %v", err)
		}
	}
}

func mustQuantity(name, value string) resource.Quantity {
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		klog.Fatalf("parse %s %q: %v", name, value, err)
	}
	return quantity
}
