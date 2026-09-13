package main

import (
	"context"
	"reflect"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/cleanupapi"
	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/volume"
)

func TestMoveCopyArgumentsPreserveFilesystemContract(t *testing.T) {
	copyArguments, verifyArguments := moveCopyArguments("rsync://source/data/", "/pool/incoming")
	wantCopy := []string{
		"-aHAXS", "--numeric-ids", "--one-file-system", "--no-devices", "--delete", "--fsync",
		"rsync://source/data/", "/pool/incoming/",
	}
	wantVerify := []string{
		"-aHAXS", "--numeric-ids", "--one-file-system", "--no-devices", "--delete",
		"--checksum", "--dry-run", "--itemize-changes", "rsync://source/data/", "/pool/incoming/",
	}
	if !reflect.DeepEqual(copyArguments, wantCopy) {
		t.Fatalf("copy arguments = %#v", copyArguments)
	}
	if !reflect.DeepEqual(verifyArguments, wantVerify) {
		t.Fatalf("verify arguments = %#v", verifyArguments)
	}
}

func TestVolumeCleanupAuthorityRequiresDurableDeletionFence(t *testing.T) {
	const volumeID = "shiftpv-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	copy := volume.CopyIdentity{
		InstallationID: "installation", PoolName: "pool", PoolUID: "pool-uid",
		VolumeID: volumeID, VolumeUID: "volume-uid", CopyID: "copy-id", NodeName: "node-a", Role: volume.RoleServing,
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "shiftpv.io/v1alpha1", "kind": "ShiftPVVolume",
		"metadata": map[string]any{"name": volumeID, "uid": copy.VolumeUID},
		"spec":     map[string]any{"volumeID": volumeID},
	}}
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{volumeapi.VolumeResource: "ShiftPVVolumeList"}, object)
	registry := &volumeapi.Registry{Client: dynamicClient}
	cleanup := cleanupapi.Cleanup{Spec: cleanupapi.Spec{
		OperationID: "delete-" + copy.VolumeUID, Target: copy, Reason: "VolumeDelete",
		Authority: cleanupapi.Authority{Kind: "ShiftPVVolume", Name: volumeID, UID: copy.VolumeUID},
	}}
	setVolumeStateFixture(t, dynamicClient, volumeID, volumeapi.State{Phase: volumeapi.PhaseReady, OwnerNode: copy.NodeName, CurrentCopy: &copy})
	if err := verifyCleanupAuthority(context.Background(), registry, cleanup, false); err == nil {
		t.Fatal("Ready volume was accepted without a deletion fence")
	}
	if _, err := registry.BeginDelete(context.Background(), volumeID, copy.VolumeUID, copy); err != nil {
		t.Fatal(err)
	}
	if err := verifyCleanupAuthority(context.Background(), registry, cleanup, false); err != nil {
		t.Fatalf("durably fenced deletion was rejected: %v", err)
	}
	state, err := registry.Get(context.Background(), volumeID)
	if err != nil {
		t.Fatal(err)
	}
	state.PublishedNodes = []string{copy.NodeName}
	setVolumeStateFixture(t, dynamicClient, volumeID, state)
	if err := verifyCleanupAuthority(context.Background(), registry, cleanup, false); err == nil {
		t.Fatal("published volume crossed deletion authority")
	}
}

func TestUnknownOrphanCleanupHasNoExecutableAuthority(t *testing.T) {
	cleanup := cleanupapi.Cleanup{Spec: cleanupapi.Spec{
		OperationID: "review-orphan", Reason: "OrphanReclaim",
		Target: volume.CopyIdentity{
			InstallationID: "installation", PoolName: "pool", PoolUID: "pool-uid",
			VolumeID: "shiftpv-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", VolumeUID: "volume-uid",
			CopyID: "copy-id", NodeName: "node-a", Role: volume.RoleServing,
		},
		Authority: cleanupapi.Authority{Kind: "Namespace", Name: "kube-system", UID: "installation"},
	}}
	if err := cleanup.Spec.Validate(); err == nil {
		t.Fatal("orphan cleanup intent unexpectedly validated")
	}
	if err := verifyCleanupAuthority(context.Background(), nil, cleanup, false); err == nil {
		t.Fatal("unknown orphan observation gained destructive authority")
	}
}

