package volumeapi

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/cagojeiger/ShiftPV/src/volume"
)

func TestStateCASPreservesConcurrentNodePublication(t *testing.T) {
	ctx := context.Background()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{VolumeResource: "ShiftPVVolumeList"})
	r := &Registry{Client: client}
	id := "shiftpv-44444444444444444444444444444444"
	if err := r.Ensure(ctx, id, "source"); err != nil {
		t.Fatal(err)
	}
	object, err := client.Resource(VolumeResource).Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	object.SetUID("volume-uid")
	if _, err := client.Resource(VolumeResource).Update(ctx, object, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	stale, _ := r.Get(ctx, id)
	if err := r.SetPublished(ctx, id, "source", true); err != nil {
		t.Fatal(err)
	}
	stale.Phase, stale.ActiveMove = PhaseMoving, "move"
	if err := r.CompareAndSetState(ctx, id, PhaseReady, "", "source", stale); err != nil {
		t.Fatal(err)
	}
	live, _ := r.Get(ctx, id)
	if !reflect.DeepEqual(live.PublishedNodes, []string{"source"}) {
		t.Fatalf("publish lost: %+v", live)
	}
	stale.Phase, stale.OwnerNode = PhaseReady, "destination"
	if err := r.CompareAndSetState(ctx, id, PhaseMoving, "move", "source", stale); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("foreign publication permitted commit: %v", err)
	}
	if err := r.SetPublished(ctx, id, "source", false); err != nil {
		t.Fatal(err)
	}
	if err := r.CompareAndSetState(ctx, id, PhaseMoving, "move", "source", stale); err != nil {
		t.Fatal(err)
	}
	live, _ = r.Get(ctx, id)
	if len(live.PublishedNodes) != 0 {
		t.Fatalf("unpublish lost: %+v", live)
	}
}

func TestStateCASRejectsReplacementVolumeUID(t *testing.T) {
	ctx := context.Background()
	const id = "shiftpv-55555555555555555555555555555555"
	original := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "shiftpv.io/v1alpha1", "kind": "ShiftPVVolume",
		"metadata": map[string]any{"name": id, "uid": "original-uid"},
		"spec":     map[string]any{"volumeID": id},
		"status":   map[string]any{"phase": PhaseReady, "ownerNode": "source", "activeMove": ""},
	}}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{VolumeResource: "ShiftPVVolumeList"}, original)
	registry := &Registry{Client: client}
	stale, err := registry.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	replacement := original.DeepCopy()
	replacement.SetUID("replacement-uid")
	replacement.SetResourceVersion("")
	if err := client.Resource(VolumeResource).Delete(ctx, id, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Resource(VolumeResource).Create(ctx, replacement, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	stale.Phase = PhaseMoving
	stale.ActiveMove = "stale-move"
	if err := registry.CompareAndSetState(ctx, id, PhaseReady, "", "source", stale); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("replacement Volume CAS error = %v", err)
	}
	current, err := registry.Get(ctx, id)
	if err != nil || current.UID != "replacement-uid" || current.Phase != PhaseReady || current.ActiveMove != "" {
		t.Fatalf("replacement Volume changed: state=%#v err=%v", current, err)
	}
}

func TestRecoveryFieldsRoundTrip(t *testing.T) {
	object := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "move", "uid": "uid"},
		"spec":     map[string]any{"volumeID": "volume", "sourceNode": "source", "recovery": "ResumeOwner"},
	}}
	status := MoveStatus{Phase: "Blocked", ConsumerUID: "consumer-uid", CandidateNodes: []string{}, LastTransitionTime: "2026-09-04T00:00:00Z", LastProgressTime: "2026-09-04T00:00:00Z", RecoveryPhase: "Quiescing", RecoveryOwner: "destination", RecoveryReason: "NodeNotReady", RecoveryMessage: "observe node"}
	setMoveStatus(object, status)
	move, err := moveFrom(object)
	if err != nil || move.Spec.Recovery != "ResumeOwner" || !reflect.DeepEqual(move.Status, status) {
		t.Fatalf("round trip: %+v %v", move, err)
	}
}

