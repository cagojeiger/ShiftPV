package helperpod

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/volume"
)

func TestCreationAcceptedResponseLossReusesOnePod(t *testing.T) {
	client := fake.NewClientset()
	createCalls := 0
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if createCalls > 0 {
			return false, nil, nil
		}
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
		pod.Namespace = action.GetNamespace()
		pod.UID = "creation-pod-uid"
		pod.Status.Phase = corev1.PodRunning
		if err := client.Tracker().Add(pod); err != nil {
			return true, nil, err
		}
		createCalls++
		return true, nil, apierrors.NewTimeoutError("accepted, response lost", 1)
	})
	runner := validRunner(client)
	runner.ServiceAccountName = "shiftpv-controller"
	runner.Timeout = 10 * time.Millisecond
	identity := creationIdentity()
	if err := runner.CreateCopy(context.Background(), identity); !isRetryable(err) {
		t.Fatalf("ambiguous running helper was not retryable: %v", err)
	}
	pods, err := client.CoreV1().Pods(runner.Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("accepted creation helper was lost or duplicated: pods=%d err=%v", len(pods.Items), err)
	}
	completed := pods.Items[0].DeepCopy()
	completed.Status.Phase = corev1.PodSucceeded
	completed.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "operation", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
	}}
	if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), completed, runner.Namespace); err != nil {
		t.Fatal(err)
	}
	runner.Timeout = time.Second
	if err := runner.CreateCopy(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	if createCalls != 1 {
		t.Fatalf("created %d helper incarnations, want 1", createCalls)
	}
	if err := runner.FinalizeCreate(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	if pods, err = client.CoreV1().Pods(runner.Namespace).List(context.Background(), metav1.ListOptions{}); err != nil || len(pods.Items) != 0 {
		t.Fatalf("settled creation helper remains: pods=%d err=%v", len(pods.Items), err)
	}
}

func TestCreationSuccessRequiresTerminationBeforeFinalization(t *testing.T) {
	client, captured := clientWithPodPhase(t, corev1.PodSucceeded, "")
	client.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		object, err := client.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), action.GetNamespace(), action.(k8stesting.GetAction).GetName())
		if err != nil {
			return true, nil, err
		}
		pod := object.(*corev1.Pod).DeepCopy()
		pod.Status.ContainerStatuses = nil
		return true, pod, nil
	})
	runner := validRunner(client)
	runner.ServiceAccountName = "shiftpv-controller"
	if err := runner.CreateCopy(context.Background(), creationIdentity()); !isRetryable(err) {
		t.Fatalf("missing termination evidence was accepted: %v", err)
	}
	if captured.pod == nil {
		t.Fatal("creation helper was not created")
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "delete" {
			t.Fatal("helper without termination evidence was deleted")
		}
	}
}

func TestCreationFailureDeletesOnlyExactUID(t *testing.T) {
	client, _ := clientWithPodPhase(t, corev1.PodFailed, "failed")
	deleted := false
	client.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		options := action.(k8stesting.DeleteAction).GetDeleteOptions()
		if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != "helper-uid" {
			t.Fatalf("creation helper delete precondition=%#v", options.Preconditions)
		}
		deleted = true
		return false, nil, nil
	})
	runner := validRunner(client)
	runner.ServiceAccountName = "shiftpv-controller"
	if err := runner.CreateCopy(context.Background(), creationIdentity()); !isRetryable(err) || !deleted {
		t.Fatalf("failed creation executor was not retired safely: deleted=%v err=%v", deleted, err)
	}
}

func TestCreationRejectsChangedExistingPodWithoutDeletingIt(t *testing.T) {
	client, captured := clientWithPodPhase(t, corev1.PodSucceeded, "")
	runner := validRunner(client)
	runner.ServiceAccountName = "shiftpv-controller"
	identity := creationIdentity()
	if err := runner.CreateCopy(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	pod := captured.pod.DeepCopy()
	pod.Spec.Containers[0].Command = []string{"/bin/sh", "-c", "touch /pool/foreign"}
	if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), pod, runner.Namespace); err != nil {
		t.Fatal(err)
	}
	if err := runner.CreateCopy(context.Background(), identity); err == nil || !strings.Contains(err.Error(), "command changed") {
		t.Fatalf("changed helper was accepted: %v", err)
	}
	if _, err := client.CoreV1().Pods(runner.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("changed helper was deleted: %v", err)
	}
}