func TestCleanupExecutorMustMatchExactRunningPod(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "pod-uid"}, Spec: corev1.PodSpec{NodeName: "worker-a"}}
	executor := &cleanupapi.Executor{JobName: "cleanup-job", JobUID: "job-uid", PodUID: "pod-uid", NodeName: "worker-a"}
	if !matchesCleanupExecutor(executor, "cleanup-job", "job-uid", pod) {
		t.Fatal("exact Pod-bound executor was rejected")
	}
	for name, mutate := range map[string]func(*cleanupapi.Executor, *corev1.Pod){
		"missing executor": func(current *cleanupapi.Executor, _ *corev1.Pod) { *current = cleanupapi.Executor{} },
		"job name":         func(current *cleanupapi.Executor, _ *corev1.Pod) { current.JobName = "replacement" },
		"job UID":          func(current *cleanupapi.Executor, _ *corev1.Pod) { current.JobUID = "replacement" },
		"Pod UID":          func(current *cleanupapi.Executor, _ *corev1.Pod) { current.PodUID = "replacement" },
		"node":             func(_ *cleanupapi.Executor, currentPod *corev1.Pod) { currentPod.Spec.NodeName = "worker-b" },
	} {
		t.Run(name, func(t *testing.T) {
			changedExecutor := *executor
			changedPod := pod.DeepCopy()
			mutate(&changedExecutor, changedPod)
			if matchesCleanupExecutor(&changedExecutor, "cleanup-job", "job-uid", changedPod) {
				t.Fatal("changed executor identity was accepted")
			}
		})
	}
	if matchesCleanupExecutor(nil, "cleanup-job", "job-uid", pod) || matchesCleanupExecutor(executor, "cleanup-job", "job-uid", nil) {
		t.Fatal("missing executor or Pod was accepted")
	}
}

