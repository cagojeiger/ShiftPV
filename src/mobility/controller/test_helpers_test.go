package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/cleanupapi"
	"github.com/cagojeiger/ShiftPV/src/volume"
)

func assignJobUIDs(client *fake.Clientset) {
	for _, resource := range []string{"jobs", "pods", "services", "configmaps", "secrets"} {
		client.PrependReactor("create", resource, func(action ktesting.Action) (bool, runtime.Object, error) {
			object := action.(ktesting.CreateAction).GetObject().(metav1.Object)
			if object.GetUID() == "" {
				object.SetUID(types.UID(object.GetName() + "-uid"))
			}
			return false, nil, nil
		})
	}
}

func testCopyIdentities(volumeID, sourceNode, destinationNode string) (volume.CopyIdentity, volume.CopyIdentity, volume.CopyIdentity) {
	source := volume.CopyIdentity{
		InstallationID: "test-installation", PoolName: sourceNode + "-pool", PoolUID: sourceNode + "-pool-uid",
		VolumeID: volumeID, VolumeUID: "test-volume-uid", CopyID: "test-copy-" + sourceNode,
		NodeName: sourceNode, Role: volume.RoleServing,
	}
	incoming := volume.CopyIdentity{
		InstallationID: source.InstallationID, PoolName: destinationNode + "-pool", PoolUID: destinationNode + "-pool-uid",
		VolumeID: volumeID, VolumeUID: source.VolumeUID, CopyID: "test-incoming-" + destinationNode,
		NodeName: destinationNode, Role: volume.RoleIncoming,
	}
	destination := incoming
	destination.CopyID = "test-copy-" + destinationNode
	destination.Role = volume.RoleServing
	return source, incoming, destination
}

func newTestCleanupStore() *cleanupapi.Store {
	parent := func(name, uid string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "shiftpv.io/v1alpha1", "kind": "ShiftPVMove",
			"metadata": map[string]any{
				"name": name, "uid": uid, "resourceVersion": "1", "generation": int64(1),
				"finalizers": []any{cleanupapi.MoveProtectionFinalizer},
			},
			"spec": map[string]any{},
		}}
	}
	pool := func(name, uid, node string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "shiftpv.io/v1alpha1", "kind": "ShiftPVPool",
			"metadata": map[string]any{
				"name": name, "uid": uid, "resourceVersion": "1", "generation": int64(1),
				"finalizers": []any{cleanupapi.PoolProtectionFinalizer},
			},
			"spec": map[string]any{"nodeName": node, "scanEpoch": int64(0)},
			"status": map[string]any{
				"observedGeneration": int64(1),
				"inventory":          map[string]any{"valid": true, "truncated": false, "message": "", "copies": []any{}},
			},
		}}
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		cleanupapi.VolumeResource: "ShiftPVVolumeList",
		cleanupapi.MoveResource:   "ShiftPVMoveList",
		cleanupapi.PoolResource:   "ShiftPVPoolList",
	}, parent("move-test", "move-uid"), parent("move-cleanup", "move-cleanup-uid"),
		pool("source-pool", "source-pool-uid", "source"), pool("source", "source-pool-uid", "source"),
		pool("destination-pool", "destination-pool-uid", "destination"))
	client.PrependReactor("update", "shiftpvpools", func(action ktesting.Action) (bool, runtime.Object, error) {
		update := action.(ktesting.UpdateAction)
		if update.GetSubresource() == "" {
			object := update.GetObject().(*unstructured.Unstructured)
			object.SetGeneration(object.GetGeneration() + 1)
			_ = unstructured.SetNestedField(object.Object, object.GetGeneration(), "status", "observedGeneration")
		}
		return false, nil, nil
	})
	return &cleanupapi.Store{Client: client}
}
