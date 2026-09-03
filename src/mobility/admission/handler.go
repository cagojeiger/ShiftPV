package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
)

const (
	DriverName             = "csi.shiftpv.io"
	HostnameLabel          = "kubernetes.io/hostname"
	PlacementHold          = "shiftpv.io/placement-hold"
	PlacementKey           = "shiftpv.io/placement"
	MobilityNamespaceLabel = "shiftpv.io/admission"
)

type VolumeReader interface {
	Get(context.Context, string) (volumeapi.State, error)
}

type Handler struct {
	Client  kubernetes.Interface
	Volumes VolumeReader
}

type patchOperation struct {
	Operation string `json:"op"`
	Path      string `json:"path"`
	Value     any    `json:"value,omitempty"`
}

func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	var review admissionv1.AdmissionReview
	if err := json.NewDecoder(request.Body).Decode(&review); err != nil {
		http.Error(writer, fmt.Sprintf("decode AdmissionReview: %v", err), http.StatusBadRequest)
		return
	}
	response := h.Admit(request.Context(), review.Request)
	responseReview := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: admissionv1.SchemeGroupVersion.String(), Kind: "AdmissionReview"},
		Response: response,
	}
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(responseReview); err != nil {
		http.Error(writer, fmt.Sprintf("encode AdmissionReview: %v", err), http.StatusInternalServerError)
	}
}

func (h *Handler) Admit(ctx context.Context, request *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	if request == nil {
		return denied("AdmissionReview has no request")
	}
	response := &admissionv1.AdmissionResponse{UID: request.UID, Allowed: true}
	if request.Operation != admissionv1.Create || request.Resource.Group != "" || request.Resource.Resource != "pods" {
		return response
	}
	var pod corev1.Pod
	if err := json.Unmarshal(request.Object.Raw, &pod); err != nil {
		return deniedWithUID(request.UID, fmt.Sprintf("decode Pod: %v", err))
	}
	operations, err := h.MutatePod(ctx, request.Namespace, &pod)
	if err != nil {
		return deniedWithUID(request.UID, err.Error())
	}
	if len(operations) == 0 {
		return response
	}
	patch, err := json.Marshal(operations)
	if err != nil {
		return deniedWithUID(request.UID, fmt.Sprintf("encode JSON patch: %v", err))
	}
	patchType := admissionv1.PatchTypeJSONPatch
	response.PatchType = &patchType
	response.Patch = patch
	return response
}

func (h *Handler) MutatePod(ctx context.Context, namespace string, pod *corev1.Pod) ([]patchOperation, error) {
	if h.Client == nil || h.Volumes == nil {
		return nil, fmt.Errorf("mobility admission is not configured")
	}
	var state *volumeapi.State
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim == nil {
			continue
		}
		claim, err := h.Client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, volume.PersistentVolumeClaim.ClaimName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("read PVC %s/%s: %w", namespace, volume.PersistentVolumeClaim.ClaimName, err)
		}
		if claim.Spec.VolumeName == "" {
			continue
		}
		persistentVolume, err := h.Client.CoreV1().PersistentVolumes().Get(ctx, claim.Spec.VolumeName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("read PV %s: %w", claim.Spec.VolumeName, err)
		}
		if persistentVolume.Spec.CSI == nil || persistentVolume.Spec.CSI.Driver != DriverName {
			continue
		}
		if state != nil {
			return nil, fmt.Errorf("multiple bound ShiftPV volumes are unsupported")
		}
		current, err := h.Volumes.Get(ctx, persistentVolume.Spec.CSI.VolumeHandle)
		if err != nil {
			return nil, fmt.Errorf("read ShiftPVVolume %s: %w", persistentVolume.Spec.CSI.VolumeHandle, err)
		}
		state = &current
	}
	if state == nil {
		return nil, nil
	}
	if state.OwnerNode == "" {
		return nil, fmt.Errorf("ShiftPVVolume has no owner node")
	}
	if state.Phase != volumeapi.PhaseReady {
		return holdPlacement(pod), nil
	}
	node, err := h.Client.CoreV1().Nodes().Get(ctx, state.OwnerNode, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read owner Node %s: %w", state.OwnerNode, err)
	}
	if node.Spec.Unschedulable || !NodeReady(node) {
		namespaceObject, err := h.Client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("read workload Namespace %s: %w", namespace, err)
		}
		if namespaceObject.Labels[MobilityNamespaceLabel] == "enabled" {
			return holdPlacement(pod), nil
		}
		return pinToOwner(pod, state.OwnerNode)
	}
	return pinToOwner(pod, state.OwnerNode)
}

func NodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func holdPlacement(pod *corev1.Pod) []patchOperation {
	gates := append([]corev1.PodSchedulingGate(nil), pod.Spec.SchedulingGates...)
	for _, gate := range gates {
		if gate.Name == PlacementHold {
			return annotationPatch(pod, "held")
		}
	}
	gates = append(gates, corev1.PodSchedulingGate{Name: PlacementHold})
	operations := annotationPatch(pod, "held")
	operation := "add"
	if pod.Spec.SchedulingGates != nil {
		operation = "replace"
	}
	return append(operations, patchOperation{Operation: operation, Path: "/spec/schedulingGates", Value: gates})
}

func pinToOwner(pod *corev1.Pod, ownerNode string) ([]patchOperation, error) {
	if selectedNode := pod.Spec.NodeSelector[HostnameLabel]; selectedNode != "" && selectedNode != ownerNode {
		return nil, fmt.Errorf("Pod requires node %s but ShiftPV volume is owned by %s", selectedNode, ownerNode)
	}
	operations := annotationPatch(pod, "owner")
	if pod.Spec.NodeSelector == nil {
		return append(operations, patchOperation{Operation: "add", Path: "/spec/nodeSelector", Value: map[string]string{HostnameLabel: ownerNode}}), nil
	}
	if pod.Spec.NodeSelector[HostnameLabel] == "" {
		return append(operations, patchOperation{Operation: "add", Path: "/spec/nodeSelector/kubernetes.io~1hostname", Value: ownerNode}), nil
	}
	return operations, nil
}

func annotationPatch(pod *corev1.Pod, value string) []patchOperation {
	if pod.Annotations == nil {
		return []patchOperation{{Operation: "add", Path: "/metadata/annotations", Value: map[string]string{PlacementKey: value}}}
	}
	operation := "add"
	if _, exists := pod.Annotations[PlacementKey]; exists {
		operation = "replace"
	}
	return []patchOperation{{Operation: operation, Path: "/metadata/annotations/shiftpv.io~1placement", Value: value}}
}

func denied(message string) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{Allowed: false, Result: &metav1.Status{Status: metav1.StatusFailure, Message: message, Code: http.StatusBadRequest}}
}

func deniedWithUID(uid types.UID, message string) *admissionv1.AdmissionResponse {
	response := denied(message)
	response.UID = uid
	return response
}