func TestClassifyCopyAuthorityUsesExactCurrentCopy(t *testing.T) {
	target := volume.CopyIdentity{
		InstallationID: "installation", PoolName: "pool-a", PoolUID: "pool-uid",
		VolumeID: "shiftpv-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", VolumeUID: "volume-uid",
		CopyID: "old-copy", NodeName: "node-a", Role: volume.RoleIncoming,
	}
	current := target
	current.CopyID, current.NodeName, current.Role = "current-copy", "node-b", volume.RoleServing

	for name, test := range map[string]struct {
		states map[string]State
		want   CopyAuthority
	}{
		"none": {states: map[string]State{}, want: CopyAuthorityNone},
		"current": {states: map[string]State{target.VolumeID: {
			UID: target.VolumeUID, CurrentCopy: &target,
		}}, want: CopyAuthorityCurrent},
		"superseded": {states: map[string]State{target.VolumeID: {
			UID: target.VolumeUID, CurrentCopy: &current,
		}}, want: CopyAuthoritySuperseded},
		"missing current identity": {states: map[string]State{target.VolumeID: {
			UID: target.VolumeUID,
		}}, want: CopyAuthorityUncertain},
		"contradictory current identity": {states: map[string]State{target.VolumeID: {
			UID: "another-volume-uid", CurrentCopy: &current,
		}}, want: CopyAuthorityUncertain},
		"ambiguous duplicate authority": {states: map[string]State{
			target.VolumeID: {UID: target.VolumeUID, CurrentCopy: &current},
			"shiftpv-bbbbbbbbbbbbbbbbbbbbbbbbbbbb": {UID: target.VolumeUID, CurrentCopy: func() *volume.CopyIdentity {
				duplicate := current
				duplicate.VolumeID = "shiftpv-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				return &duplicate
			}()},
		}, want: CopyAuthorityUncertain},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ClassifyCopyAuthority(test.states, target); got != test.want {
				t.Fatalf("authority=%v, want %v", got, test.want)
			}
		})
	}
}

func TestRegistryLifecycleAndPoolNodes(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		VolumeResource: "ShiftPVVolumeList",
		PoolResource:   "ShiftPVPoolList",
		MoveResource:   "ShiftPVMoveList",
	}, pool("pool-b", "node-b"), pool("pool-a", "node-a"))
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	registry := &Registry{Client: client, Now: func() time.Time { return now }}
	ctx := context.Background()

	if err := registry.Ensure(ctx, "shiftpv-11111111111111111111111111111111", "node-a"); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	state, err := registry.Get(ctx, "shiftpv-11111111111111111111111111111111")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if state.Phase != PhaseReady || state.OwnerNode != "node-a" {
		t.Fatalf("state = %#v", state)
	}
	if err := registry.SetPublished(ctx, "shiftpv-11111111111111111111111111111111", "node-a", true); err != nil {
		t.Fatalf("SetPublished(true): %v", err)
	}
	state, _ = registry.Get(ctx, "shiftpv-11111111111111111111111111111111")
	if !reflect.DeepEqual(state.PublishedNodes, []string{"node-a"}) {
		t.Fatalf("published nodes = %#v", state.PublishedNodes)
	}
	if err := registry.SetPublished(ctx, "shiftpv-11111111111111111111111111111111", "node-a", false); err != nil {
		t.Fatalf("SetPublished(false): %v", err)
	}
	nodes, err := registry.PoolNodes(ctx)
	if err != nil {
		t.Fatalf("PoolNodes: %v", err)
	}
	if !reflect.DeepEqual(nodes, []string{"node-a", "node-b"}) {
		t.Fatalf("pool nodes = %#v", nodes)
	}
	registered, err := registry.PoolForNode(ctx, "node-b")
	if err != nil || registered.Name != "pool-b" || registered.MountPath != "/mnt/shiftpv" || registered.CapacityLimit != "10Gi" {
		t.Fatalf("PoolForNode = %#v, %v", registered, err)
	}
}