// TestCreationPodDifferenceLocalizesEveryComparedField flips one compared field
// at a time. Every row must reject its own change, report itself, and keep the
// error text its group is reported under; an unchanged Pod must be accepted.
func TestCreationPodDifferenceLocalizesEveryComparedField(t *testing.T) {
	runner := validRunner(fake.NewClientset())
	runner.ServiceAccountName = "shiftpv-controller"
	identity := creationIdentity()
	operationID, err := volumeapi.CreationOperationID(identity.VolumeUID)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := runner.creationPod(context.Background(), identity, operationID, []string{"/shiftpv-volume-helper", "create"})
	if err != nil {
		t.Fatal(err)
	}
	settled := desired.DeepCopy()
	settled.UID = "creation-pod-uid"
	if field, err := creationPodDifference(desired, settled); field != "" || err != nil {
		t.Fatalf("desired creation helper rejected at %q: %v", field, err)
	}
	if err := sameCreationPod(nil, settled); err == nil {
		t.Fatal("missing creation helper accepted")
	}
	deleted := metav1.Now()
	const (
		shapeError      = "creation helper Pod identity or execution shape changed"
		labelError      = "creation helper Pod label changed"
		annotationError = "creation helper Pod annotation changed"
		commandError    = "creation helper Pod command changed"
		mountError      = "creation helper Pod Pool mount changed"
	)
	for _, testCase := range []struct {
		name    string
		field   string
		message string
		change  func(*corev1.Pod)
	}{
		{"uid", "metadata.uid", shapeError, func(pod *corev1.Pod) { pod.UID = "" }},
		{"deleting", "metadata.deletionTimestamp", shapeError, func(pod *corev1.Pod) { pod.DeletionTimestamp = &deleted }},
		{"name", "metadata.name", shapeError, func(pod *corev1.Pod) { pod.Name = "other-helper" }},
		{"namespace", "metadata.namespace", shapeError, func(pod *corev1.Pod) { pod.Namespace = "other-namespace" }},
		{"node", "spec.nodeName", shapeError, func(pod *corev1.Pod) { pod.Spec.NodeName = "other-node" }},
		{"account", "spec.serviceAccountName", shapeError, func(pod *corev1.Pod) { pod.Spec.ServiceAccountName = "other-account" }},
		{"restart", "spec.restartPolicy", shapeError, func(pod *corev1.Pod) { pod.Spec.RestartPolicy = corev1.RestartPolicyOnFailure }},
		{"token", "spec.automountServiceAccountToken", shapeError, func(pod *corev1.Pod) {
			pod.Spec.AutomountServiceAccountToken = nil
		}},
		{"hostNetwork", "spec.hostNetwork", shapeError, func(pod *corev1.Pod) { pod.Spec.HostNetwork = true }},
		{"hostPID", "spec.hostPID", shapeError, func(pod *corev1.Pod) { pod.Spec.HostPID = true }},
		{"hostIPC", "spec.hostIPC", shapeError, func(pod *corev1.Pod) { pod.Spec.HostIPC = true }},
		{"containers", "spec.containers", shapeError, func(pod *corev1.Pod) {
			pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "sidecar"})
		}},
		{"initContainers", "spec.initContainers", shapeError, func(pod *corev1.Pod) {
			pod.Spec.InitContainers = []corev1.Container{{Name: "init"}}
		}},
		{"ephemeralContainers", "spec.ephemeralContainers", shapeError, func(pod *corev1.Pod) {
			pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{{}}
		}},
		{"label", "metadata.labels[shiftpv.io/volume-id]", labelError, func(pod *corev1.Pod) {
			pod.Labels["shiftpv.io/volume-id"] = "other-volume"
		}},
		{"annotation", "metadata.annotations[" + creationCopyIDAnnotation + "]", annotationError, func(pod *corev1.Pod) {
			pod.Annotations[creationCopyIDAnnotation] = "other-copy"
		}},
		{"containerName", "spec.containers[0].name", commandError, func(pod *corev1.Pod) { pod.Spec.Containers[0].Name = "other" }},
		{"image", "spec.containers[0].image", commandError, func(pod *corev1.Pod) { pod.Spec.Containers[0].Image = "other:tag" }},
		{"command", "spec.containers[0].command", commandError, func(pod *corev1.Pod) {
			pod.Spec.Containers[0].Command = []string{"/bin/sh", "-c", "touch /pool/foreign"}
		}},
		{"args", "spec.containers[0].args", commandError, func(pod *corev1.Pod) {
			pod.Spec.Containers[0].Args = []string{"--pool-root=/other"}
		}},
		{"env", "spec.containers[0].env", commandError, func(pod *corev1.Pod) {
			pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "POOL_ROOT", Value: "/other"}}
		}},
		{"envFrom", "spec.containers[0].envFrom", commandError, func(pod *corev1.Pod) {
			pod.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{Prefix: "SHIFTPV_"}}
		}},
		{"resources", "spec.containers[0].resources", commandError, func(pod *corev1.Pod) {
			pod.Spec.Containers[0].Resources.Requests = corev1.ResourceList{}
		}},
		{"securityContext", "spec.containers[0].securityContext", commandError, func(pod *corev1.Pod) {
			pod.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation = boolPtr(true)
		}},
		{"poolRoot", "spec.volumes[pool]", mountError, func(pod *corev1.Pod) { pod.Spec.Volumes[0].HostPath.Path = "/other" }},
		{"mountPath", "spec.volumes[pool]", mountError, func(pod *corev1.Pod) {
			pod.Spec.Containers[0].VolumeMounts[0].MountPath = "/other"
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			current := settled.DeepCopy()
			testCase.change(current)
			field, err := creationPodDifference(desired, current)
			if field != testCase.field {
				t.Fatalf("difference = %q, want %q", field, testCase.field)
			}
			if err == nil || err.Error() != testCase.message {
				t.Fatalf("error = %v, want %q", err, testCase.message)
			}
			if got := sameCreationPod(desired, current); got == nil || got.Error() != testCase.message {
				t.Fatalf("changed creation helper accepted: %v", got)
			}
		})
	}
}

func creationIdentity() volume.CopyIdentity {
	return volume.CopyIdentity{
		InstallationID: "installation", PoolName: "pool-a", PoolUID: "pool-uid",
		VolumeID: testVolumeID, VolumeUID: "volume-uid", CopyID: "copy-id",
		NodeName: "worker-a", Role: volume.RoleServing,
	}
}
