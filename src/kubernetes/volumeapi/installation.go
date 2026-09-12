package volumeapi

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const installationNamespace = "kube-system"

// InstallationID follows the Kubernetes cluster incarnation, so a Helm
// reinstall can resume retained Pools while a different cluster cannot adopt
// their node-local identities silently.
func (r *Registry) InstallationID(ctx context.Context) (string, error) {
	if err := r.validate(); err != nil {
		return "", err
	}
	namespaceResource := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	namespace, err := r.Client.Resource(namespaceResource).Get(ctx, installationNamespace, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read ShiftPV installation identity: %w", err)
	}
	if namespace.GetUID() == "" || namespace.GetDeletionTimestamp() != nil {
		return "", fmt.Errorf("%w: cluster identity is missing or terminating", ErrStateConflict)
	}
	return string(namespace.GetUID()), nil
}