func TestRegistryReturnsObjectIncarnationIdentity(t *testing.T) {
	ctx := context.Background()
	namespaceResource := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	clusterIdentity := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": installationNamespace, "uid": "installation-uid"},
	}}
	registeredPool := pool("pool-a", "node-a")
	registeredPool.SetUID("pool-uid")
	volumeID := "shiftpv-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	registeredVolume := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "shiftpv.io/v1alpha1", "kind": "ShiftPVVolume",
		"metadata": map[string]any{"name": volumeID, "uid": "volume-uid"},
		"spec":     map[string]any{"volumeID": volumeID},
		"status":   map[string]any{"phase": PhaseReady, "ownerNode": "node-a", "publishedNodes": []any{}},
	}}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		VolumeResource: "ShiftPVVolumeList", PoolResource: "ShiftPVPoolList", namespaceResource: "NamespaceList",
	}, clusterIdentity, registeredPool, registeredVolume)
	registry := &Registry{Client: client}
	installationID, err := registry.InstallationID(ctx)
	if err != nil || installationID != "installation-uid" {
		t.Fatalf("installationID=%q err=%v", installationID, err)
	}
	registered, err := registry.PoolForNode(ctx, "node-a")
	if err != nil || registered.UID != "pool-uid" {
		t.Fatalf("pool=%#v err=%v", registered, err)
	}
	state, err := registry.Get(ctx, volumeID)
	if err != nil || state.UID != "volume-uid" {
		t.Fatalf("volume=%#v err=%v", state, err)
	}
}

func TestBeginCreatePersistsIdentityBeforeReady(t *testing.T) {
	ctx := context.Background()
	namespaceResource := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	clusterIdentity := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": installationNamespace, "uid": "installation-uid"},
	}}
	registeredPool := pool("pool-a", "node-a")
	registeredPool.SetUID("pool-uid")
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		VolumeResource: "ShiftPVVolumeList", PoolResource: "ShiftPVPoolList", namespaceResource: "NamespaceList",
	}, clusterIdentity, registeredPool)
	client.PrependReactor("create", "shiftpvvolumes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		object := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		object.SetUID("volume-uid")
		return false, nil, nil
	})
	registry := &Registry{Client: client, Now: func() time.Time { return time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC) }}
	volumeID := "shiftpv-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	state, err := registry.BeginCreate(ctx, volumeID, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != PhasePending || state.UID != "volume-uid" || state.CreationOperationID != "create-volume-uid" || state.CurrentCopy == nil {
		t.Fatalf("pending state=%#v", state)
	}
	if state.CurrentCopy.InstallationID != "installation-uid" || state.CurrentCopy.PoolUID != "pool-uid" || state.CurrentCopy.VolumeUID != "volume-uid" || state.CurrentCopy.Role != volume.RoleServing {
		t.Fatalf("copy identity=%#v", state.CurrentCopy)
	}
	second, err := registry.BeginCreate(ctx, volumeID, "node-a")
	if err != nil || !reflect.DeepEqual(second, state) {
		t.Fatalf("idempotent begin=%#v err=%v", second, err)
	}
	if err := registry.CompleteCreate(ctx, volumeID, state.UID, *state.CurrentCopy); err != nil {
		t.Fatal(err)
	}
	ready, err := registry.Get(ctx, volumeID)
	if err != nil || ready.Phase != PhaseReady || ready.CreationOperationID != state.CreationOperationID {
		t.Fatalf("ready=%#v err=%v", ready, err)
	}
	if err := registry.CompleteCreate(ctx, volumeID, state.UID, *state.CurrentCopy); err != nil {
		t.Fatalf("idempotent completion: %v", err)
	}
}

