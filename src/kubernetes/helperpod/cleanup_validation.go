package helperpod

import (
	"reflect"

	batchv1 "k8s.io/api/batch/v1"

	"github.com/project-jelly/ShiftPV/src/kubernetes/cleanupapi"
)

// fieldCheck is one row of an ordered comparison table: the name a mismatch is
// reported under, and the comparison itself. Rows are evaluated in order and
// stop at the first false one, so a later row may rely on an earlier row having
// fixed a length or excluded a nil.
type fieldCheck struct {
	field string
	same  func() bool
}

// firstDifference returns the name of the first row that does not hold, or ""
// when every row holds.
func firstDifference(checks []fieldCheck) string {
	for _, check := range checks {
		if !check.same() {
			return check.field
		}
	}
	return ""
}

func sameCleanupJob(current, expected *batchv1.Job) bool {
	return cleanupJobDifference(current, expected) == ""
}

// cleanupJobDifference names the first field in which a live Job departs from
// the approved cleanup effect, or returns "" when the Job is exactly that
// effect. The three tables keep the original comparison order, so the effect
// rows may index the first container and Volume: the shape rows have already
// fixed both counts.
func cleanupJobDifference(current, expected *batchv1.Job) string {
	if current == nil || expected == nil {
		return "job"
	}
	if field := firstDifference(cleanupJobIdentityChecks(current, expected)); field != "" {
		return field
	}
	if field := firstDifference(cleanupJobShapeChecks(current, expected)); field != "" {
		return field
	}
	return firstDifference(cleanupJobEffectChecks(current, expected))
}

func cleanupJobIdentityChecks(current, expected *batchv1.Job) []fieldCheck {
	return []fieldCheck{
		{"metadata.labels[" + cleanupUIDLabel + "]", func() bool {
			return current.Labels[cleanupUIDLabel] == expected.Labels[cleanupUIDLabel]
		}},
		{"metadata.labels[" + cleanupNameLabel + "]", func() bool {
			return current.Labels[cleanupNameLabel] == expected.Labels[cleanupNameLabel]
		}},
		{"metadata.deletionTimestamp", func() bool { return current.DeletionTimestamp == nil }},
		{"metadata.namespace", func() bool { return current.Namespace == expected.Namespace }},
		{"metadata.name", func() bool { return current.Name == expected.Name }},
		{"metadata.ownerReferences", func() bool {
			return reflect.DeepEqual(current.OwnerReferences, expected.OwnerReferences)
		}},
	}
}

// cleanupJobShapeChecks holds the executor to one Pod attempt of the approved
// shape: no fan-out, no alternative completion or failure handling, no foreign
// controller, and no extra container or Volume.
func cleanupJobShapeChecks(current, expected *batchv1.Job) []fieldCheck {
	currentPod, expectedPod := current.Spec.Template.Spec, expected.Spec.Template.Spec
	return []fieldCheck{
		{"spec.parallelism", func() bool { return oneOrDefault(current.Spec.Parallelism) }},
		{"spec.completions", func() bool { return oneOrDefault(current.Spec.Completions) }},
		{"spec.manualSelector", func() bool {
			return current.Spec.ManualSelector == nil || !*current.Spec.ManualSelector
		}},
		{"spec.completionMode", func() bool {
			return current.Spec.CompletionMode == nil || *current.Spec.CompletionMode == batchv1.NonIndexedCompletion
		}},
		{"spec.podFailurePolicy", func() bool { return current.Spec.PodFailurePolicy == nil }},
		{"spec.successPolicy", func() bool { return current.Spec.SuccessPolicy == nil }},
		{"spec.backoffLimitPerIndex", func() bool { return current.Spec.BackoffLimitPerIndex == nil }},
		{"spec.maxFailedIndexes", func() bool { return current.Spec.MaxFailedIndexes == nil }},
		{"spec.managedBy", func() bool {
			return current.Spec.ManagedBy == nil || *current.Spec.ManagedBy == batchv1.JobControllerName
		}},
		{"spec.template.labels[" + cleanupUIDLabel + "]", func() bool {
			return current.Spec.Template.Labels[cleanupUIDLabel] == expected.Spec.Template.Labels[cleanupUIDLabel]
		}},
		{"spec.template.labels[" + cleanupNameLabel + "]", func() bool {
			return current.Spec.Template.Labels[cleanupNameLabel] == expected.Spec.Template.Labels[cleanupNameLabel]
		}},
		{"spec.template.spec.nodeName", func() bool { return currentPod.NodeName == expectedPod.NodeName }},
		{"spec.template.spec.serviceAccountName", func() bool {
			return currentPod.ServiceAccountName == expectedPod.ServiceAccountName
		}},
		{"spec.template.spec.restartPolicy", func() bool { return currentPod.RestartPolicy == expectedPod.RestartPolicy }},
		{"spec.template.spec.hostPID", func() bool { return !currentPod.HostPID }},
		{"spec.template.spec.hostIPC", func() bool { return !currentPod.HostIPC }},
		{"spec.template.spec.hostNetwork", func() bool { return !currentPod.HostNetwork }},
		{"spec.template.spec.initContainers", func() bool { return len(currentPod.InitContainers) == 0 }},
		{"spec.template.spec.ephemeralContainers", func() bool { return len(currentPod.EphemeralContainers) == 0 }},
		{"spec.template.spec.containers", func() bool {
			return len(currentPod.Containers) == 1 && len(expectedPod.Containers) == 1
		}},
		{"spec.template.spec.volumes", func() bool {
			return len(currentPod.Volumes) == 1 && len(expectedPod.Volumes) == 1
		}},
	}
}

