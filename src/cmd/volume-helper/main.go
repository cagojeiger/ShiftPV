package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/cleanupapi"
	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/node/ownership"
	"github.com/cagojeiger/ShiftPV/src/volume"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: shiftpv-volume-helper create|serve-source|copy|promote|verify-owner|cleanup [flags]")
	}
	switch os.Args[1] {
	case "create":
		return runCreate(os.Args[2:])
	case "cleanup":
		return runCleanup(os.Args[2:])
	case "copy":
		return runMoveCopy(os.Args[2:])
	case "serve-source":
		return runServeSource(os.Args[2:])
	case "promote":
		return runMovePromote(os.Args[2:])
	case "verify-owner":
		return runVerifyOwner(os.Args[2:])
	default:
		return fmt.Errorf("unknown volume helper action %q", os.Args[1])
	}
}

func runServeSource(arguments []string) error {
	options, err := parseMoveOptions("serve-source", arguments, false)
	if err != nil {
		return err
	}
	dynamicClient, client, err := inClusterClients()
	if err != nil {
		return err
	}
	registry := &volumeapi.Registry{Client: dynamicClient}
	move, err := registry.GetMove(context.Background(), options.moveName)
	if err != nil || move.UID != options.moveUID || move.Status.SourceCopy == nil || move.Status.CopyOperationID != options.operationID {
		return fmt.Errorf("source service intent identity changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
	}
	identity := *move.Status.SourceCopy
	authority := sourceAuthority(client, registry, options, identity)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := authority(ctx); err != nil {
		return err
	}
	if err := ownership.VerifyServingPath(options.root, identity); err != nil {
		return fmt.Errorf("verify source copy: %w", err)
	}
	if err := authority(ctx); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "rsync", "--daemon", "--no-detach", "--config=/config/rsyncd.conf")
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

func sourceAuthority(client kubernetes.Interface, registry *volumeapi.Registry, options moveOptions, identity volume.CopyIdentity) func(context.Context) error {
	return func(ctx context.Context) error {
		move, err := registry.GetMove(ctx, options.moveName)
		if err != nil || move.UID != options.moveUID || move.Status.SourceCopy == nil || *move.Status.SourceCopy != identity ||
			move.Status.CopyOperationID != options.operationID || (move.Status.Phase != "WaitingForCapacity" && move.Status.Phase != "Copying") {
			return fmt.Errorf("source service authority changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
		}
		installationID, err := registry.InstallationID(ctx)
		if err != nil || installationID != identity.InstallationID {
			return fmt.Errorf("source installation authority changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
		}
		pool, err := registry.PoolForNode(ctx, identity.NodeName)
		if err != nil || pool.Name != identity.PoolName || pool.UID != identity.PoolUID {
			return fmt.Errorf("source Pool authority changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
		}
		state, err := registry.Get(ctx, identity.VolumeID)
		if err != nil || state.UID != identity.VolumeUID || state.Phase != volumeapi.PhaseMoving || state.ActiveMove != move.Name ||
			state.OwnerNode != identity.NodeName || state.CurrentCopy == nil || *state.CurrentCopy != identity || containsString(state.PublishedNodes, identity.NodeName) {
			return fmt.Errorf("source volume authority changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
		}
		pod, err := client.CoreV1().Pods(options.namespace).Get(ctx, os.Getenv("POD_NAME"), metav1.GetOptions{})
		if err != nil || pod.UID == "" || pod.DeletionTimestamp != nil || pod.Spec.NodeName != identity.NodeName || pod.Labels["shiftpv.io/move-uid"] != move.UID {
			return fmt.Errorf("source executor authority changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
		}
		for _, owner := range pod.OwnerReferences {
			if owner.Controller != nil && *owner.Controller && owner.APIVersion == "shiftpv.io/v1alpha1" && owner.Kind == "ShiftPVMove" && owner.Name == move.Name && string(owner.UID) == move.UID {
				return nil
			}
		}
		return fmt.Errorf("source executor is not owned by the current Move: %w", volumeapi.ErrStateConflict)
	}
}

type moveOptions struct {
	moveName, moveUID, operationID, namespace, sourceService, passwordFile, root string
}

func parseMoveOptions(action string, arguments []string, copyAction bool) (moveOptions, error) {
	flags := flag.NewFlagSet(action, flag.ContinueOnError)
	var options moveOptions
	flags.StringVar(&options.moveName, "move-name", "", "ShiftPVMove name")
	flags.StringVar(&options.moveUID, "move-uid", "", "ShiftPVMove UID")
	flags.StringVar(&options.operationID, "operation-id", "", "move operation identity")
	flags.StringVar(&options.namespace, "namespace", "", "helper Pod namespace")
	flags.StringVar(&options.root, "pool-root", "/pool", "mounted Pool root")
	if copyAction {
		flags.StringVar(&options.sourceService, "source-service", "", "rsync source Service")
		flags.StringVar(&options.passwordFile, "password-file", "", "rsync password file")
	}
	if err := flags.Parse(arguments); err != nil {
		return moveOptions{}, err
	}
	if !volume.ValidObjectName(options.moveName) || !volume.ValidIdentityToken(options.moveUID) || !volume.ValidIdentityToken(options.operationID) || options.namespace == "" || os.Getenv("POD_NAME") == "" {
		return moveOptions{}, fmt.Errorf("move helper identity is incomplete")
	}
	if copyAction && (len(utilvalidation.IsDNS1123Label(options.sourceService)) != 0 || options.passwordFile == "") {
		return moveOptions{}, fmt.Errorf("copy source is invalid")
	}
	return options, nil
}

func runMoveCopy(arguments []string) error {
	options, err := parseMoveOptions("copy", arguments, true)
	if err != nil {
		return err
	}
	dynamicClient, client, err := inClusterClients()
	if err != nil {
		return err
	}
	registry := &volumeapi.Registry{Client: dynamicClient}
	move, err := registry.GetMove(context.Background(), options.moveName)
	if err != nil || move.UID != options.moveUID || move.Status.IncomingCopy == nil || move.Status.CopyOperationID != options.operationID {
		return fmt.Errorf("copy intent identity changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
	}
	identity := *move.Status.IncomingCopy
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	authority := moveAuthority(client, registry, options, "copy", identity)
	return ownership.PopulateIncoming(ctx, options.root, identity, options.operationID, authority, func(copyCtx context.Context, target string) error {
		password, err := os.ReadFile(options.passwordFile)
		if err != nil {
			return fmt.Errorf("read rsync password: %w", err)
		}
		environment := append(os.Environ(), "RSYNC_PASSWORD="+strings.TrimSpace(string(password)))
		source := "rsync://shiftpv@" + options.sourceService + "/data/"
		for _, arguments := range [][]string{
			{"-a", "--delete", source, target + "/"},
			{"-a", "--checksum", "--delete", "--dry-run", "--itemize-changes", source, target + "/"},
		} {
			command := exec.CommandContext(copyCtx, "rsync", arguments...)
			command.Env = environment
			output, commandErr := command.CombinedOutput()
			if commandErr != nil {
				return fmt.Errorf("rsync copy validation: %w: %s", commandErr, strings.TrimSpace(string(output)))
			}
			if len(arguments) > 1 && arguments[1] == "--checksum" && len(output) != 0 {
				return fmt.Errorf("rsync checksum validation reported differences")
			}
		}
		return nil
	})
}

func runMovePromote(arguments []string) error {
	options, err := parseMoveOptions("promote", arguments, false)
	if err != nil {
		return err
	}
	dynamicClient, client, err := inClusterClients()
	if err != nil {
		return err
	}
	registry := &volumeapi.Registry{Client: dynamicClient}
	move, err := registry.GetMove(context.Background(), options.moveName)
	if err != nil || move.UID != options.moveUID || move.Status.IncomingCopy == nil || move.Status.DestinationCopy == nil || move.Status.PromotionOperationID != options.operationID {
		return fmt.Errorf("promotion intent identity changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
	}
	incoming, destination := *move.Status.IncomingCopy, *move.Status.DestinationCopy
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return ownership.PromoteIncoming(ctx, options.root, incoming, destination, options.operationID, moveAuthority(client, registry, options, "promote", incoming))
}

func runVerifyOwner(arguments []string) error {
	options, err := parseMoveOptions("verify-owner", arguments, false)
	if err != nil {
		return err
	}
	dynamicClient, client, err := inClusterClients()
	if err != nil {
		return err
	}
	registry := &volumeapi.Registry{Client: dynamicClient}
	move, err := registry.GetMove(context.Background(), options.moveName)
	if err != nil || move.UID != options.moveUID {
		return fmt.Errorf("recovery intent identity changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
	}
	state, err := registry.Get(context.Background(), move.Spec.VolumeID)
	if err != nil || state.CurrentCopy == nil {
		return fmt.Errorf("recovery owner identity is unavailable: %w", errors.Join(err, volumeapi.ErrStateConflict))
	}
	identity := *state.CurrentCopy
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	authority := func(checkCtx context.Context) error {
		currentMove, err := registry.GetMove(checkCtx, options.moveName)
		if err != nil || currentMove.UID != options.moveUID || currentMove.Status.Phase != "Blocked" || currentMove.Status.RecoveryPhase != "Verifying" || currentMove.Status.RecoveryOwner != identity.NodeName {
			return fmt.Errorf("recovery authority changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
		}
		current, err := registry.Get(checkCtx, currentMove.Spec.VolumeID)
		if err != nil || current.UID != identity.VolumeUID || current.Phase != volumeapi.PhaseBlocked || current.ActiveMove != currentMove.Name || current.OwnerNode != identity.NodeName || current.CurrentCopy == nil || *current.CurrentCopy != identity {
			return fmt.Errorf("recovery volume authority changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
		}
		for _, node := range current.PublishedNodes {
			if node != current.OwnerNode {
				return fmt.Errorf("foreign publication remains")
			}
		}
		if err := verifyOwnedMoveJob(checkCtx, client, options, currentMove, identity.NodeName); err != nil {
			return err
		}
		installationID, err := registry.InstallationID(checkCtx)
		if err != nil || installationID != identity.InstallationID {
			return fmt.Errorf("recovery installation authority changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
		}
		pool, err := registry.PoolForNode(checkCtx, identity.NodeName)
		if err != nil || pool.Name != identity.PoolName || pool.UID != identity.PoolUID {
			return fmt.Errorf("recovery Pool authority changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
		}
		return nil
	}
	return ownership.WithExistingLock(ctx, options.root, ownership.PoolIdentity{InstallationID: identity.InstallationID, PoolUID: identity.PoolUID}, identity.VolumeID, func(store *ownership.Store) error {
		if err := authority(ctx); err != nil {
			return err
		}
		if err := store.VerifyServing(identity); err != nil {
			return err
		}
		return authority(ctx)
	})
}

func inClusterClients() (dynamic.Interface, kubernetes.Interface, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("load in-cluster configuration: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("create dynamic Kubernetes client: %w", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	return dynamicClient, client, nil
}

func moveAuthority(client kubernetes.Interface, registry *volumeapi.Registry, options moveOptions, action string, target volume.CopyIdentity) func(context.Context) error {
	return func(ctx context.Context) error {
		move, err := registry.GetMove(ctx, options.moveName)
		if err != nil || move.UID != options.moveUID || move.Status.SourceCopy == nil || move.Status.IncomingCopy == nil {
			return fmt.Errorf("move authority changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
		}
		allowedPhase := (action == "copy" && (move.Status.Phase == "WaitingForCapacity" || move.Status.Phase == "Copying")) ||
			(action == "promote" && (move.Status.Phase == "Copying" || move.Status.Phase == "Promoting"))
		operationMatches := action == "copy" && move.Status.CopyJobName != "" && move.Status.CopyOperationID == options.operationID && *move.Status.IncomingCopy == target ||
			action == "promote" && move.Status.PromotionJobName != "" && move.Status.PromotionOperationID == options.operationID && *move.Status.IncomingCopy == target && move.Status.DestinationCopy != nil
		if !allowedPhase || !operationMatches {
			return fmt.Errorf("move operation is no longer authorized")
		}
		if err := verifyMoveExecutor(ctx, client, options, move, action, target.NodeName); err != nil {
			return err
		}
		installationID, err := registry.InstallationID(ctx)
		if err != nil || installationID != target.InstallationID {
			return fmt.Errorf("installation authority changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
		}
		pool, err := registry.PoolForNode(ctx, target.NodeName)
		if err != nil || pool.Name != target.PoolName || pool.UID != target.PoolUID {
			return fmt.Errorf("Pool authority changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
		}
		state, err := registry.Get(ctx, move.Spec.VolumeID)
		if err != nil || state.UID != target.VolumeUID || state.Phase != volumeapi.PhaseMoving || state.ActiveMove != move.Name || state.OwnerNode != move.Spec.SourceNode ||
			state.CurrentCopy == nil || *state.CurrentCopy != *move.Status.SourceCopy || containsString(state.PublishedNodes, move.Spec.SourceNode) {
			return fmt.Errorf("source volume authority changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
		}
		return nil
	}
}

func verifyMoveExecutor(ctx context.Context, client kubernetes.Interface, options moveOptions, move volumeapi.Move, action, nodeName string) error {
	job, err := ownedMoveJob(ctx, client, options, move, nodeName)
	if err != nil {
		return err
	}
	wantedJob := move.Status.CopyJobName
	if action == "promote" {
		wantedJob = move.Status.PromotionJobName
	}
	if job.Name != wantedJob {
		return fmt.Errorf("move executor Job differs from the durable operation")
	}
	return nil
}

func verifyOwnedMoveJob(ctx context.Context, client kubernetes.Interface, options moveOptions, move volumeapi.Move, nodeName string) error {
	_, err := ownedMoveJob(ctx, client, options, move, nodeName)
	return err
}

func ownedMoveJob(ctx context.Context, client kubernetes.Interface, options moveOptions, move volumeapi.Move, nodeName string) (*batchv1.Job, error) {
	pod, err := client.CoreV1().Pods(options.namespace).Get(ctx, os.Getenv("POD_NAME"), metav1.GetOptions{})
	if err != nil || pod.DeletionTimestamp != nil || pod.Spec.NodeName != nodeName {
		return nil, fmt.Errorf("read move executor Pod: %w", err)
	}
	jobName, jobUID := "", ""
	for _, owner := range pod.OwnerReferences {
		if owner.Controller != nil && *owner.Controller && owner.Kind == "Job" {
			jobName, jobUID = owner.Name, string(owner.UID)
			break
		}
	}
	job, err := client.BatchV1().Jobs(options.namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil || jobUID == "" || string(job.UID) != jobUID || job.DeletionTimestamp != nil || job.Labels["shiftpv.io/move-uid"] != move.UID {
		return nil, fmt.Errorf("move executor is not authorized: %w", errors.Join(err, volumeapi.ErrStateConflict))
	}
	for _, owner := range job.OwnerReferences {
		if owner.Controller != nil && *owner.Controller && owner.Kind == "ShiftPVMove" && owner.Name == move.Name && string(owner.UID) == move.UID {
			return job, nil
		}
	}
	return nil, fmt.Errorf("move Job is not owned by the exact ShiftPVMove")
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func runCreate(arguments []string) error {
	flags := flag.NewFlagSet("create", flag.ContinueOnError)
	var identity volume.CopyIdentity
	operationID := flags.String("operation-id", "", "volume creation operation identity")
	flags.StringVar(&identity.InstallationID, "installation-id", "", "Kubernetes cluster identity")
	flags.StringVar(&identity.PoolName, "pool-name", "", "ShiftPVPool name")
	flags.StringVar(&identity.PoolUID, "pool-uid", "", "ShiftPVPool UID")
	flags.StringVar(&identity.VolumeID, "volume-id", "", "ShiftPV volume ID")
	flags.StringVar(&identity.VolumeUID, "volume-uid", "", "ShiftPVVolume UID")
	flags.StringVar(&identity.CopyID, "copy-id", "", "copy identity")
	flags.StringVar(&identity.NodeName, "node-name", "", "Kubernetes node name")
	root := flags.String("pool-root", "/pool", "mounted Pool root")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	identity.Role = volume.RoleServing
	if err := identity.Validate(); err != nil {
		return err
	}
	if !volume.ValidIdentityToken(*operationID) {
		return fmt.Errorf("volume creation operation identity is invalid")
	}
	expectedOperationID, err := volumeapi.CreationOperationID(identity.VolumeUID)
	if err != nil || *operationID != expectedOperationID {
		return fmt.Errorf("volume creation operation identity does not match the volume UID")
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("load in-cluster configuration: %w", err)
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}
	registry := &volumeapi.Registry{Client: client}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	authority := func(checkCtx context.Context) error {
		installationID, err := registry.InstallationID(checkCtx)
		if err != nil || installationID != identity.InstallationID {
			return fmt.Errorf("installation authority changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
		}
		pool, err := registry.PoolForNode(checkCtx, identity.NodeName)
		if err != nil || pool.Name != identity.PoolName || pool.UID != identity.PoolUID {
			return fmt.Errorf("Pool authority changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
		}
		state, err := registry.Get(checkCtx, identity.VolumeID)
		if err != nil || state.UID != identity.VolumeUID || state.CurrentCopy == nil || *state.CurrentCopy != identity ||
			state.CreationOperationID != *operationID ||
			(state.Phase != volumeapi.PhasePending && state.Phase != volumeapi.PhaseReady) {
			return fmt.Errorf("volume creation authority changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
		}
		return nil
	}
	return ownership.PrepareServing(ctx, *root, identity, authority)
}

func runCleanup(arguments []string) error {
	flags := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	cleanupName := flags.String("cleanup-name", "", "ShiftPVCleanup name")
	cleanupUID := flags.String("cleanup-uid", "", "ShiftPVCleanup UID")
	operationID := flags.String("operation-id", "", "cleanup operation identity")
	namespace := flags.String("namespace", "", "helper Pod namespace")
	root := flags.String("pool-root", "/pool", "mounted Pool root")
	poolReadinessStaleAfter := flags.Duration("pool-readiness-stale-after", volumeapi.DefaultPoolReadinessStaleAfter, "maximum age of the Pool readiness and inventory observation")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *cleanupName == "" || *cleanupUID == "" || !volume.ValidIdentityToken(*operationID) || *namespace == "" || os.Getenv("POD_NAME") == "" || *poolReadinessStaleAfter <= 0 {
		return fmt.Errorf("cleanup helper identity is incomplete")
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("load in-cluster configuration: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create dynamic Kubernetes client: %w", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}
	cleanups := &cleanupapi.Store{Client: dynamicClient}
	registry := &volumeapi.Registry{Client: dynamicClient}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	approved, err := cleanups.Get(ctx, *cleanupName)
	if err != nil || approved.UID != *cleanupUID || approved.Spec.OperationID != *operationID || !approved.Spec.Approved {
		return fmt.Errorf("cleanup intent identity changed: %w", errors.Join(err, cleanupapi.ErrConflict))
	}
	pod, err := client.CoreV1().Pods(*namespace).Get(ctx, os.Getenv("POD_NAME"), metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read cleanup executor Pod: %w", err)
	}
	jobUID := ""
	for _, owner := range pod.OwnerReferences {
		if owner.Controller != nil && *owner.Controller && owner.Kind == "Job" {
			jobUID = string(owner.UID)
			break
		}
	}
	if jobUID == "" || approved.Status.Phase != cleanupapi.PhaseRunning || approved.Status.Executor == nil ||
		approved.Status.Executor.JobUID != jobUID || approved.Status.Executor.NodeName != approved.Spec.Target.NodeName {
		return fmt.Errorf("cleanup executor is not authorized")
	}
	authority := func(checkCtx context.Context) error {
		current, err := cleanups.Get(checkCtx, approved.Name)
		if err != nil || current.UID != approved.UID || current.Spec != approved.Spec || current.Status.Phase != cleanupapi.PhaseRunning ||
			current.Status.Executor == nil || current.Status.Executor.JobUID != jobUID {
			return fmt.Errorf("cleanup intent changed: %w", errors.Join(err, cleanupapi.ErrConflict))
		}
		installationID, err := registry.InstallationID(checkCtx)
		if err != nil || installationID != approved.Spec.Target.InstallationID {
			return fmt.Errorf("installation authority changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
		}
		pool, err := registry.PoolForIdentity(checkCtx, approved.Spec.Target.PoolName, approved.Spec.Target.PoolUID, approved.Spec.Target.NodeName)
		if err != nil || pool.Name != approved.Spec.Target.PoolName || pool.UID != approved.Spec.Target.PoolUID {
			return fmt.Errorf("Pool authority changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
		}
		return verifyCleanupAuthority(checkCtx, client, registry, *namespace, approved, *poolReadinessStaleAfter)
	}
	localReceipt, digest, err := ownership.Reclaim(ctx, *root, approved.Spec.Target, approved.Spec.OperationID, authority)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	receipt := &cleanupapi.Receipt{
		OperationID: approved.Spec.OperationID, ExecutorUID: jobUID, ObservedAt: now,
		Retired: localReceipt.Retired, Purged: localReceipt.Purged, LocalReceiptDigest: digest,
	}
	return cleanups.UpdateStatus(ctx, approved.Name, approved.UID, cleanupapi.Status{
		Phase: cleanupapi.PhaseVerifying, Executor: approved.Status.Executor, Receipt: receipt,
	})
}

func verifyCleanupAuthority(ctx context.Context, client kubernetes.Interface, registry *volumeapi.Registry, namespace string, cleanup cleanupapi.Cleanup, freshness time.Duration) error {
	switch cleanup.Spec.Authority.Kind {
	case "ShiftPVVolume":
		state, err := registry.Get(ctx, cleanup.Spec.Authority.Name)
		if err != nil || state.UID != cleanup.Spec.Authority.UID || state.CurrentCopy == nil || *state.CurrentCopy != cleanup.Spec.Target ||
			state.Phase != volumeapi.PhaseDeleting || state.DeletionOperationID != cleanup.Spec.OperationID || state.ActiveMove != "" || len(state.PublishedNodes) != 0 {
			return fmt.Errorf("volume cleanup authority changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
		}
		return nil
	case "ShiftPVMove":
		move, err := registry.GetMove(ctx, cleanup.Spec.Authority.Name)
		if err != nil || move.UID != cleanup.Spec.Authority.UID || move.Status.SourceCopy == nil || *move.Status.SourceCopy != cleanup.Spec.Target ||
			move.Status.DestinationCopy == nil || (move.Status.Phase != "WaitingForDestinationPublish" && move.Status.Phase != "CleaningSource") {
			return fmt.Errorf("move cleanup authority changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
		}
		state, err := registry.Get(ctx, move.Spec.VolumeID)
		if err != nil || state.UID != cleanup.Spec.Target.VolumeUID || state.Phase != volumeapi.PhaseReady || state.ActiveMove != move.Name ||
			state.OwnerNode != move.Status.DestinationNode || state.CurrentCopy == nil || *state.CurrentCopy != *move.Status.DestinationCopy ||
			!containsString(state.PublishedNodes, move.Status.DestinationNode) || containsString(state.PublishedNodes, move.Spec.SourceNode) {
			return fmt.Errorf("committed move cleanup authority changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
		}
		return nil
	case "Namespace":
		return verifyOrphanCleanupAuthority(ctx, client, registry, namespace, cleanup, freshness)
	default:
		return fmt.Errorf("cleanup authority %q is not implemented", cleanup.Spec.Authority.Kind)
	}
}

func verifyOrphanCleanupAuthority(ctx context.Context, client kubernetes.Interface, registry *volumeapi.Registry, namespace string, cleanup cleanupapi.Cleanup, poolReadinessStaleAfter time.Duration) error {
	if client == nil || namespace == "" || cleanup.Spec.Reason != "OrphanReclaim" || !cleanup.Spec.Approved ||
		cleanup.Spec.Authority.Name != "kube-system" || cleanup.Spec.Authority.UID != cleanup.Spec.Target.InstallationID {
		return fmt.Errorf("orphan cleanup authority is incomplete: %w", volumeapi.ErrStateConflict)
	}
	installation, err := client.CoreV1().Namespaces().Get(ctx, cleanup.Spec.Authority.Name, metav1.GetOptions{})
	if err != nil || string(installation.UID) != cleanup.Spec.Authority.UID {
		return fmt.Errorf("installation namespace authority changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
	}
	persistentVolumes, err := client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list PersistentVolumes before orphan cleanup: %w", err)
	}
	volumes, err := registry.ListVolumes(ctx)
	if err != nil {
		return err
	}
	authority := volumeapi.ClassifyCopyAuthority(volumes, cleanup.Spec.Target)
	if authority == volumeapi.CopyAuthorityCurrent || authority == volumeapi.CopyAuthorityUncertain {
		return fmt.Errorf("ShiftPVVolume still has orphan authority: %w", volumeapi.ErrStateConflict)
	}
	if authority != volumeapi.CopyAuthoritySuperseded {
		for index := range persistentVolumes.Items {
			persistentVolume := &persistentVolumes.Items[index]
			if persistentVolume.Spec.CSI != nil && persistentVolume.Spec.CSI.Driver == "csi.shiftpv.io" && persistentVolume.Spec.CSI.VolumeHandle == cleanup.Spec.Target.VolumeID {
				return fmt.Errorf("PersistentVolume %q still references orphan target: %w", persistentVolume.Name, volumeapi.ErrStateConflict)
			}
		}
	}
	moves, err := registry.ListMoves(ctx)
	if err != nil {
		return err
	}
	for _, move := range moves {
		if move.Status.Phase == "Succeeded" || move.Status.RecoveryPhase == "Recovered" {
			continue
		}
		for _, candidate := range []*volume.CopyIdentity{move.Status.SourceCopy, move.Status.IncomingCopy, move.Status.DestinationCopy} {
			if candidate != nil && *candidate == cleanup.Spec.Target {
				return fmt.Errorf("ShiftPVMove %q still has orphan authority: %w", move.Name, volumeapi.ErrStateConflict)
			}
		}
	}
	pool, err := registry.PoolForIdentity(ctx, cleanup.Spec.Target.PoolName, cleanup.Spec.Target.PoolUID, cleanup.Spec.Target.NodeName)
	if err != nil || pool.Name != cleanup.Spec.Target.PoolName || pool.UID != cleanup.Spec.Target.PoolUID {
		return fmt.Errorf("orphan Pool authority changed: %w", errors.Join(err, volumeapi.ErrStateConflict))
	}
	now := time.Now().UTC()
	if ready, reason := pool.CleanupReadyAt(now, poolReadinessStaleAfter); !ready {
		return fmt.Errorf("orphan Pool is not available for cleanup (%s): %w", reason, volumeapi.ErrStateConflict)
	}
	if pool.Status.Inventory == nil || !pool.Status.Inventory.Valid || pool.Status.Inventory.ObservedAt.IsZero() ||
		now.Before(pool.Status.Inventory.ObservedAt.Time) || now.Sub(pool.Status.Inventory.ObservedAt.Time) > poolReadinessStaleAfter {
		return fmt.Errorf("orphan inventory is unavailable or stale: %w", volumeapi.ErrStateConflict)
	}
	observed := false
	for _, candidate := range pool.Status.Inventory.Copies {
		if candidate.Identity == nil || *candidate.Identity != cleanup.Spec.Target {
			continue
		}
		observed = true
		if !candidate.Present || candidate.Published || candidate.Problem != "" {
			return fmt.Errorf("orphan copy is absent, published, or invalid: %w", volumeapi.ErrStateConflict)
		}
	}
	if !observed {
		return fmt.Errorf("orphan copy is not present in the exact Pool inventory: %w", volumeapi.ErrStateConflict)
	}
	reservationVolumeUID := cleanup.Spec.Target.VolumeUID
	if authority == volumeapi.CopyAuthoritySuperseded {
		liveVolume, exists := volumes[cleanup.Spec.Target.VolumeID]
		if !exists {
			return fmt.Errorf("active volume identity is unavailable: %w", volumeapi.ErrStateConflict)
		}
		reservationVolumeUID = liveVolume.UID
	}
	return verifyOrphanReservation(ctx, client, namespace, cleanup, authority, reservationVolumeUID)
}

func verifyOrphanReservation(ctx context.Context, client kubernetes.Interface, namespace string, cleanup cleanupapi.Cleanup, authority volumeapi.CopyAuthority, reservationVolumeUID string) error {
	reservation, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, cleanup.Spec.Target.VolumeID, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if authority == volumeapi.CopyAuthoritySuperseded {
			return fmt.Errorf("active volume capacity reservation is missing: %w", volumeapi.ErrStateConflict)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read orphan capacity reservation: %w", err)
	}
	if reservation.Labels["app.kubernetes.io/name"] != "shiftpv" || reservation.Labels["app.kubernetes.io/component"] != "volume-reservation" ||
		reservation.Data["volumeID"] != cleanup.Spec.Target.VolumeID || reservation.Data["volumeUID"] != reservationVolumeUID {
		return fmt.Errorf("orphan capacity reservation identity changed: %w", volumeapi.ErrStateConflict)
	}
	if authority == volumeapi.CopyAuthoritySuperseded {
		if cleanup.Spec.ReservationUID != "" {
			return fmt.Errorf("cleanup owns an active volume capacity reservation: %w", volumeapi.ErrStateConflict)
		}
		return nil
	}
	if cleanup.Spec.ReservationUID == "" || string(reservation.UID) != cleanup.Spec.ReservationUID {
		return fmt.Errorf("orphan capacity reservation identity changed: %w", volumeapi.ErrStateConflict)
	}
	return nil
}