func TestRecoveryCleanupAuthorityRequiresExactTargetAndRetainedOwner(t *testing.T) {
	const (
		volumeID = "shiftpv-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		moveName = "move-recovery"
		moveUID  = "move-recovery-uid"
	)
	source := volume.CopyIdentity{
		InstallationID: "installation", PoolName: "source-pool", PoolUID: "source-pool-uid",
		VolumeID: volumeID, VolumeUID: "volume-uid", CopyID: "source-copy", NodeName: "source", Role: volume.RoleServing,
	}
	incoming := volume.CopyIdentity{
		InstallationID: source.InstallationID, PoolName: "destination-pool", PoolUID: "destination-pool-uid",
		VolumeID: volumeID, VolumeUID: source.VolumeUID, CopyID: "incoming-copy", NodeName: "destination", Role: volume.RoleIncoming,
	}
	destination := incoming
	destination.Role = volume.RoleServing

	for name, test := range map[string]struct {
		reason, operationID, recoveryOwner string
		target, current                    volume.CopyIdentity
	}{
		"precommit rollback": {
			reason: "MoveRollback", operationID: "rollback-" + moveUID, recoveryOwner: source.NodeName,
			target: incoming, current: source,
		},
		"postcommit source cleanup": {
			reason: "MoveSource", operationID: "cleanup-" + moveUID, recoveryOwner: destination.NodeName,
			target: source, current: destination,
		},
	} {
		t.Run(name, func(t *testing.T) {
			moveObject := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "shiftpv.io/v1alpha1", "kind": "ShiftPVMove",
				"metadata": map[string]any{"name": moveName, "uid": moveUID},
				"spec":     map[string]any{"volumeID": volumeID, "sourceNode": source.NodeName, "recovery": "ResumeOwner"},
			}}
			volumeObject := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "shiftpv.io/v1alpha1", "kind": "ShiftPVVolume",
				"metadata": map[string]any{"name": volumeID, "uid": source.VolumeUID},
				"spec":     map[string]any{"volumeID": volumeID},
			}}
			poolObject := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "shiftpv.io/v1alpha1", "kind": "ShiftPVPool",
				"metadata": map[string]any{
					"name": destination.PoolName, "uid": destination.PoolUID, "generation": int64(1),
					"finalizers": []any{volumeapi.PoolProtectionFinalizer},
				},
				"spec": map[string]any{"nodeName": destination.NodeName, "mountPath": "/destination-pool"},
			}}
			client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
				volumeapi.VolumeResource: "ShiftPVVolumeList", volumeapi.MoveResource: "ShiftPVMoveList", volumeapi.PoolResource: "ShiftPVPoolList",
			}, moveObject, volumeObject, poolObject)
			registry := &volumeapi.Registry{Client: client}
			moveStatus := volumeapi.MoveStatus{
				Phase: "Blocked", RecoveryPhase: "Retiring", RecoveryOwner: test.recoveryOwner,
				DestinationNode: destination.NodeName, DestinationPoolUID: destination.PoolUID,
				SourceCopy: &source, IncomingCopy: &incoming, DestinationCopy: &destination,
			}
			if err := registry.SetMoveStatus(context.Background(), moveName, moveUID, moveStatus); err != nil {
				t.Fatal(err)
			}
			statePhase := volumeapi.PhaseBlocked
			if test.reason == "MoveSource" {
				statePhase = volumeapi.PhaseReady
			}
			state := volumeapi.State{
				UID: source.VolumeUID, Phase: statePhase, OwnerNode: test.current.NodeName,
				ActiveMove: moveName, CurrentCopy: &test.current, PublishedNodes: []string{test.current.NodeName},
			}
			setVolumeStateFixture(t, client, volumeID, state)
			if test.reason == "MoveSource" {
				now := metav1.Now()
				if err := registry.SetPoolStatus(context.Background(), destination.PoolName, destination.PoolUID, destination.NodeName, volumeapi.PoolStatus{
					ObservedGeneration: 1,
					LastProbeTime:      now,
					Conditions:         []metav1.Condition{{Type: volumeapi.PoolConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 1}},
					Inventory: &volumeapi.PoolInventory{
						ObservedAt: now, Valid: true,
						Copies: []volumeapi.CopyObservation{{Identity: &destination, Present: true, Published: true}},
					},
				}); err != nil {
					t.Fatal(err)
				}
			}
			cleanup := cleanupapi.Cleanup{Spec: cleanupapi.Spec{
				OperationID: test.operationID, Target: test.target, Reason: test.reason,
				Authority: cleanupapi.Authority{Kind: "ShiftPVMove", Name: moveName, UID: moveUID},
			}}
			if err := verifyCleanupAuthority(context.Background(), registry, cleanup, false); err != nil {
				t.Fatalf("exact recovery cleanup rejected: %v", err)
			}
			if test.reason == "MoveSource" {
				state.PublishedNodes = nil
				setVolumeStateFixture(t, client, volumeID, state)
				if err := verifyCleanupAuthority(context.Background(), registry, cleanup, false); err == nil {
					t.Fatal("postcommit source cleanup was accepted before destination publish intent")
				}
				state.PublishedNodes = []string{test.current.NodeName}
				setVolumeStateFixture(t, client, volumeID, state)
				now := metav1.Now()
				if err := registry.SetPoolStatus(context.Background(), destination.PoolName, destination.PoolUID, destination.NodeName, volumeapi.PoolStatus{
					ObservedGeneration: 1,
					LastProbeTime:      now,
					Conditions:         []metav1.Condition{{Type: volumeapi.PoolConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 1}},
					Inventory: &volumeapi.PoolInventory{
						ObservedAt: now, Valid: true,
						Copies: []volumeapi.CopyObservation{{Identity: &destination, Present: true, Published: false}},
					},
				}); err != nil {
					t.Fatal(err)
				}
				if err := verifyCleanupAuthority(context.Background(), registry, cleanup, false); err == nil {
					t.Fatal("postcommit source cleanup was accepted before scanner publication proof")
				}
				if err := registry.SetPoolStatus(context.Background(), destination.PoolName, destination.PoolUID, destination.NodeName, volumeapi.PoolStatus{
					ObservedGeneration: 1,
					LastProbeTime:      now,
					Conditions:         []metav1.Condition{{Type: volumeapi.PoolConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 1}},
					Inventory: &volumeapi.PoolInventory{
						ObservedAt: now, Valid: true,
						Copies: []volumeapi.CopyObservation{{Identity: &destination, Present: true, Published: true}},
					},
				}); err != nil {
					t.Fatal(err)
				}
			}
			cleanup.Spec.OperationID = "replacement-operation"
			if err := verifyCleanupAuthority(context.Background(), registry, cleanup, false); err == nil {
				t.Fatal("replacement cleanup operation was accepted")
			}
			cleanup.Spec.OperationID = test.operationID
			state.PublishedNodes = append(state.PublishedNodes, test.target.NodeName)
			setVolumeStateFixture(t, client, volumeID, state)
			if err := verifyCleanupAuthority(context.Background(), registry, cleanup, false); err == nil {
				t.Fatal("published cleanup target was accepted")
			}
		})
	}
}