func TestBeginCreateResumesExactPendingIdentityWithInvalidInventory(t *testing.T) {
	ctx := context.Background()
	namespaceResource := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	clusterIdentity := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": installationNamespace, "uid": "installation-uid"},
	}}
	registeredPool := pool("pool-a", "node-a")
	registeredPool.SetUID("pool-uid")
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		VolumeResource: "ShiftPVVolumeList", PoolResource: "ShiftPVPoolList", namespaceResource: "NamespaceList",
	}, clusterIdentity, registeredPool)
	client.PrependReactor("create", "shiftpvvolumes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		object := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		object.SetUID("volume-uid")
		return false, nil, nil
	})
	registry := &Registry{Client: client, Now: func() time.Time { return time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC) }}
	volumeID := "shiftpv-cccccccccccccccccccccccccccccccc"
	pending, err := registry.BeginCreate(ctx, volumeID, "node-a")
	if err != nil {
		t.Fatal(err)
	}

	currentPool, err := client.Resource(PoolResource).Get(ctx, registeredPool.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(currentPool.Object, false, "status", "inventory", "valid"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Resource(PoolResource).Update(ctx, currentPool, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ReadyPoolForNode(ctx, "node-a"); !errors.Is(err, ErrPoolNotReady) {
		t.Fatalf("invalid inventory remained eligible: %v", err)
	}

	resumed, err := registry.BeginCreate(ctx, volumeID, "node-a")
	if err != nil || !reflect.DeepEqual(resumed, pending) {
		t.Fatalf("exact pending creation did not resume: state=%#v err=%v", resumed, err)
	}
	if _, err := registry.BeginCreate(ctx, "shiftpv-dddddddddddddddddddddddddddddddd", "node-a"); !errors.Is(err, ErrPoolNotReady) {
		t.Fatalf("new creation bypassed invalid inventory: %v", err)
	}
}

func TestBeginDeleteFencesPublicationAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	volumeID := "shiftpv-cccccccccccccccccccccccccccccccc"
	copy := volume.CopyIdentity{
		InstallationID: "installation-uid", PoolName: "pool-a", PoolUID: "pool-uid",
		VolumeID: volumeID, VolumeUID: "volume-uid", CopyID: "copy-id", NodeName: "node-a", Role: volume.RoleServing,
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "shiftpv.io/v1alpha1", "kind": "ShiftPVVolume",
		"metadata": map[string]any{"name": volumeID, "uid": copy.VolumeUID},
		"spec":     map[string]any{"volumeID": volumeID},
	}}
	setState(object, State{UID: copy.VolumeUID, Phase: PhaseReady, OwnerNode: copy.NodeName, CurrentCopy: &copy})
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{VolumeResource: "ShiftPVVolumeList"}, object)
	registry := &Registry{Client: client}

	fenced, err := registry.BeginDelete(ctx, volumeID, copy.VolumeUID, copy)
	if err != nil || fenced.Phase != PhaseDeleting || fenced.DeletionOperationID != "delete-"+copy.VolumeUID {
		t.Fatalf("fenced=%#v err=%v", fenced, err)
	}
	if _, err := registry.BeginDelete(ctx, volumeID, copy.VolumeUID, copy); err != nil {
		t.Fatalf("idempotent deletion fence: %v", err)
	}
	if err := registry.BeginPublish(ctx, volumeID, copy.NodeName, copy); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("publication crossed deletion fence: %v", err)
	}
}

func TestBeginDeleteRejectsPublishedOrChangedIdentity(t *testing.T) {
	ctx := context.Background()
	volumeID := "shiftpv-dddddddddddddddddddddddddddddddd"
	copy := volume.CopyIdentity{
		InstallationID: "installation-uid", PoolName: "pool-a", PoolUID: "pool-uid",
		VolumeID: volumeID, VolumeUID: "volume-uid", CopyID: "copy-id", NodeName: "node-a", Role: volume.RoleServing,
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "shiftpv.io/v1alpha1", "kind": "ShiftPVVolume",
		"metadata": map[string]any{"name": volumeID, "uid": copy.VolumeUID},
		"spec":     map[string]any{"volumeID": volumeID},
	}}
	setState(object, State{UID: copy.VolumeUID, Phase: PhaseReady, OwnerNode: copy.NodeName, PublishedNodes: []string{copy.NodeName}, CurrentCopy: &copy})
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{VolumeResource: "ShiftPVVolumeList"}, object)
	registry := &Registry{Client: client}
	if _, err := registry.BeginDelete(ctx, volumeID, copy.VolumeUID, copy); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("published copy was fenced for deletion: %v", err)
	}
	if err := registry.SetPublished(ctx, volumeID, copy.NodeName, false); err != nil {
		t.Fatal(err)
	}
	replacement := copy
	replacement.CopyID = "replacement"
	if _, err := registry.BeginDelete(ctx, volumeID, copy.VolumeUID, replacement); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("changed copy identity was fenced for deletion: %v", err)
	}
}

