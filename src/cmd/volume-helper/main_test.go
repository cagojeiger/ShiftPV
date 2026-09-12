package main

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/cleanupapi"
	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/volume"
)

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
		OperationID: "delete-" + copy.VolumeUID, Target: copy, Reason: "VolumeDelete", Approved: true,
		Authority: cleanupapi.Authority{Kind: "ShiftPVVolume", Name: volumeID, UID: copy.VolumeUID},
	}}
	if err := registry.SetState(context.Background(), volumeID, volumeapi.State{Phase: volumeapi.PhaseReady, OwnerNode: copy.NodeName, CurrentCopy: &copy}); err != nil {
		t.Fatal(err)
	}
	if err := verifyCleanupAuthority(context.Background(), nil, registry, "", cleanup, volumeapi.DefaultPoolReadinessStaleAfter, false); err == nil {
		t.Fatal("Ready volume was accepted without a deletion fence")
	}
	if _, err := registry.BeginDelete(context.Background(), volumeID, copy.VolumeUID, copy); err != nil {
		t.Fatal(err)
	}
	if err := verifyCleanupAuthority(context.Background(), nil, registry, "", cleanup, volumeapi.DefaultPoolReadinessStaleAfter, false); err != nil {
		t.Fatalf("durably fenced deletion was rejected: %v", err)
	}
	state, err := registry.Get(context.Background(), volumeID)
	if err != nil {
		t.Fatal(err)
	}
	state.PublishedNodes = []string{copy.NodeName}
	if err := registry.SetState(context.Background(), volumeID, state); err != nil {
		t.Fatal(err)
	}
	if err := verifyCleanupAuthority(context.Background(), nil, registry, "", cleanup, volumeapi.DefaultPoolReadinessStaleAfter, false); err == nil {
		t.Fatal("published volume crossed deletion authority")
	}
}