// cleanupJobEffectChecks compares what the single container would actually do:
// the command and its identity-bound arguments, the Pool mount, and the retry
// and deadline budget the effect was approved with.
func cleanupJobEffectChecks(current, expected *batchv1.Job) []fieldCheck {
	currentPod, expectedPod := current.Spec.Template.Spec, expected.Spec.Template.Spec
	currentContainer, expectedContainer := currentPod.Containers[0], expectedPod.Containers[0]
	return []fieldCheck{
		{"spec.template.spec.containers[0].name", func() bool { return currentContainer.Name == expectedContainer.Name }},
		{"spec.template.spec.containers[0].image", func() bool { return currentContainer.Image == expectedContainer.Image }},
		{"spec.template.spec.containers[0].command", func() bool {
			return reflect.DeepEqual(currentContainer.Command, expectedContainer.Command)
		}},
		{"spec.template.spec.containers[0].args", func() bool {
			return reflect.DeepEqual(currentContainer.Args, expectedContainer.Args)
		}},
		{"spec.template.spec.containers[0].env", func() bool {
			return reflect.DeepEqual(currentContainer.Env, expectedContainer.Env)
		}},
		{"spec.template.spec.containers[0].envFrom", func() bool { return len(currentContainer.EnvFrom) == 0 }},
		{"spec.template.spec.containers[0].resources", func() bool {
			return reflect.DeepEqual(currentContainer.Resources, expectedContainer.Resources)
		}},
		{"spec.template.spec.containers[0].securityContext", func() bool {
			return reflect.DeepEqual(currentContainer.SecurityContext, expectedContainer.SecurityContext)
		}},
		{"spec.template.spec.containers[0].volumeMounts", func() bool {
			return reflect.DeepEqual(currentContainer.VolumeMounts, expectedContainer.VolumeMounts)
		}},
		{"spec.template.spec.containers[0].volumeDevices", func() bool { return len(currentContainer.VolumeDevices) == 0 }},
		{"spec.template.spec.containers[0].lifecycle", func() bool { return currentContainer.Lifecycle == nil }},
		{"spec.template.spec.containers[0].livenessProbe", func() bool { return currentContainer.LivenessProbe == nil }},
		{"spec.template.spec.containers[0].readinessProbe", func() bool { return currentContainer.ReadinessProbe == nil }},
		{"spec.template.spec.containers[0].startupProbe", func() bool { return currentContainer.StartupProbe == nil }},
		{"spec.template.spec.volumes[0]", func() bool { return reflect.DeepEqual(currentPod.Volumes, expectedPod.Volumes) }},
		{"spec.backoffLimit", func() bool {
			return reflect.DeepEqual(current.Spec.BackoffLimit, expected.Spec.BackoffLimit)
		}},
		{"spec.activeDeadlineSeconds", func() bool {
			return reflect.DeepEqual(current.Spec.ActiveDeadlineSeconds, expected.Spec.ActiveDeadlineSeconds)
		}},
	}
}

func oneOrDefault(value *int32) bool { return value == nil || *value == 1 }

func sameBoundExecutor(current, expected *cleanupapi.Executor) bool {
	return current != nil && expected != nil && current.JobName == expected.JobName && current.JobUID == expected.JobUID &&
		current.NodeName == expected.NodeName && (expected.PodUID == "" || current.PodUID == expected.PodUID)
}