func TestReconcilePublishedRequiresExactLiveCopy(t *testing.T) {
	ctx := context.Background()
	volumeID := "shiftpv-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	copy := volume.CopyIdentity{
		InstallationID: "installation-uid", PoolName: "pool-a", PoolUID: "pool-uid",
		VolumeID: volumeID, VolumeUID: "volume-uid", CopyID: "copy-id", NodeName: "node-a", Role: volume.RoleServing,
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "shiftpv.io/v1alpha1", "kind": "ShiftPVVolume",
		"metadata": map[string]any{"name": volumeID, "uid": copy.VolumeUID},
		"spec":     map[string]any{"volumeID": volumeID},
	}}
	setState(object, State{UID: copy.VolumeUID, Phase: PhaseReady, OwnerNode: copy.NodeName, CurrentCopy: &copy})
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{VolumeResource: "ShiftPVVolumeList"}, object)
	registry := &Registry{Client: client}
	if err := registry.ReconcilePublished(ctx, volumeID, copy.NodeName, copy, true); err != nil {
		t.Fatal(err)
	}
	state, err := registry.Get(ctx, volumeID)
	if err != nil || !reflect.DeepEqual(state.PublishedNodes, []string{copy.NodeName}) {
		t.Fatalf("published state=%#v err=%v", state, err)
	}
	replacement := copy
	replacement.CopyID = "replacement-copy"
	if err := registry.ReconcilePublished(ctx, volumeID, copy.NodeName, replacement, false); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("replacement copy cleared publication: %v", err)
	}
	state, err = registry.Get(ctx, volumeID)
	if err != nil || !reflect.DeepEqual(state.PublishedNodes, []string{copy.NodeName}) {
		t.Fatalf("publication changed after rejected identity: state=%#v err=%v", state, err)
	}
	if err := registry.ReconcilePublished(ctx, volumeID, copy.NodeName, copy, false); err != nil {
		t.Fatal(err)
	}
}

func TestCreationOperationIDRejectsUnboundedUID(t *testing.T) {
	if got, err := CreationOperationID("volume-uid"); err != nil || got != "create-volume-uid" {
		t.Fatalf("operationID=%q err=%v", got, err)
	}
	if _, err := CreationOperationID(strings.Repeat("x", 128)); err == nil {
		t.Fatal("unbounded creation operation identity was accepted")
	}
}

