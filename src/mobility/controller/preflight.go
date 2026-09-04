package controller

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
	"k8s.io/klog/v2"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
)

// preflight only rejects known constraints. It neither reserves scheduler resources
// nor predicts mutations that future admission/workload reconciliation may apply.
func (r *Reconciler) preflight(ctx context.Context, observed *observation) (string, error) {
	pod := observed.Consumer
	if pod == nil || pod.DeletionTimestamp != nil {
		return "ConsumerUnavailable", nil
	}
	if observed.Replacement != nil {
		return "MultipleConsumers", nil
	}
	template, reason, err := r.consumerTemplate(ctx, pod)
	if err != nil || reason != "" {
		return reason, err
	}
	if len(pvcNames(pod.Spec)) != 1 {
		return "MultiplePVCsUnsupported", nil
	}
	if reason := unsupportedPlacement(pod.Spec); reason != "" {
		return reason, nil
	}
	if reason := unsupportedPlacement(template.Spec); reason != "" {
		return reason, nil
	}
	if template.Spec.NodeName != "" {
		return "ExplicitNodeNameUnsupported", nil
	}
	// A controller's template is the source of the next Pod, not the scheduler's
	// assigned nodeName on the current Pod. Preserve all other live constraints.
	live := pod.DeepCopy()
	if live.Annotations["shiftpv.io/placement"] == "owner" &&
		template.Spec.NodeSelector[corev1.LabelHostname] == "" &&
		live.Spec.NodeSelector[corev1.LabelHostname] == observed.Volume.OwnerNode {
		delete(live.Spec.NodeSelector, corev1.LabelHostname)
	}
	liveAffinity := nodeaffinity.GetRequiredNodeAffinity(live)
	templateAffinity := nodeaffinity.GetRequiredNodeAffinity(&corev1.Pod{Spec: template.Spec})
	var pvAffinity *nodeaffinity.NodeSelector
	if observed.PV.Spec.NodeAffinity != nil && observed.PV.Spec.NodeAffinity.Required != nil {
		pvAffinity, err = nodeaffinity.NewNodeSelector(observed.PV.Spec.NodeAffinity.Required)
		if err != nil {
			return "InvalidPVNodeAffinity", nil
		}
	}
	var candidates []string
	for _, nodeName := range observed.CandidateNodes {
		node, err := r.Client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("preflight destination Node: %w", err)
		}
		liveMatch, liveErr := liveAffinity.Match(node)
		templateMatch, templateErr := templateAffinity.Match(node)
		if liveErr != nil || templateErr != nil {
			return "InvalidNodeAffinity", nil
		}
		if nodeReady(node) && !node.Spec.Unschedulable && liveMatch && templateMatch &&
			(pvAffinity == nil || pvAffinity.Match(node)) && toleratesPlacement(live.Spec, node) && toleratesPlacement(template.Spec, node) {
			candidates = append(candidates, nodeName)
		}
	}
	if len(candidates) == 0 {
		return "NoCompatibleDestination", nil
	}
	sort.Strings(candidates)
	observed.CandidateNodes = candidates
	return r.preflightPDB(ctx, pod)
}

func (r *Reconciler) consumerTemplate(ctx context.Context, pod *corev1.Pod) (*corev1.PodTemplateSpec, string, error) {
	owner := metav1.GetControllerOf(pod)
	if owner == nil {
		return nil, "BarePodUnsupported", nil
	}
	var object metav1.Object
	var template *corev1.PodTemplateSpec
	var err error
	if owner.APIVersion == "apps/v1" {
		switch owner.Kind {
		case "ReplicaSet":
			rs, getErr := r.Client.AppsV1().ReplicaSets(pod.Namespace).Get(ctx, owner.Name, metav1.GetOptions{})
			err = getErr
			if err == nil {
				object, template = rs, &rs.Spec.Template
			}
		case "StatefulSet":
			sts, getErr := r.Client.AppsV1().StatefulSets(pod.Namespace).Get(ctx, owner.Name, metav1.GetOptions{})
			err = getErr
			if err == nil {
				object, template = sts, &sts.Spec.Template
			}
		}
	}
	if apierrors.IsNotFound(err) {
		return nil, "ConsumerControllerMissing", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("read consumer controller: %w", err)
	}
	if object == nil {
		return nil, "UnsupportedWorkloadController", nil
	}
	if owner.UID == "" || object.GetUID() != owner.UID || object.GetDeletionTimestamp() != nil {
		return nil, "ConsumerControllerChanged", nil
	}
	return template, "", nil
}

func unsupportedPlacement(spec corev1.PodSpec) string {
	if spec.SchedulerName != "" && spec.SchedulerName != corev1.DefaultSchedulerName {
		return "CustomSchedulerUnsupported"
	}
	if len(spec.SchedulingGates) != 0 {
		return "SchedulingGateUnsupported"
	}
	if a := spec.Affinity; a != nil {
		if (a.PodAffinity != nil && len(a.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 0) ||
			(a.PodAntiAffinity != nil && len(a.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 0) {
			return "InterPodAffinityUnsupported"
		}
	}
	for _, spread := range spec.TopologySpreadConstraints {
		if spread.WhenUnsatisfiable == corev1.DoNotSchedule {
			return "HardTopologySpreadUnsupported"
		}
	}
	return ""
}

func toleratesPlacement(spec corev1.PodSpec, node *corev1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		tolerated := false
		for _, toleration := range spec.Tolerations {
			if toleration.ToleratesTaint(klog.Background(), &taint, false) {
				tolerated = true
				break
			}
		}
		if !tolerated {
			return false
		}
	}
	return true
}

func (r *Reconciler) preflightPDB(ctx context.Context, pod *corev1.Pod) (string, error) {
	budgets, err := r.Client.PolicyV1().PodDisruptionBudgets(pod.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("preflight PodDisruptionBudgets: %w", err)
	}
	matched := 0
	for _, budget := range budgets.Items {
		selector, err := metav1.LabelSelectorAsSelector(budget.Spec.Selector)
		if err != nil {
			return "InvalidPodDisruptionBudget", nil
		}
		if !selector.Matches(labels.Set(pod.Labels)) {
			continue
		}
		matched++
		if budget.Status.ObservedGeneration < budget.Generation || budget.Status.DisruptionsAllowed < 1 {
			return "DisruptionBudgetDenied", nil
		}
	}
	if matched > 1 {
		return "MultipleDisruptionBudgets", nil
	}
	return "", nil
}

func pvcNames(spec corev1.PodSpec) map[string]bool {
	names := make(map[string]bool)
	for _, v := range spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			names[v.PersistentVolumeClaim.ClaimName] = true
		}
	}
	return names
}

func preEviction(move volumeapi.Move) bool {
	return !move.Status.EvictionRequested && (move.Status.Phase == "" || move.Status.Phase == "Pending" ||
		move.Status.Phase == "Locking" || move.Status.Phase == "Evicting")
}
