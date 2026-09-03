package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	uninstallcheck "github.com/cagojeiger/ShiftPV/src/lifecycle/uninstall"
)

func main() {
	storageClassName := flag.String("storage-class", "shiftpv", "ShiftPV StorageClass name")
	permitNamespace := flag.String("permit-namespace", os.Getenv("POD_NAMESPACE"), "namespace for the uninstall permit ConfigMap")
	permitName := flag.String("permit-name", "shiftpv-uninstall-permit", "uninstall permit ConfigMap name")
	validationWebhook := flag.String("validation-webhook", "shiftpv-lifecycle", "lifecycle ValidatingWebhookConfiguration name")
	timeout := flag.Duration("timeout", 45*time.Second, "maximum Kubernetes inspection time")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	config, err := rest.InClusterConfig()
	if err != nil {
		deny("load in-cluster configuration", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		deny("create Kubernetes client", err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		deny("create dynamic Kubernetes client", err)
	}

	checker := &uninstallcheck.Checker{
		Client:           client,
		Volumes:          &volumeapi.Registry{Client: dynamicClient},
		StorageClassName: *storageClassName,
	}
	permit := &uninstallcheck.PermitStore{Client: client, Namespace: *permitNamespace, Name: *permitName, CSIDriver: uninstallcheck.DriverName}
	if err := run(ctx, checker, permit, *validationWebhook); err != nil {
		deny("quiesce and inspect ShiftPV dependencies", err)
	}
	fmt.Println("ShiftPV uninstall allowed: provisioning is quiesced and no dependent PV, PVC, Volume, or active Move exists")
}

func run(ctx context.Context, checker *uninstallcheck.Checker, permit *uninstallcheck.PermitStore, validationWebhook string) (resultErr error) {
	attempt, err := permit.BeginQuiesce(ctx)
	if err != nil {
		return err
	}
	completed := false
	defer func() {
		if completed {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := permit.Cancel(rollbackCtx, attempt); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("cancel uninstall quiesce: %w", err))
		}
	}()

	if err := permit.WaitForQuiesced(ctx, attempt); err != nil {
		return fmt.Errorf("wait for controller quiesce: %w", err)
	}
	report, err := checker.Check(ctx)
	if err != nil {
		return fmt.Errorf("inspect ShiftPV dependencies: %w", err)
	}
	if !report.Safe() {
		return fmt.Errorf("dependent storage resources still exist:\n%s", formatBlockers(report))
	}
	if err := permit.DisableValidation(ctx, validationWebhook); err != nil {
		return fmt.Errorf("disable lifecycle validation for teardown: %w", err)
	}
	if err := permit.Grant(ctx, attempt); err != nil {
		return fmt.Errorf("grant protected resource deletion: %w", err)
	}
	completed = true
	return nil
}

func formatBlockers(report uninstallcheck.Report) string {
	message := ""
	for _, blocker := range report.Blockers {
		name := blocker.Name
		if blocker.Namespace != "" {
			name = blocker.Namespace + "/" + name
		}
		message += fmt.Sprintf("- %s %s: %s\n", blocker.Kind, name, blocker.Reason)
	}
	return message + "Migrate or remove every dependency before uninstalling. Emergency bypass requires deleting the lifecycle ValidatingWebhookConfiguration before helm uninstall --no-hooks."
}

func deny(operation string, err error) {
	fmt.Fprintf(os.Stderr, "ShiftPV uninstall denied: %s: %v\n", operation, err)
	os.Exit(1)
}