func TestRegistryReadyPoolsRejectsMissingStaleAndOutdatedStatus(t *testing.T) {
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	ready := pool("ready", "node-ready")
	stale := pool("stale", "node-stale")
	_ = unstructured.SetNestedField(stale.Object, now.Add(-4*time.Minute).Format(time.RFC3339), "status", "lastProbeTime")
	outdated := pool("outdated", "node-outdated")
	_ = unstructured.SetNestedField(outdated.Object, int64(0), "status", "observedGeneration")
	conditionOutdated := pool("condition-outdated", "node-condition-outdated")
	conditions, _, _ := unstructured.NestedSlice(conditionOutdated.Object, "status", "conditions")
	conditions[0].(map[string]any)["observedGeneration"] = int64(0)
	_ = unstructured.SetNestedSlice(conditionOutdated.Object, conditions, "status", "conditions")
	pending := pool("pending", "node-pending")
	delete(pending.Object, "status")
	missingInventory := pool("missing-inventory", "node-missing-inventory")
	unstructured.RemoveNestedField(missingInventory.Object, "status", "inventory")
	invalidInventory := pool("invalid-inventory", "node-invalid-inventory")
	_ = unstructured.SetNestedMap(invalidInventory.Object, map[string]any{
		"observedAt": now.Format(time.RFC3339), "valid": false,
	}, "status", "inventory")
	staleInventory := pool("stale-inventory", "node-stale-inventory")
	_ = unstructured.SetNestedMap(staleInventory.Object, map[string]any{
		"observedAt": now.Add(-4 * time.Minute).Format(time.RFC3339), "valid": true,
	}, "status", "inventory")
	truncated := pool("truncated", "node-truncated")
	_ = unstructured.SetNestedMap(truncated.Object, map[string]any{
		"observedAt": now.Format(time.RFC3339), "valid": true, "truncated": true,
	}, "status", "inventory")
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		PoolResource: "ShiftPVPoolList",
	}, ready, stale, outdated, conditionOutdated, pending, missingInventory, invalidInventory, staleInventory, truncated)
	registry := &Registry{Client: client, Now: func() time.Time { return now }}

	pools, err := registry.ReadyPools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 1 || pools[0].Name != "ready" {
		t.Fatalf("ready pools = %#v", pools)
	}
	nodes, err := registry.PoolNodes(context.Background())
	if err != nil || len(nodes) != 9 {
		t.Fatalf("registered topology nodes = %#v err=%v", nodes, err)
	}
	if _, err := registry.ReadyPoolForNode(context.Background(), "node-stale"); !errors.Is(err, ErrPoolNotReady) {
		t.Fatalf("stale pool error = %v", err)
	}
	if _, err := registry.ReadyPoolForNode(context.Background(), "node-truncated"); !errors.Is(err, ErrPoolNotReady) || !strings.Contains(err.Error(), "InventoryTruncated") {
		t.Fatalf("truncated pool error = %v", err)
	}
	for node, reason := range map[string]string{
		"node-missing-inventory": "InventoryMissing",
		"node-invalid-inventory": "InventoryInvalid",
		"node-stale-inventory":   "InventoryStale",
	} {
		if _, err := registry.ReadyPoolForNode(context.Background(), node); !errors.Is(err, ErrPoolNotReady) || !strings.Contains(err.Error(), reason) {
			t.Fatalf("%s pool error = %v", reason, err)
		}
	}
}

func TestRegistrySetPoolStatusUsesPoolIdentity(t *testing.T) {
	object := pool("pool-a", "node-a")
	object.SetUID("pool-a-uid")
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		PoolResource: "ShiftPVPoolList",
	}, object)
	registry := &Registry{Client: client}
	status := PoolStatus{ObservedGeneration: 1, LastProbeTime: metav1.NewTime(time.Now()), Conditions: []metav1.Condition{{
		Type: PoolConditionReady, Status: metav1.ConditionFalse, Reason: "ReadOnly", Message: "read-only",
	}}}
	if err := registry.SetPoolStatus(context.Background(), "pool-a", "pool-a-uid", "node-b", status); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("foreign node update error = %v", err)
	}
	if err := registry.SetPoolStatus(context.Background(), "pool-a", "replacement-uid", "node-a", status); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("replacement Pool update error = %v", err)
	}
	if err := registry.SetPoolStatus(context.Background(), "pool-a", "pool-a-uid", "node-a", status); err != nil {
		t.Fatal(err)
	}
	updated, err := registry.PoolForNode(context.Background(), "node-a")
	if err != nil || updated.Status.Conditions[0].Reason != "ReadOnly" {
		t.Fatalf("updated pool = %#v err=%v", updated, err)
	}
}

func TestRegistryPoolForNodeRejectsMissingAndDuplicateRegistration(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		VolumeResource: "ShiftPVVolumeList", PoolResource: "ShiftPVPoolList", MoveResource: "ShiftPVMoveList",
	}, pool("pool-a", "node-a"), pool("pool-a-duplicate", "node-a"))
	registry := &Registry{Client: client}
	if _, err := registry.PoolForNode(context.Background(), "node-b"); err == nil {
		t.Fatal("missing node registration was accepted")
	}
	if _, err := registry.PoolForNode(context.Background(), "node-a"); err == nil {
		t.Fatal("duplicate node registration was accepted")
	}
}

func TestRegistryRejectsMissingConfigurationAndOwnerConflict(t *testing.T) {
	if err := (&Registry{}).Ensure(context.Background(), "volume", "node"); err == nil {
		t.Fatal("expected missing client error")
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		VolumeResource: "ShiftPVVolumeList",
		PoolResource:   "ShiftPVPoolList",
		MoveResource:   "ShiftPVMoveList",
	})
	registry := &Registry{Client: client}
	ctx := context.Background()
	volumeID := "shiftpv-22222222222222222222222222222222"
	if err := registry.Ensure(ctx, volumeID, "node-a"); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	if err := registry.Ensure(ctx, volumeID, "node-b"); err == nil {
		t.Fatal("expected owner conflict")
	}
	if _, err := registry.PoolNodes(ctx); err == nil {
		t.Fatal("expected empty pool error")
	}
}

