package controller

import (
	"context"
	"fmt"
	"path/filepath"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/mobility/fsm"
)

type observation struct {
	FSM             fsm.Observation
	Volume          volumeapi.State
	PV              *corev1.PersistentVolume
	Claim           *corev1.PersistentVolumeClaim
	Consumer        *corev1.Pod
	Replacement     *corev1.Pod
	DestinationNode string
	CandidateNodes  []string
	Names           resourceNames
}

func (r *Reconciler) observe(ctx context.Context, move volumeapi.Move) (observation, error) {
	result := observation{Names: namesFor(move.Name)}
	state, err := r.Repository.Get(ctx, move.Spec.VolumeID)
	if err != nil {
		return result, err
	}
	result.Volume = state
	result.DestinationNode = move.Status.DestinationNode
	result.FSM.OwnerCommitted = result.DestinationNode != "" && state.Phase == volumeapi.PhaseReady && state.OwnerNode == result.DestinationNode && state.ActiveMove == move.Name

	pools, err := r.Repository.Pools(ctx)
	if err != nil {
		return result, err
	}
	poolNodes := make(map[string]volumeapi.Pool, len(pools))
	for _, pool := range pools {
		if pool.NodeName == "" || !filepath.IsAbs(pool.MountPath) || filepath.Clean(pool.MountPath) == "/" {
			return result, fmt.Errorf("ShiftPVPool %q has invalid nodeName or mountPath", pool.Name)
		}
		if _, duplicate := poolNodes[pool.NodeName]; duplicate {
			return result, fmt.Errorf("multiple ShiftPVPools are registered for node %q", pool.NodeName)
		}
		poolNodes[pool.NodeName] = pool
	}
	sourceNode, err := r.Client.CoreV1().Nodes().Get(ctx, move.Spec.SourceNode, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return result, fmt.Errorf("read source Node: %w", err)
	}
	_, sourceRegistered := poolNodes[move.Spec.SourceNode]
	sourceHealthy := err == nil && nodeReady(sourceNode) && sourceRegistered
	result.FSM.SourceHealthy = sourceHealthy
	if !sourceHealthy {
		result.FSM.UnsafeReason = "SourceUnavailable"
	}

	for nodeName := range poolNodes {
		if nodeName == move.Spec.SourceNode {
			continue
		}
		node, nodeErr := r.Client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if nodeErr != nil {
			if apierrors.IsNotFound(nodeErr) {
				continue
			}
			return result, fmt.Errorf("read destination Node %q: %w", nodeName, nodeErr)
		}
		if nodeReady(node) && !node.Spec.Unschedulable {
			result.CandidateNodes = append(result.CandidateNodes, nodeName)
		}
	}

	persistentVolumes, err := r.Client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return result, fmt.Errorf("list PersistentVolumes: %w", err)
	}
	for index := range persistentVolumes.Items {
		candidate := &persistentVolumes.Items[index]
		if candidate.Spec.CSI != nil && candidate.Spec.CSI.Driver == "csi.shiftpv.io" && candidate.Spec.CSI.VolumeHandle == move.Spec.VolumeID {
			result.PV = candidate.DeepCopy()
			break
		}
	}
	if result.PV == nil || result.PV.Spec.ClaimRef == nil {
		result.FSM.UnsafeReason = "VolumeBindingMissing"
		result.FSM.SourceAuthorityInvalid = true
		return result, nil
	}
	claimRef := result.PV.Spec.ClaimRef
	claim, err := r.Client.CoreV1().PersistentVolumeClaims(claimRef.Namespace).Get(ctx, claimRef.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			result.FSM.UnsafeReason = "VolumeBindingMissing"
			result.FSM.SourceAuthorityInvalid = true
			return result, nil
		}
		return result, fmt.Errorf("read PVC: %w", err)
	}
	result.Claim = claim
	// Names can be reused after PVC/namespace deletion while Retain PVs and
	// ShiftPVVolumes survive. Never associate that old volume with the new Pod.
	if claim.UID == "" || claimRef.UID != claim.UID || claim.Spec.VolumeName != result.PV.Name ||
		claim.DeletionTimestamp != nil || result.PV.DeletionTimestamp != nil {
		result.FSM.UnsafeReason = "VolumeBindingMismatch"
		result.FSM.SourceAuthorityInvalid = true
		return result, nil
	}
	if state.OwnerNode != move.Spec.SourceNode && !result.FSM.OwnerCommitted {
		result.FSM.UnsafeReason = "OwnerMismatch"
		result.FSM.SourceAuthorityInvalid = true
		return result, nil
	}
	namespace, err := r.Client.CoreV1().Namespaces().Get(ctx, claim.Namespace, metav1.GetOptions{})
	if err != nil {
		return result, fmt.Errorf("read workload Namespace: %w", err)
	}
	if namespace.Labels[admissionNamespaceLabel] != "enabled" {
		result.FSM.UnsafeReason = "AdmissionNotEnabled"
		result.FSM.PreflightDeferred = preEviction(move)
		return result, nil
	}

	pods, err := r.Client.CoreV1().Pods(claim.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return result, fmt.Errorf("list consumer Pods: %w", err)
	}
	for index := range pods.Items {
		pod := &pods.Items[index]
		if !podUsesClaim(pod, claim.Name) || terminalPod(pod) {
			continue
		}
		if move.Status.ConsumerName != "" && pod.Name == move.Status.ConsumerName &&
			(move.Status.ConsumerUID == "" || string(pod.UID) == move.Status.ConsumerUID) {
			result.Consumer = pod.DeepCopy()
			continue
		}
		if move.Status.ConsumerName == "" && pod.Spec.NodeName == move.Spec.SourceNode {
			if result.Consumer != nil {
				result.FSM.UnsafeReason = "MultipleConsumers"
				result.FSM.PreflightDeferred = preEviction(move)
				return result, nil
			}
			result.Consumer = pod.DeepCopy()
			continue
		}
		if result.Replacement == nil {
			result.Replacement = pod.DeepCopy()
		}
	}

	sourceCordoned := sourceNode != nil && sourceNode.Spec.Unschedulable
	preconditions := sourceHealthy && !result.FSM.SourceAuthorityInvalid && sourceCordoned && len(result.CandidateNodes) > 0 && result.Consumer != nil && metav1.GetControllerOf(result.Consumer) != nil
	// The original consumer is expected to disappear after eviction. Eligibility
	// diagnostics must not mask CopyFailed/PromotionFailed/CleanupFailed later.
	preflight := move.Status.Phase == "" || move.Status.Phase == string(fsm.PhasePending)
	if preflight && !preconditions && result.FSM.UnsafeReason == "" {
		switch {
		case result.Consumer == nil:
			result.FSM.UnsafeReason = "ControlledConsumerMissing"
		case metav1.GetControllerOf(result.Consumer) == nil:
			result.FSM.UnsafeReason = "BarePodUnsupported"
		case len(result.CandidateNodes) == 0:
			result.FSM.UnsafeReason = "DestinationUnavailable"
		case !sourceCordoned:
			result.FSM.UnsafeReason = "SourceNotCordoned"
		}
	}
	result.FSM.PreconditionsValid = preconditions
	if preEviction(move) && result.Consumer != nil && sourceHealthy {
		reason, err := r.preflight(ctx, &result)
		if err != nil {
			return result, err
		}
		if reason == "" && !preconditions {
			reason = result.FSM.UnsafeReason
			if reason == "" {
				reason = "PreconditionFailed"
			}
		}
		if reason != "" {
			result.FSM.PreconditionsValid = false
			result.FSM.PreflightDeferred = true
			result.FSM.UnsafeReason = reason
		}
	}
	result.FSM.VolumeLocked = state.Phase == volumeapi.PhaseMoving && state.ActiveMove == move.Name && state.OwnerNode == move.Spec.SourceNode
	result.FSM.ConsumerExists = result.Consumer != nil
	result.FSM.EvictionRequested = move.Status.EvictionRequested
	result.FSM.PublishedOnSource = contains(state.PublishedNodes, move.Spec.SourceNode)
	result.FSM.ReplacementExists = result.Replacement != nil
	result.FSM.ReplacementHeld = result.Replacement != nil && hasPlacementHold(result.Replacement)

	if result.Replacement != nil && !preEviction(move) {
		if selected := result.Replacement.Spec.NodeSelector["kubernetes.io/hostname"]; selected != "" && !contains(result.CandidateNodes, selected) {
			result.FSM.DestinationBlocked = true
			result.FSM.UnsafeReason = "UnsupportedSchedulingConstraint"
		}
		if result.Replacement.Spec.NodeName != "" {
			if contains(result.CandidateNodes, result.Replacement.Spec.NodeName) {
				result.DestinationNode = result.Replacement.Spec.NodeName
				result.FSM.DestinationScheduled = true
			} else {
				result.FSM.DestinationBlocked = true
				result.FSM.UnsafeReason = "InvalidDestination"
			}
		}
	}
	result.FSM.CopyComplete, result.FSM.CopyFailed, err = r.jobState(ctx, result.Names.CopyJob)
	if err != nil {
		return result, err
	}
	result.FSM.PromotionComplete, result.FSM.PromotionFailed, err = r.jobState(ctx, result.Names.PromotionJob)
	if err != nil {
		return result, err
	}
	result.FSM.PublishedOnDestination = result.DestinationNode != "" && contains(state.PublishedNodes, result.DestinationNode)
	result.FSM.CleanupComplete, result.FSM.CleanupFailed, err = r.jobState(ctx, result.Names.CleanupJob)
	if err != nil {
		return result, err
	}
	return result, nil
}

func (r *Reconciler) jobState(ctx context.Context, name string) (complete, failed bool, err error) {
	job, err := r.Client.BatchV1().Jobs(r.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("read Job %q: %w", name, err)
	}
	for _, condition := range job.Status.Conditions {
		switch condition.Type {
		case batchv1.JobComplete:
			complete = condition.Status == corev1.ConditionTrue
		case batchv1.JobFailed:
			failed = condition.Status == corev1.ConditionTrue
		}
	}
	return complete, failed, nil
}

func nodeReady(node *corev1.Node) bool {
	if node == nil {
		return false
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func podUsesClaim(pod *corev1.Pod, claimName string) bool {
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == claimName {
			return true
		}
	}
	return false
}

func terminalPod(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

func hasPlacementHold(pod *corev1.Pod) bool {
	for _, gate := range pod.Spec.SchedulingGates {
		if gate.Name == placementHoldName {
			return true
		}
	}
	return false
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
