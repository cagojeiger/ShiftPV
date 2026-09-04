package volumeapi

import (
	"context"
	"errors"
	"reflect"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestStateCASPreservesConcurrentNodePublication(t *testing.T) {
	ctx := context.Background()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{VolumeResource: "ShiftPVVolumeList"})
	r := &Registry{Client: client}
	id := "shiftpv-44444444444444444444444444444444"
	if err := r.Ensure(ctx, id, "source"); err != nil {
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

func TestRegistryLifecycleAndPoolNodes(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		VolumeResource: "ShiftPVVolumeList",
		PoolResource:   "ShiftPVPoolList",
		MoveResource:   "ShiftPVMoveList",
	}, pool("pool-b", "node-b"), pool("pool-a", "node-a"))
	registry := &Registry{Client: client}
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
	if err != nil || registered.Name != "pool-b" || registered.MountPath != "/mnt/shiftpv" {
		t.Fatalf("PoolForNode = %#v, %v", registered, err)
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
		"metadata": map[string]any{"name": "move-test"},
		"spec":     map[string]any{"volumeID": volumeID, "sourceNode": "node-a"},
	}}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		VolumeResource: "ShiftPVVolumeList", PoolResource: "ShiftPVPoolList", MoveResource: "ShiftPVMoveList",
	}, pool("pool-a", "node-a"), moveObject)
	registry := &Registry{Client: client}
	if err := registry.Ensure(ctx, volumeID, "node-a"); err != nil {
		t.Fatal(err)
	}
	next := State{Phase: PhaseMoving, OwnerNode: "node-a", ActiveMove: "move-test"}
	if err := registry.CompareAndSetState(ctx, volumeID, PhaseReady, "", "node-a", next); err != nil {
		t.Fatal(err)
	}
	if err := registry.CompareAndSetState(ctx, volumeID, PhaseReady, "", "node-a", next); err == nil {
		t.Fatal("stale state precondition was accepted")
	}
	status := MoveStatus{Phase: "Copying", DestinationNode: "node-b", CandidateNodes: []string{"node-b"}, EvictionRequested: true, CopyJobName: "copy"}
	if err := registry.SetMoveStatus(ctx, "move-test", status); err != nil {
		t.Fatal(err)
	}
	move, err := registry.GetMove(ctx, "move-test")
	if err != nil {
		t.Fatal(err)
	}
	if move.Status.Phase != "Copying" || move.Status.DestinationNode != "node-b" || !move.Status.EvictionRequested {
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
	if err := registry.Delete(ctx, volumeID); err != nil {
		t.Fatal(err)
	}
	if err := registry.Delete(ctx, volumeID); err != nil {
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
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "shiftpv.io/v1alpha1",
		"kind":       "ShiftPVPool",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"nodeName": nodeName, "mountPath": "/mnt/shiftpv"},
	}}
}
