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
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		cleanupapi.Resource: "ShiftPVCleanupList",
	})
	client.PrependReactor("create", "shiftpvcleanups", func(action ktesting.Action) (bool, runtime.Object, error) {
		object := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
		object.SetUID(types.UID(object.GetName() + "-uid"))
		return false, nil, nil
	})
	return &cleanupapi.Store{Client: client}
}