func TestMoveAuthorityRequiresExactMoveJobPoolAndSourceCopy(t *testing.T) {
	const (
		volumeID = "shiftpv-0123456789abcdef0123456789abcdef"
		moveName = "move-test"
		moveUID  = "move-uid"
		jobName  = "copy-job"
		jobUID   = "job-uid"
	)
	source := volume.CopyIdentity{InstallationID: "installation", PoolName: "source-pool", PoolUID: "source-pool-uid", VolumeID: volumeID, VolumeUID: "volume-uid", CopyID: "source-copy", NodeName: "source", Role: volume.RoleServing}
	incoming := volume.CopyIdentity{InstallationID: "installation", PoolName: "destination-pool", PoolUID: "destination-pool-uid", VolumeID: volumeID, VolumeUID: "volume-uid", CopyID: "incoming-copy", NodeName: "destination", Role: volume.RoleIncoming}
	moveObject := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "shiftpv.io/v1alpha1", "kind": "ShiftPVMove", "metadata": map[string]any{"name": moveName, "uid": moveUID}, "spec": map[string]any{"volumeID": volumeID, "sourceNode": "source"}}}
	volumeObject := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "shiftpv.io/v1alpha1", "kind": "ShiftPVVolume", "metadata": map[string]any{"name": volumeID, "uid": source.VolumeUID}, "spec": map[string]any{"volumeID": volumeID}}}
	poolObject := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "shiftpv.io/v1alpha1", "kind": "ShiftPVPool", "metadata": map[string]any{"name": incoming.PoolName, "uid": incoming.PoolUID}, "spec": map[string]any{"nodeName": incoming.NodeName, "mountPath": "/pool", "capacity": map[string]any{"limit": "1Gi"}}}}
	clusterIdentity := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "kube-system", "uid": source.InstallationID}}}
	namespaceResource := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		volumeapi.VolumeResource: "ShiftPVVolumeList", volumeapi.MoveResource: "ShiftPVMoveList", volumeapi.PoolResource: "ShiftPVPoolList", namespaceResource: "NamespaceList",
	}, moveObject, volumeObject, poolObject, clusterIdentity)
	registry := &volumeapi.Registry{Client: dynamicClient}
	moveStatus := volumeapi.MoveStatus{Phase: "Copying", SourceCopy: &source, IncomingCopy: &incoming, CopyOperationID: "copy-operation", CopyJobName: jobName}
	if err := registry.SetMoveStatus(context.Background(), moveName, moveUID, moveStatus); err != nil {
		t.Fatal(err)
	}
	setVolumeStateFixture(t, dynamicClient, volumeID, volumeapi.State{UID: source.VolumeUID, Phase: volumeapi.PhaseMoving, OwnerNode: "source", ActiveMove: moveName, CurrentCopy: &source})
	controller := true
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: "system", UID: types.UID(jobUID), Labels: map[string]string{"shiftpv.io/move-uid": moveUID}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "shiftpv.io/v1alpha1", Kind: "ShiftPVMove", Name: moveName, UID: types.UID(moveUID), Controller: &controller}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "copy-pod", Namespace: "system", OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: jobName, UID: types.UID(jobUID), Controller: &controller}}}, Spec: corev1.PodSpec{NodeName: incoming.NodeName}}
	client := fake.NewClientset(job, pod)
	t.Setenv("POD_NAME", pod.Name)
	options := moveOptions{moveName: moveName, moveUID: moveUID, operationID: "copy-operation", namespace: "system"}
	authority := moveAuthority(client, registry, options, "copy", incoming)
	if err := authority(context.Background()); err != nil {
		t.Fatal(err)
	}
	moveStatus.CopyOperationID = "replacement"
	if err := registry.SetMoveStatus(context.Background(), moveName, moveUID, moveStatus); err == nil {
		t.Fatal("immutable operation identity was replaced")
	}
	job.Labels["shiftpv.io/move-uid"] = "replacement"
	if _, err := client.BatchV1().Jobs("system").Update(context.Background(), job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := authority(context.Background()); err == nil {
		t.Fatal("changed executor identity was accepted")
	}
}

