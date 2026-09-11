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

func creationIdentity() volume.CopyIdentity {
	return volume.CopyIdentity{
		InstallationID: "installation", PoolName: "pool-a", PoolUID: "pool-uid",
		VolumeID: testVolumeID, VolumeUID: "volume-uid", CopyID: "copy-id",
		NodeName: "worker-a", Role: volume.RoleServing,
	}
}
