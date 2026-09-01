package helperpod

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const testVolumeID = "shiftpv-0123456789abcdef0123456789abcdef"

func TestCreateRunsPinnedHelperPodAndCleansItUp(t *testing.T) {
	client, created := clientWithPodPhase(t, corev1.PodSucceeded, "")
	runner := validRunner(client)

	if err := runner.Create(context.Background(), "worker-a", testVolumeID); err != nil {
		t.Fatal(err)
	}
	if created.pod == nil {
		t.Fatal("helper Pod was not created")
	}
	if created.pod.Spec.NodeName != "worker-a" {
		t.Fatalf("unexpected node: %q", created.pod.Spec.NodeName)
	}
	command := created.pod.Spec.Containers[0].Command
	want := []string{"mkdir", "-p", "/pool/volumes/" + testVolumeID}
	if strings.Join(command, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("unexpected command: %#v", command)
	}
	hostPath := created.pod.Spec.Volumes[0].HostPath
	if hostPath == nil || hostPath.Path != "/mnt/shiftpv" || hostPath.Type == nil || *hostPath.Type != corev1.HostPathDirectory {
		t.Fatalf("unexpected HostPath: %#v", hostPath)
	}
	pods, err := client.CoreV1().Pods("shiftpv-system").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("helper Pod was not cleaned up: %d remain", len(pods.Items))
	}
}

func TestRunReportsFailedHelperPodAndCleansItUp(t *testing.T) {
	client, _ := clientWithPodPhase(t, corev1.PodFailed, "operation failed")
	runner := validRunner(client)

	err := runner.Delete(context.Background(), "worker-a", testVolumeID)
	if err == nil || !strings.Contains(err.Error(), "operation failed") {
		t.Fatalf("expected helper failure, got %v", err)
	}
	pods, listErr := client.CoreV1().Pods("shiftpv-system").List(context.Background(), metav1.ListOptions{})
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("failed helper Pod was not cleaned up: %d remain", len(pods.Items))
	}
}

func TestRunTimesOutAndCleansUpPendingHelperPod(t *testing.T) {
	client, _ := clientWithPodPhase(t, corev1.PodPending, "")
	runner := validRunner(client)
	runner.Timeout = 20 * time.Millisecond

	err := runner.Create(context.Background(), "worker-a", testVolumeID)
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("expected timeout, got %v", err)
	}
	pods, listErr := client.CoreV1().Pods("shiftpv-system").List(context.Background(), metav1.ListOptions{})
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("timed-out helper Pod was not cleaned up: %d remain", len(pods.Items))
	}
}

func TestRunRejectsIncompleteConfiguration(t *testing.T) {
	tests := map[string]Runner{
		"relative pool": {
			Client: fake.NewClientset(), Namespace: "shiftpv-system", PoolRoot: "relative", Image: "busybox", Timeout: time.Second,
		},
		"missing client": {
			Namespace: "shiftpv-system", PoolRoot: "/mnt/shiftpv", Image: "busybox", Timeout: time.Second,
		},
		"missing namespace": {
			Client: fake.NewClientset(), PoolRoot: "/mnt/shiftpv", Image: "busybox", Timeout: time.Second,
		},
		"missing image": {
			Client: fake.NewClientset(), Namespace: "shiftpv-system", PoolRoot: "/mnt/shiftpv", Timeout: time.Second,
		},
		"invalid timeout": {
			Client: fake.NewClientset(), Namespace: "shiftpv-system", PoolRoot: "/mnt/shiftpv", Image: "busybox",
		},
	}

	for name, runner := range tests {
		t.Run(name, func(t *testing.T) {
			if err := runner.Create(context.Background(), "worker-a", testVolumeID); err == nil {
				t.Fatal("expected configuration error")
			}
		})
	}
	valid := validRunner(fake.NewClientset())
	if err := valid.Create(context.Background(), "", testVolumeID); err == nil {
		t.Fatal("expected missing node error")
	}
	if err := valid.Create(context.Background(), "worker-a", "../escape"); err == nil {
		t.Fatal("expected unsafe volume ID error")
	}
}

type createdPod struct {
	pod *corev1.Pod
}

func clientWithPodPhase(t *testing.T, phase corev1.PodPhase, message string) (*fake.Clientset, *createdPod) {
	t.Helper()
	client := fake.NewClientset()
	captured := &createdPod{}
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create := action.(k8stesting.CreateAction)
		pod := create.GetObject().(*corev1.Pod).DeepCopy()
		pod.Name = "helper-1"
		pod.Namespace = action.GetNamespace()
		pod.Status.Phase = phase
		pod.Status.Message = message
		captured.pod = pod.DeepCopy()
		if err := client.Tracker().Add(pod); err != nil {
			return true, nil, err
		}
		return true, pod, nil
	})
	return client, captured
}

func validRunner(client *fake.Clientset) Runner {
	return Runner{
		Client:    client,
		Namespace: "shiftpv-system",
		PoolRoot:  "/mnt/shiftpv",
		Image:     "busybox:1.37",
		Timeout:   time.Second,
	}
}