func TestSourceAuthorityRequiresCurrentMoveOwnedPodAndUnpublishedCopy(t *testing.T) {
	const (
		volumeID = "shiftpv-0123456789abcdef0123456789abcdef"
		moveName = "move-source"
		moveUID  = "move-uid"
	)
	identity := volume.CopyIdentity{InstallationID: "installation", PoolName: "source-pool", PoolUID: "source-pool-uid", VolumeID: volumeID, VolumeUID: "volume-uid", CopyID: "source-copy", NodeName: "source", Role: volume.RoleServing}
	moveObject := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "shiftpv.io/v1alpha1", "kind": "ShiftPVMove", "metadata": map[string]any{"name": moveName, "uid": moveUID}, "spec": map[string]any{"volumeID": volumeID, "sourceNode": "source"}}}
	volumeObject := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "shiftpv.io/v1alpha1", "kind": "ShiftPVVolume", "metadata": map[string]any{"name": volumeID, "uid": identity.VolumeUID}, "spec": map[string]any{"volumeID": volumeID}}}
	poolObject := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "shiftpv.io/v1alpha1", "kind": "ShiftPVPool", "metadata": map[string]any{"name": identity.PoolName, "uid": identity.PoolUID}, "spec": map[string]any{"nodeName": identity.NodeName, "mountPath": "/pool", "capacity": map[string]any{"limit": "1Gi"}}}}
	clusterIdentity := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "kube-system", "uid": identity.InstallationID}}}
	namespaceResource := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		volumeapi.VolumeResource: "ShiftPVVolumeList", volumeapi.MoveResource: "ShiftPVMoveList", volumeapi.PoolResource: "ShiftPVPoolList", namespaceResource: "NamespaceList",
	}, moveObject, volumeObject, poolObject, clusterIdentity)
	registry := &volumeapi.Registry{Client: dynamicClient}
	status := volumeapi.MoveStatus{Phase: "Copying", SourceCopy: &identity, CopyOperationID: "copy-operation"}
	if err := registry.SetMoveStatus(context.Background(), moveName, moveUID, status); err != nil {
		t.Fatal(err)
	}
	setVolumeStateFixture(t, dynamicClient, volumeID, volumeapi.State{UID: identity.VolumeUID, Phase: volumeapi.PhaseMoving, OwnerNode: identity.NodeName, ActiveMove: moveName, CurrentCopy: &identity})
	controller := true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "source-pod", Namespace: "system", UID: "pod-uid", Labels: map[string]string{"shiftpv.io/move-uid": moveUID},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "shiftpv.io/v1alpha1", Kind: "ShiftPVMove", Name: moveName, UID: types.UID(moveUID), Controller: &controller}},
	}, Spec: corev1.PodSpec{NodeName: identity.NodeName}}
	client := fake.NewClientset(pod)
	t.Setenv("POD_NAME", pod.Name)
	options := moveOptions{moveName: moveName, moveUID: moveUID, operationID: "copy-operation", namespace: "system"}
	authority := sourceAuthority(client, registry, options, identity)
	if err := authority(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := registry.Get(context.Background(), volumeID)
	if err != nil {
		t.Fatal(err)
	}
	state.PublishedNodes = []string{identity.NodeName}
	setVolumeStateFixture(t, dynamicClient, volumeID, state)
	if err := authority(context.Background()); err == nil {
		t.Fatal("published source was accepted for transfer")
	}
}

func setVolumeStateFixture(t *testing.T, client dynamic.Interface, volumeID string, state volumeapi.State) {
	t.Helper()
	resource := client.Resource(volumeapi.VolumeResource)
	object, err := resource.Get(context.Background(), volumeID, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ShiftPVVolume fixture: %v", err)
	}
	previous, _ := object.Object["status"].(map[string]any)
	status := map[string]any{
		"phase": state.Phase, "ownerNode": state.OwnerNode, "activeMove": state.ActiveMove,
		"publishedNodes": stringSliceToAnyFixture(state.PublishedNodes),
	}
	if cleanup := previous["cleanup"]; cleanup != nil {
		status["cleanup"] = cleanup
	}
	for name, value := range map[string]string{
		"creationOperationID": state.CreationOperationID,
		"deletionOperationID": state.DeletionOperationID,
	} {
		if value != "" {
			status[name] = value
		}
	}
	if state.CurrentCopy != nil {
		encoded, err := runtime.DefaultUnstructuredConverter.ToUnstructured(state.CurrentCopy)
		if err != nil {
			t.Fatalf("encode current copy fixture: %v", err)
		}
		status["currentCopy"] = encoded
	}
	object.Object["status"] = status
	if _, err := resource.UpdateStatus(context.Background(), object, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update ShiftPVVolume fixture: %v", err)
	}
}

func stringSliceToAnyFixture(values []string) []any {
	result := make([]any, len(values))
	for index := range values {
		result[index] = values[index]
	}
	return result
}