func TestRegistryCompareAndSetAndMoveStatus(t *testing.T) {
	ctx := context.Background()
	volumeID := "shiftpv-33333333333333333333333333333333"
	moveObject := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "shiftpv.io/v1alpha1", "kind": "ShiftPVMove",
		"metadata": map[string]any{"name": "move-test", "uid": "move-uid"},
		"spec":     map[string]any{"volumeID": volumeID, "sourceNode": "node-a"},
	}}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		VolumeResource: "ShiftPVVolumeList", PoolResource: "ShiftPVPoolList", MoveResource: "ShiftPVMoveList",
	}, pool("pool-a", "node-a"), moveObject)
	registry := &Registry{Client: client}
	if err := registry.Ensure(ctx, volumeID, "node-a"); err != nil {
		t.Fatal(err)
	}
	createdVolume, err := client.Resource(VolumeResource).Get(ctx, volumeID, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	createdVolume.SetUID("volume-uid")
	if _, err := client.Resource(VolumeResource).Update(ctx, createdVolume, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	current, err := registry.Get(ctx, volumeID)
	if err != nil {
		t.Fatal(err)
	}
	next := State{UID: current.UID, Phase: PhaseMoving, OwnerNode: "node-a", ActiveMove: "move-test"}
	if err := registry.CompareAndSetState(ctx, volumeID, PhaseReady, "", "node-a", next); err != nil {
		t.Fatal(err)
	}
	if err := registry.CompareAndSetState(ctx, volumeID, PhaseReady, "", "node-a", next); err == nil {
		t.Fatal("stale state precondition was accepted")
	}
	status := MoveStatus{Phase: "Copying", DestinationNode: "node-b", DestinationPoolUID: "pool-b-uid", ReplacementUID: "replacement-uid", CandidateNodes: []string{"node-b"}, EvictionRequested: true, CopyJobName: "copy"}
	if err := registry.SetMoveStatus(ctx, "move-test", "replacement-uid", status); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("replacement Move status error = %v", err)
	}
	if err := registry.SetMoveStatus(ctx, "move-test", "move-uid", status); err != nil {
		t.Fatal(err)
	}
	move, err := registry.GetMove(ctx, "move-test")
	if err != nil {
		t.Fatal(err)
	}
	if move.Status.Phase != "Copying" || move.Status.DestinationNode != "node-b" || move.Status.DestinationPoolUID != "pool-b-uid" || move.Status.ReplacementUID != "replacement-uid" || !move.Status.EvictionRequested {
		t.Fatalf("move = %#v", move)
	}
	moves, err := registry.ListMoves(ctx)
	if err != nil || len(moves) != 1 {
		t.Fatalf("moves=%#v err=%v", moves, err)
	}
	volumes, err := registry.ListVolumes(ctx)
	if err != nil || volumes[volumeID].ActiveMove != "move-test" {
		t.Fatalf("volumes=%#v err=%v", volumes, err)
	}
	pools, err := registry.Pools(ctx)
	if err != nil || pools[0].MountPath != "/mnt/shiftpv" {
		t.Fatalf("pools=%#v err=%v", pools, err)
	}
	volumeObject, err := client.Resource(VolumeResource).Get(ctx, volumeID, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	volumeObject.SetUID("volume-uid")
	if _, err := client.Resource(VolumeResource).Update(ctx, volumeObject, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("delete", "shiftpvvolumes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		options := action.(k8stesting.DeleteAction).GetDeleteOptions()
		if options.Preconditions == nil || options.Preconditions.UID == nil || string(*options.Preconditions.UID) != "volume-uid" {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "shiftpv.io", Resource: "shiftpvvolumes"}, volumeID, errors.New("UID precondition failed"))
		}
		return false, nil, nil
	})
	if err := registry.Delete(ctx, volumeID, "wrong-uid"); err == nil {
		t.Fatal("replacement volume was deleted with the wrong UID")
	}
	if err := registry.Delete(ctx, volumeID, "volume-uid"); err != nil {
		t.Fatal(err)
	}
	if err := registry.Delete(ctx, volumeID, "volume-uid"); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryCreateMove(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		VolumeResource: "ShiftPVVolumeList", PoolResource: "ShiftPVPoolList", MoveResource: "ShiftPVMoveList",
	})
	client.PrependReactor("create", "shiftpvmoves", func(action k8stesting.Action) (bool, runtime.Object, error) {
		created := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		if created.GetGenerateName() != "move-volume-" {
			t.Fatalf("generateName = %q", created.GetGenerateName())
		}
		created.SetName("move-volume-abcde")
		return true, created, nil
	})
	registry := &Registry{Client: client}
	move, err := registry.CreateMove(context.Background(), "move-volume-", MoveSpec{VolumeID: "volume", SourceNode: "source"})
	if err != nil {
		t.Fatal(err)
	}
	if move.Name != "move-volume-abcde" || move.Spec.VolumeID != "volume" || move.Spec.SourceNode != "source" {
		t.Fatalf("move = %#v", move)
	}
}

