package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
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
	if err != nil {
		return fmt.Errorf("read source service Move: %w", err)
	}
	if move.UID != options.moveUID || move.Status.SourceCopy == nil || move.Status.CopyOperationID != options.operationID {
		return fmt.Errorf("source service intent identity changed: %w", volumeapi.ErrStateConflict)
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
		if err != nil {
			return fmt.Errorf("read source service Move: %w", err)
		}
		if move.UID != options.moveUID || move.Status.SourceCopy == nil || *move.Status.SourceCopy != identity ||
			move.Status.CopyOperationID != options.operationID || (move.Status.Phase != "WaitingForCapacity" && move.Status.Phase != "Copying") {
			return fmt.Errorf("source service authority changed: %w", volumeapi.ErrStateConflict)
		}
		installationID, err := registry.InstallationID(ctx)
		if err != nil {
			return fmt.Errorf("read source installation identity: %w", err)
		}
		if installationID != identity.InstallationID {
			return fmt.Errorf("source installation authority changed: %w", volumeapi.ErrStateConflict)
		}
		pool, err := registry.PoolForNode(ctx, identity.NodeName)
		if err != nil {
			return fmt.Errorf("read source Pool: %w", err)
		}
		if pool.Name != identity.PoolName || pool.UID != identity.PoolUID {
			return fmt.Errorf("source Pool authority changed: %w", volumeapi.ErrStateConflict)
		}
		state, err := registry.Get(ctx, identity.VolumeID)
		if err != nil {
			return fmt.Errorf("read source volume state: %w", err)
		}
		if state.UID != identity.VolumeUID || state.Phase != volumeapi.PhaseMoving || state.ActiveMove != move.Name ||
			state.OwnerNode != identity.NodeName || state.CurrentCopy == nil || *state.CurrentCopy != identity || containsString(state.PublishedNodes, identity.NodeName) {
			return fmt.Errorf("source volume authority changed: %w", volumeapi.ErrStateConflict)
		}
		pod, err := client.CoreV1().Pods(options.namespace).Get(ctx, os.Getenv("POD_NAME"), metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read source executor Pod: %w", err)
		}
		if pod.UID == "" || pod.DeletionTimestamp != nil || pod.Spec.NodeName != identity.NodeName || pod.Labels["shiftpv.io/move-uid"] != move.UID {
			return fmt.Errorf("source executor authority changed: %w", volumeapi.ErrStateConflict)
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
	if err != nil {
		return fmt.Errorf("read copy Move: %w", err)
	}
	if move.UID != options.moveUID || move.Status.IncomingCopy == nil || move.Status.CopyOperationID != options.operationID {
		return fmt.Errorf("copy intent identity changed: %w", volumeapi.ErrStateConflict)
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
		copyArguments, verifyArguments := moveCopyArguments(source, target)
		copyCommand := exec.CommandContext(copyCtx, "rsync", copyArguments...)
		copyCommand.Env = environment
		if output, commandErr := copyCommand.CombinedOutput(); commandErr != nil {
			return fmt.Errorf("rsync copy: %w: %s", commandErr, strings.TrimSpace(string(output)))
		}
		verifyCommand := exec.CommandContext(copyCtx, "rsync", verifyArguments...)
		verifyCommand.Env = environment
		output, commandErr := verifyCommand.CombinedOutput()
		if commandErr != nil {
			return fmt.Errorf("rsync checksum validation: %w: %s", commandErr, strings.TrimSpace(string(output)))
		}
		if difference := strings.TrimSpace(string(output)); difference != "" {
			return fmt.Errorf("rsync checksum validation reported differences: %s", difference)
		}
		return nil
	})
}