func TestOrphanCleanupAuthorityRequiresNoLiveReferenceMountOrReplacementReservation(t *testing.T) {
	const (
		volumeID             = "shiftpv-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		reservationUID       = "reservation-uid"
		replacementVolumeUID = "replacement-volume-uid"
	)
	target := volume.CopyIdentity{
		InstallationID: "installation", PoolName: "pool", PoolUID: "pool-uid", VolumeID: volumeID,
		VolumeUID: "volume-uid", CopyID: "copy-id", NodeName: "node-a", Role: volume.RoleServing,
	}
	poolObject := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "shiftpv.io/v1alpha1", "kind": "ShiftPVPool",
		"metadata": map[string]any{"name": target.PoolName, "uid": target.PoolUID, "generation": int64(1)},
		"spec":     map[string]any{"nodeName": target.NodeName, "mountPath": "/pool", "capacity": map[string]any{"limit": "1Gi"}},
	}}
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		volumeapi.VolumeResource: "ShiftPVVolumeList", volumeapi.MoveResource: "ShiftPVMoveList", volumeapi.PoolResource: "ShiftPVPoolList",
	}, poolObject)
	registry := &volumeapi.Registry{Client: dynamicClient}
	now := metav1.NewTime(time.Now().UTC())
	poolStatus := volumeapi.PoolStatus{
		ObservedGeneration: 1, LastProbeTime: now,
		Conditions: []metav1.Condition{
			{Type: volumeapi.PoolConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 1, LastTransitionTime: now, Reason: "Ready", Message: "ready"},
			{Type: volumeapi.PoolConditionAccessible, Status: metav1.ConditionTrue, ObservedGeneration: 1, LastTransitionTime: now, Reason: "PathAccessible", Message: "accessible"},
			{Type: volumeapi.PoolConditionWritable, Status: metav1.ConditionTrue, ObservedGeneration: 1, LastTransitionTime: now, Reason: "PathWritable", Message: "writable"},
			{Type: volumeapi.PoolConditionCapacityReadable, Status: metav1.ConditionTrue, ObservedGeneration: 1, LastTransitionTime: now, Reason: "CapacityReadable", Message: "capacity readable"},
		},
		Inventory: &volumeapi.PoolInventory{ObservedAt: now, Valid: true, Copies: []volumeapi.CopyObservation{{Marker: "copy", Identity: &target, Present: true}}},
	}
	if err := registry.SetPoolStatus(context.Background(), target.PoolName, target.PoolUID, target.NodeName, poolStatus); err != nil {
		t.Fatal(err)
	}
	reservation := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: volumeID, Namespace: "system", UID: types.UID(reservationUID), Labels: map[string]string{
			"app.kubernetes.io/name": "shiftpv", "app.kubernetes.io/component": "volume-reservation",
		}},
		Data: map[string]string{"volumeID": volumeID, "volumeUID": replacementVolumeUID},
	}
	client := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(target.InstallationID)}}, reservation,
	)
	cleanup := cleanupapi.Cleanup{Spec: cleanupapi.Spec{
		OperationID: "review-copy-id", Target: target, Reason: "OrphanReclaim", Approved: true,
		Authority: cleanupapi.Authority{Kind: "Namespace", Name: "kube-system", UID: target.InstallationID},
	}}
	recoveredMove := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "shiftpv.io/v1alpha1", "kind": "ShiftPVMove",
		"metadata": map[string]any{"name": "recovered-move", "uid": "recovered-move-uid"},
		"spec":     map[string]any{"volumeID": volumeID, "sourceNode": target.NodeName},
	}}
	if _, err := dynamicClient.Resource(volumeapi.MoveResource).Create(context.Background(), recoveredMove, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := registry.SetMoveStatus(context.Background(), recoveredMove.GetName(), string(recoveredMove.GetUID()), volumeapi.MoveStatus{
		Phase: "Blocked", RecoveryPhase: "Recovered", IncomingCopy: &target,
	}); err != nil {
		t.Fatal(err)
	}
	current := target
	current.VolumeUID, current.CopyID, current.NodeName, current.Role = replacementVolumeUID, "current-copy", "node-b", volume.RoleServing
	volumeObject := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "shiftpv.io/v1alpha1", "kind": "ShiftPVVolume",
		"metadata": map[string]any{"name": volumeID, "uid": replacementVolumeUID},
		"spec":     map[string]any{"volumeID": volumeID},
	}}
	if _, err := dynamicClient.Resource(volumeapi.VolumeResource).Create(context.Background(), volumeObject, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := registry.SetState(context.Background(), volumeID, volumeapi.State{UID: replacementVolumeUID, Phase: volumeapi.PhaseReady, OwnerNode: current.NodeName, CurrentCopy: &current}); err != nil {
		t.Fatal(err)
	}
	persistentVolume := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "live-pv"},
		Spec:       corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "csi.shiftpv.io", VolumeHandle: volumeID}}},
	}
	if _, err := client.CoreV1().PersistentVolumes().Create(context.Background(), persistentVolume, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := verifyCleanupAuthority(context.Background(), client, registry, "system", cleanup, volumeapi.DefaultPoolReadinessStaleAfter, false); err != nil {
		t.Fatalf("superseded copy retained recovered Move or volume-wide authority: %v", err)
	}
	deletingPool, err := dynamicClient.Resource(volumeapi.PoolResource).Get(context.Background(), target.PoolName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	deletionTime := metav1.NewTime(time.Now().UTC())
	deletingPool.SetDeletionTimestamp(&deletionTime)
	if _, err := dynamicClient.Resource(volumeapi.PoolResource).Update(context.Background(), deletingPool, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	poolStatus.Conditions[0] = metav1.Condition{Type: volumeapi.PoolConditionReady, Status: metav1.ConditionFalse, ObservedGeneration: 1, LastTransitionTime: deletionTime, Reason: "PoolDeregistering", Message: "new placement is closed"}
	poolStatus.LastProbeTime = deletionTime
	poolStatus.Inventory.ObservedAt = deletionTime
	if err := registry.SetPoolStatus(context.Background(), target.PoolName, target.PoolUID, target.NodeName, poolStatus); err != nil {
		t.Fatal(err)
	}
	if err := verifyCleanupAuthority(context.Background(), client, registry, "system", cleanup, volumeapi.DefaultPoolReadinessStaleAfter, false); err != nil {
		t.Fatalf("terminating Pool rejected exact orphan cleanup: %v", err)
	}
	withinConfiguredWindow := metav1.NewTime(time.Now().UTC().Add(-5 * time.Minute))
	poolStatus.LastProbeTime = withinConfiguredWindow
	poolStatus.Inventory.ObservedAt = withinConfiguredWindow
	if err := registry.SetPoolStatus(context.Background(), target.PoolName, target.PoolUID, target.NodeName, poolStatus); err != nil {
		t.Fatal(err)
	}
	if err := verifyCleanupAuthority(context.Background(), client, registry, "system", cleanup, 10*time.Minute, false); err != nil {
		t.Fatalf("configured helper freshness rejected exact orphan cleanup: %v", err)
	}
	poolStatus.LastProbeTime = metav1.Now()
	poolStatus.Inventory.ObservedAt = poolStatus.LastProbeTime
	poolStatus.Inventory.Valid = false
	poolStatus.Inventory.Message = "CopyObservationProblem"
	poolStatus.Inventory.Copies = []volumeapi.CopyObservation{{Marker: "path:.shiftpv/retired/copy-id", Present: true, Problem: "UnrecordedPath"}}
	if err := registry.SetPoolStatus(context.Background(), target.PoolName, target.PoolUID, target.NodeName, poolStatus); err != nil {
		t.Fatal(err)
	}
	if err := verifyCleanupAuthority(context.Background(), client, registry, "system", cleanup, volumeapi.DefaultPoolReadinessStaleAfter, false); err == nil {
		t.Fatal("invalid post-effect inventory was accepted for a fresh orphan cleanup")
	}
	if err := verifyCleanupAuthority(context.Background(), client, registry, "system", cleanup, volumeapi.DefaultPoolReadinessStaleAfter, true); err != nil {
		t.Fatalf("journaled orphan cleanup could not resume after its own filesystem effect: %v", err)
	}
	poolStatus.Inventory.Valid = true
	poolStatus.Inventory.Message = ""
	poolStatus.Inventory.Copies = []volumeapi.CopyObservation{{Marker: "copy", Identity: &target, Present: true}}
	if err := registry.SetPoolStatus(context.Background(), target.PoolName, target.PoolUID, target.NodeName, poolStatus); err != nil {
		t.Fatal(err)
	}
	if err := dynamicClient.Resource(volumeapi.MoveResource).Delete(context.Background(), recoveredMove.GetName(), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := dynamicClient.Resource(volumeapi.VolumeResource).Delete(context.Background(), volumeID, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := verifyCleanupAuthority(context.Background(), client, registry, "system", cleanup, volumeapi.DefaultPoolReadinessStaleAfter, false); err == nil {
		t.Fatal("PersistentVolume without exact current-copy proof did not revoke orphan cleanup authority")
	}
	if err := client.CoreV1().PersistentVolumes().Delete(context.Background(), persistentVolume.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	poolStatus.Inventory.Copies[0].Published = true
	if err := registry.SetPoolStatus(context.Background(), target.PoolName, target.PoolUID, target.NodeName, poolStatus); err != nil {
		t.Fatal(err)
	}
	if err := verifyCleanupAuthority(context.Background(), client, registry, "system", cleanup, volumeapi.DefaultPoolReadinessStaleAfter, false); err == nil {
		t.Fatal("published mount did not revoke orphan cleanup authority")
	}
	poolStatus.Inventory.Copies[0].Published = false
	poolStatus.Inventory.ObservedAt = metav1.NewTime(time.Now().UTC())
	poolStatus.LastProbeTime = poolStatus.Inventory.ObservedAt
	if err := registry.SetPoolStatus(context.Background(), target.PoolName, target.PoolUID, target.NodeName, poolStatus); err != nil {
		t.Fatal(err)
	}
	if err := client.CoreV1().ConfigMaps("system").Delete(context.Background(), volumeID, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	replacement := reservation.DeepCopy()
	replacement.ResourceVersion = ""
	replacement.UID = "replacement-reservation"
	if _, err := client.CoreV1().ConfigMaps("system").Create(context.Background(), replacement, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := verifyCleanupAuthority(context.Background(), client, registry, "system", cleanup, volumeapi.DefaultPoolReadinessStaleAfter, false); err == nil {
		t.Fatal("replacement reservation did not revoke orphan cleanup authority")
	}
	if err := verifyCleanupAuthority(context.Background(), client, registry, "system", cleanup, volumeapi.DefaultPoolReadinessStaleAfter, true); err == nil {
		t.Fatal("journal replay ignored replacement reservation authority")
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
	if err := registry.SetState(context.Background(), volumeID, volumeapi.State{UID: source.VolumeUID, Phase: volumeapi.PhaseMoving, OwnerNode: "source", ActiveMove: moveName, CurrentCopy: &source}); err != nil {
		t.Fatal(err)
	}
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
	if err := registry.SetState(context.Background(), volumeID, volumeapi.State{UID: identity.VolumeUID, Phase: volumeapi.PhaseMoving, OwnerNode: identity.NodeName, ActiveMove: moveName, CurrentCopy: &identity}); err != nil {
		t.Fatal(err)
	}
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
	if err := registry.SetState(context.Background(), volumeID, state); err != nil {
		t.Fatal(err)
	}
	if err := authority(context.Background()); err == nil {
		t.Fatal("published source was accepted for transfer")
	}
}