func TestRegistryDeleteMoveUsesUIDPreconditionAndIsIdempotent(t *testing.T) {
	move := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "shiftpv.io/v1alpha1",
		"kind":       "ShiftPVMove",
		"metadata":   map[string]any{"name": "move-test", "uid": "move-uid"},
		"spec":       map[string]any{"volumeID": "volume", "sourceNode": "source"},
	}}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		VolumeResource: "ShiftPVVolumeList", PoolResource: "ShiftPVPoolList", MoveResource: "ShiftPVMoveList",
	}, move)
	client.PrependReactor("delete", "shiftpvmoves", func(action k8stesting.Action) (bool, runtime.Object, error) {
		deletion := action.(k8stesting.DeleteAction)
		options := deletion.GetDeleteOptions()
		if options.Preconditions == nil || options.Preconditions.UID == nil {
			t.Fatal("Move deletion omitted the UID precondition")
		}
		if *options.Preconditions.UID == "wrong-uid" {
			return true, nil, apierrors.NewConflict(MoveResource.GroupResource(), deletion.GetName(), errors.New("UID precondition failed"))
		}
		return false, nil, nil
	})
	registry := &Registry{Client: client}

	if err := registry.DeleteMove(context.Background(), "move-test", "wrong-uid"); !apierrors.IsConflict(err) {
		t.Fatalf("wrong UID deletion error = %v, want conflict", err)
	}
	if _, err := client.Resource(MoveResource).Get(context.Background(), "move-test", metav1.GetOptions{}); err != nil {
		t.Fatalf("wrong UID deleted the Move: %v", err)
	}
	if err := registry.DeleteMove(context.Background(), "move-test", "move-uid"); err != nil {
		t.Fatal(err)
	}
	if err := registry.DeleteMove(context.Background(), "move-test", "move-uid"); err != nil {
		t.Fatalf("idempotent delete failed: %v", err)
	}
	if err := registry.DeleteMove(context.Background(), "", ""); err == nil {
		t.Fatal("empty delete identity was accepted")
	}
}

func pool(name, nodeName string) *unstructured.Unstructured {
	probeTime := "2026-09-07T00:00:00Z"
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "shiftpv.io/v1alpha1",
		"kind":       "ShiftPVPool",
		"metadata":   map[string]any{"name": name, "generation": int64(1)},
		"spec": map[string]any{
			"nodeName": nodeName, "mountPath": "/mnt/shiftpv",
			"capacity": map[string]any{"limit": "10Gi"},
		},
		"status": map[string]any{
			"observedGeneration": int64(1), "lastProbeTime": probeTime,
			"inventory": map[string]any{
				"observedAt": probeTime, "valid": true,
			},
			"conditions": []any{map[string]any{
				"type": PoolConditionReady, "status": "True", "observedGeneration": int64(1),
				"lastTransitionTime": probeTime, "reason": "PoolReady", "message": "ready",
			}},
		},
	}}
}