func moveCopyArguments(source, target string) ([]string, []string) {
	common := []string{
		"-aHAXS",
		"--numeric-ids",
		"--one-file-system",
		"--no-devices",
		"--delete",
	}
	copyArguments := append(append([]string{}, common...), "--fsync", source, target+"/")
	verifyArguments := append(append([]string{}, common...), "--checksum", "--dry-run", "--itemize-changes", source, target+"/")
	return copyArguments, verifyArguments
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
	if err != nil {
		return fmt.Errorf("read promotion Move: %w", err)
	}
	if move.UID != options.moveUID || move.Status.IncomingCopy == nil || move.Status.DestinationCopy == nil || move.Status.PromotionOperationID != options.operationID {
		return fmt.Errorf("promotion intent identity changed: %w", volumeapi.ErrStateConflict)
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
	if err != nil {
		return fmt.Errorf("read recovery Move: %w", err)
	}
	if move.UID != options.moveUID {
		return fmt.Errorf("recovery intent identity changed: %w", volumeapi.ErrStateConflict)
	}
	state, err := registry.Get(context.Background(), move.Spec.VolumeID)
	if err != nil {
		return fmt.Errorf("read recovery owner volume state: %w", err)
	}
	if state.CurrentCopy == nil {
		return fmt.Errorf("recovery owner identity is unavailable: %w", volumeapi.ErrStateConflict)
	}
	identity := *state.CurrentCopy
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	authority := func(checkCtx context.Context) error {
		currentMove, err := registry.GetMove(checkCtx, options.moveName)
		if err != nil {
			return fmt.Errorf("read recovery Move: %w", err)
		}
		if currentMove.UID != options.moveUID || currentMove.Status.Phase != "Blocked" || currentMove.Status.RecoveryPhase != "Verifying" || currentMove.Status.RecoveryOwner != identity.NodeName {
			return fmt.Errorf("recovery authority changed: %w", volumeapi.ErrStateConflict)
		}
		current, err := registry.Get(checkCtx, currentMove.Spec.VolumeID)
		if err != nil {
			return fmt.Errorf("read recovery volume state: %w", err)
		}
		if current.UID != identity.VolumeUID || current.Phase != volumeapi.PhaseBlocked || current.ActiveMove != currentMove.Name || current.OwnerNode != identity.NodeName || current.CurrentCopy == nil || *current.CurrentCopy != identity {
			return fmt.Errorf("recovery volume authority changed: %w", volumeapi.ErrStateConflict)
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
		if err != nil {
			return fmt.Errorf("read recovery installation identity: %w", err)
		}
		if installationID != identity.InstallationID {
			return fmt.Errorf("recovery installation authority changed: %w", volumeapi.ErrStateConflict)
		}
		pool, err := registry.PoolForNode(checkCtx, identity.NodeName)
		if err != nil {
			return fmt.Errorf("read recovery Pool: %w", err)
		}
		if pool.Name != identity.PoolName || pool.UID != identity.PoolUID {
			return fmt.Errorf("recovery Pool authority changed: %w", volumeapi.ErrStateConflict)
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
		if err != nil {
			return fmt.Errorf("read Move: %w", err)
		}
		if move.UID != options.moveUID || move.Status.SourceCopy == nil || move.Status.IncomingCopy == nil {
			return fmt.Errorf("move authority changed: %w", volumeapi.ErrStateConflict)
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
		if err != nil {
			return fmt.Errorf("read installation identity: %w", err)
		}
		if installationID != target.InstallationID {
			return fmt.Errorf("installation authority changed: %w", volumeapi.ErrStateConflict)
		}
		pool, err := registry.PoolForNode(ctx, target.NodeName)
		if err != nil {
			return fmt.Errorf("read target Pool: %w", err)
		}
		if pool.Name != target.PoolName || pool.UID != target.PoolUID {
			return fmt.Errorf("Pool authority changed: %w", volumeapi.ErrStateConflict)
		}
		state, err := registry.Get(ctx, move.Spec.VolumeID)
		if err != nil {
			return fmt.Errorf("read source volume state: %w", err)
		}
		if state.UID != target.VolumeUID || state.Phase != volumeapi.PhaseMoving || state.ActiveMove != move.Name || state.OwnerNode != move.Spec.SourceNode ||
			state.CurrentCopy == nil || *state.CurrentCopy != *move.Status.SourceCopy || containsString(state.PublishedNodes, move.Spec.SourceNode) {
			return fmt.Errorf("source volume authority changed: %w", volumeapi.ErrStateConflict)
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
	if err != nil {
		return nil, fmt.Errorf("read move executor Pod: %w", err)
	}
	if pod.DeletionTimestamp != nil {
		return nil, fmt.Errorf("move executor Pod is terminating: %w", volumeapi.ErrStateConflict)
	}
	if pod.Spec.NodeName != nodeName {
		return nil, fmt.Errorf("move executor Pod runs on a different node: %w", volumeapi.ErrStateConflict)
	}
	jobName, jobUID := "", ""
	for _, owner := range pod.OwnerReferences {
		if owner.Controller != nil && *owner.Controller && owner.Kind == "Job" {
			jobName, jobUID = owner.Name, string(owner.UID)
			break
		}
	}
	job, err := client.BatchV1().Jobs(options.namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read move executor Job: %w", err)
	}
	if jobUID == "" || string(job.UID) != jobUID || job.DeletionTimestamp != nil || job.Labels["shiftpv.io/move-uid"] != move.UID {
		return nil, fmt.Errorf("move executor is not authorized: %w", volumeapi.ErrStateConflict)
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
		if err != nil {
			return fmt.Errorf("read installation identity: %w", err)
		}
		if installationID != identity.InstallationID {
			return fmt.Errorf("installation authority changed: %w", volumeapi.ErrStateConflict)
		}
		pool, err := registry.PoolForNode(checkCtx, identity.NodeName)
		if err != nil {
			return fmt.Errorf("read Pool: %w", err)
		}
		if pool.Name != identity.PoolName || pool.UID != identity.PoolUID {
			return fmt.Errorf("Pool authority changed: %w", volumeapi.ErrStateConflict)
		}
		state, err := registry.Get(checkCtx, identity.VolumeID)
		if err != nil {
			return fmt.Errorf("read volume state: %w", err)
		}
		if state.UID != identity.VolumeUID || state.CurrentCopy == nil || *state.CurrentCopy != identity ||
			state.CreationOperationID != *operationID ||
			(state.Phase != volumeapi.PhasePending && state.Phase != volumeapi.PhaseReady) {
			return fmt.Errorf("volume creation authority changed: %w", volumeapi.ErrStateConflict)
		}
		return nil
	}
	return ownership.PrepareServing(ctx, *root, identity, authority)
}

func runCleanup(arguments []string) error {
	flags := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	authorityKind := flags.String("authority-kind", "", "cleanup parent kind")
	authorityName := flags.String("authority-name", "", "cleanup parent name")
	authorityUID := flags.String("authority-uid", "", "cleanup parent UID")
	operationID := flags.String("operation-id", "", "cleanup operation identity")
	namespace := flags.String("namespace", "", "helper Pod namespace")
	root := flags.String("pool-root", "/pool", "mounted Pool root")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	authorityIdentity := cleanupapi.Authority{Kind: *authorityKind, Name: *authorityName, UID: *authorityUID}
	if authorityIdentity.Validate() != nil || !volume.ValidIdentityToken(*operationID) || *namespace == "" || os.Getenv("POD_NAME") == "" {
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
	approved, err := cleanups.Get(ctx, authorityIdentity)
	if err != nil {
		return fmt.Errorf("read cleanup intent: %w", err)
	}
	if approved.UID != authorityIdentity.UID || approved.Spec.Authority != authorityIdentity || approved.Spec.OperationID != *operationID {
		return fmt.Errorf("cleanup intent identity changed: %w", cleanupapi.ErrConflict)
	}
	pod, err := client.CoreV1().Pods(*namespace).Get(ctx, os.Getenv("POD_NAME"), metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read cleanup executor Pod: %w", err)
	}
	jobName, jobUID := "", ""
	for _, owner := range pod.OwnerReferences {
		if owner.Controller != nil && *owner.Controller && owner.Kind == "Job" {
			jobName, jobUID = owner.Name, string(owner.UID)
			break
		}
	}
	if jobUID == "" || approved.Status.Phase != cleanupapi.PhaseRunning || approved.Status.Executor == nil ||
		approved.Status.Executor.JobName != jobName || approved.Status.Executor.JobUID != jobUID ||
		approved.Status.Executor.NodeName != approved.Spec.Target.NodeName || pod.Spec.NodeName != approved.Spec.Target.NodeName {
		return fmt.Errorf("cleanup executor is not authorized")
	}
	job, err := client.BatchV1().Jobs(*namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read cleanup executor Job: %w", err)
	}
	if string(job.UID) != jobUID || job.DeletionTimestamp != nil || !ownedByCleanupParent(job.OwnerReferences, authorityIdentity) {
		return fmt.Errorf("cleanup Job is not owned by the exact parent: %w", volumeapi.ErrStateConflict)
	}
	if pod.UID == "" {
		return fmt.Errorf("cleanup executor Pod has no UID")
	}
	executor := *approved.Status.Executor
	if executor.PodUID != string(pod.UID) {
		executor.PodUID = string(pod.UID)
		if err := cleanups.UpdateStatus(ctx, approved, cleanupapi.Status{Phase: cleanupapi.PhaseRunning, Executor: &executor}); err != nil {
			return fmt.Errorf("bind cleanup executor retry Pod: %w", err)
		}
		approved, err = cleanups.Get(ctx, authorityIdentity)
		if err != nil {
			return fmt.Errorf("read Pod-bound cleanup intent: %w", err)
		}
	}
	if !matchesCleanupExecutor(approved.Status.Executor, jobName, jobUID, pod) {
		return fmt.Errorf("cleanup executor does not match the exact running Pod")
	}
	authority := func(checkCtx context.Context, _ bool) error {
		current, err := cleanups.Get(checkCtx, authorityIdentity)
		if err != nil {
			return fmt.Errorf("read cleanup intent: %w", err)
		}
		if current.UID != approved.UID || current.Spec != approved.Spec || current.Status.Phase != cleanupapi.PhaseRunning ||
			!matchesCleanupExecutor(current.Status.Executor, jobName, jobUID, pod) {
			return fmt.Errorf("cleanup intent changed: %w", cleanupapi.ErrConflict)
		}
		installationID, err := registry.InstallationID(checkCtx)
		if err != nil {
			return fmt.Errorf("read installation identity: %w", err)
		}
		if installationID != approved.Spec.Target.InstallationID {
			return fmt.Errorf("installation authority changed: %w", volumeapi.ErrStateConflict)
		}
		pool, err := registry.PoolForIdentity(checkCtx, approved.Spec.Target.PoolName, approved.Spec.Target.PoolUID, approved.Spec.Target.NodeName)
		if err != nil {
			return fmt.Errorf("read cleanup target Pool: %w", err)
		}
		if pool.Name != approved.Spec.Target.PoolName || pool.UID != approved.Spec.Target.PoolUID ||
			!slices.Contains(pool.Finalizers, volumeapi.PoolProtectionFinalizer) {
			return fmt.Errorf("Pool authority changed: %w", volumeapi.ErrStateConflict)
		}
		return verifyCleanupAuthority(checkCtx, registry, approved)
	}
	localReceipt, digest, err := ownership.ReclaimWithResume(ctx, *root, approved.Spec.Target, approved.Spec.OperationID, authority)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	receipt := &cleanupapi.Receipt{
		OperationID: approved.Spec.OperationID, ExecutorUID: jobUID, ObservedAt: now,
		Retired: localReceipt.Retired, Purged: localReceipt.Purged, LocalReceiptDigest: digest,
	}
	return cleanups.UpdateStatus(ctx, approved, cleanupapi.Status{
		Phase: cleanupapi.PhaseVerifying, Executor: approved.Status.Executor, Receipt: receipt,
	})
}

func matchesCleanupExecutor(executor *cleanupapi.Executor, jobName, jobUID string, pod *corev1.Pod) bool {
	return executor != nil && pod != nil && pod.UID != "" && *executor == (cleanupapi.Executor{
		JobName: jobName, JobUID: jobUID, PodUID: string(pod.UID), NodeName: pod.Spec.NodeName,
	})
}

func ownedByCleanupParent(references []metav1.OwnerReference, authority cleanupapi.Authority) bool {
	for _, owner := range references {
		if owner.APIVersion == "shiftpv.io/v1alpha1" && owner.Controller != nil && *owner.Controller &&
			owner.Kind == authority.Kind && owner.Name == authority.Name && string(owner.UID) == authority.UID {
			return true
		}
	}
	return false
}

func verifyCleanupAuthority(ctx context.Context, registry *volumeapi.Registry, cleanup cleanupapi.Cleanup) error {
	switch cleanup.Spec.Authority.Kind {
	case "ShiftPVVolume":
		state, err := registry.Get(ctx, cleanup.Spec.Authority.Name)
		if err != nil {
			return fmt.Errorf("read volume cleanup state: %w", err)
		}
		if state.UID != cleanup.Spec.Authority.UID || state.CurrentCopy == nil || *state.CurrentCopy != cleanup.Spec.Target ||
			state.Phase != volumeapi.PhaseDeleting || state.DeletionOperationID != cleanup.Spec.OperationID || state.ActiveMove != "" || len(state.PublishedNodes) != 0 {
			return fmt.Errorf("volume cleanup authority changed: %w", volumeapi.ErrStateConflict)
		}
		return nil
	case "ShiftPVMove":
		return verifyMoveCleanupAuthority(ctx, registry, cleanup)
	default:
		return fmt.Errorf("cleanup authority %q is not implemented", cleanup.Spec.Authority.Kind)
	}
}

func verifyMoveCleanupAuthority(ctx context.Context, registry *volumeapi.Registry, cleanup cleanupapi.Cleanup) error {
	move, err := registry.GetMove(ctx, cleanup.Spec.Authority.Name)
	if err != nil {
		return fmt.Errorf("read move cleanup Move: %w", err)
	}
	if move.UID != cleanup.Spec.Authority.UID || move.Spec.VolumeID != cleanup.Spec.Target.VolumeID ||
		move.Status.SourceCopy == nil || move.Status.SourceCopy.NodeName != move.Spec.SourceNode {
		return fmt.Errorf("move cleanup authority changed: %w", volumeapi.ErrStateConflict)
	}
	state, err := registry.Get(ctx, move.Spec.VolumeID)
	if err != nil {
		return fmt.Errorf("read move cleanup volume state: %w", err)
	}
	if state.UID != cleanup.Spec.Target.VolumeUID || state.ActiveMove != move.Name || state.CurrentCopy == nil {
		return fmt.Errorf("move cleanup volume authority changed: %w", volumeapi.ErrStateConflict)
	}
	switch cleanup.Spec.Reason {
	case "MoveSource":
		if cleanup.Spec.OperationID != "cleanup-"+move.UID || *move.Status.SourceCopy != cleanup.Spec.Target || move.Status.DestinationCopy == nil {
			return fmt.Errorf("move source cleanup identity changed: %w", volumeapi.ErrStateConflict)
		}
		normal := (move.Status.Phase == "WaitingForDestinationPublish" || move.Status.Phase == "CleaningSource") &&
			state.Phase == volumeapi.PhaseReady && state.OwnerNode == move.Status.DestinationNode &&
			*state.CurrentCopy == *move.Status.DestinationCopy && containsString(state.PublishedNodes, move.Status.DestinationNode) &&
			!containsString(state.PublishedNodes, move.Spec.SourceNode)
		recovery := move.Status.Phase == "Blocked" && move.Spec.Recovery == "ResumeOwner" && move.Status.RecoveryPhase == "Retiring" &&
			move.Status.RecoveryOwner == move.Status.DestinationNode && state.Phase == volumeapi.PhaseReady &&
			state.OwnerNode == move.Status.DestinationNode && *state.CurrentCopy == *move.Status.DestinationCopy &&
			containsString(state.PublishedNodes, move.Status.DestinationNode) && !containsString(state.PublishedNodes, move.Spec.SourceNode)
		if !normal && !recovery {
			return fmt.Errorf("move source cleanup authority changed: %w", volumeapi.ErrStateConflict)
		}
		destinationPool, err := registry.ReadyPoolForNode(ctx, move.Status.DestinationNode)
		if err != nil {
			return fmt.Errorf("destination Pool publication proof unavailable: %w", err)
		}
		if !volumeapi.PoolHasPublishedCopy(destinationPool, move.Status.DestinationCopy) {
			return fmt.Errorf("move source cleanup publication proof changed: %w", volumeapi.ErrStateConflict)
		}
		return nil
	case "MoveRollback":
		targetIsDestination := (move.Status.IncomingCopy != nil && *move.Status.IncomingCopy == cleanup.Spec.Target) ||
			(move.Status.DestinationCopy != nil && *move.Status.DestinationCopy == cleanup.Spec.Target)
		if cleanup.Spec.OperationID != "rollback-"+move.UID || !targetIsDestination ||
			cleanup.Spec.Target.NodeName != move.Status.DestinationNode || cleanup.Spec.Target.PoolUID != move.Status.DestinationPoolUID ||
			move.Status.Phase != "Blocked" || move.Spec.Recovery != "ResumeOwner" || move.Status.RecoveryPhase != "Retiring" ||
			move.Status.RecoveryOwner != move.Spec.SourceNode || state.Phase != volumeapi.PhaseBlocked || state.OwnerNode != move.Spec.SourceNode ||
			*state.CurrentCopy != *move.Status.SourceCopy || containsString(state.PublishedNodes, cleanup.Spec.Target.NodeName) {
			return fmt.Errorf("move rollback cleanup authority changed: %w", volumeapi.ErrStateConflict)
		}
		return nil
	default:
		return fmt.Errorf("move cleanup reason %q is not implemented", cleanup.Spec.Reason)
	}
}
