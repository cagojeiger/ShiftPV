package helperpod

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
)

const testVolumeID = "shiftpv-0123456789abcdef0123456789abcdef"

type fakePoolResolver struct{ pool volumeapi.Pool }

func (f fakePoolResolver) PoolForNode(context.Context, string) (volumeapi.Pool, error) {
	return f.pool, nil
}

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

func TestCreateUsesRegisteredNodeMountPath(t *testing.T) {
	client, created := clientWithPodPhase(t, corev1.PodSucceeded, "")
	runner := validRunner(client)
	runner.Pools = fakePoolResolver{pool: volumeapi.Pool{NodeName: "worker-a", MountPath: "/srv/storage-a"}}
	if err := runner.Create(context.Background(), "worker-a", testVolumeID); err != nil {
		t.Fatal(err)
	}
	if got := created.pod.Spec.Volumes[0].HostPath.Path; got != "/srv/storage-a" {
		t.Fatalf("helper HostPath = %q, want registered mountPath", got)
	}
}

func TestRunReportsFailedHelperPodAndCleansItUp(t *testing.T) {
	client, _ := clientWithPodPhase(t, corev1.PodFailed, "operation failed")
	runner := validRunner(client)

	err := runner.Delete(context.Background(), "worker-a", testVolumeID)
	if err == nil || !strings.Contains(err.Error(), "operation failed") {
		t.Fatalf("expected helper failure, got %v", err)
	}
	var retryable interface{ Retryable() bool }
	if !errors.As(err, &retryable) || !retryable.Retryable() {
		t.Fatalf("expected retryable helper failure, got %T: %v", err, err)
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

func TestRunClassifiesPodCreateAPIErrors(t *testing.T) {
	tests := helperPodAPIErrors()
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			client := fake.NewClientset()
			client.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, test.err
			})

			runner := validRunner(client)
			err := runner.Create(context.Background(), "worker-a", testVolumeID)
			if err == nil || !strings.Contains(err.Error(), "create helper Pod") {
				t.Fatalf("expected helper Pod create error, got %v", err)
			}
			if got := isRetryable(err); got != test.retryable {
				t.Fatalf("retryable = %v, want %v for %T: %v", got, test.retryable, test.err, err)
			}
		})
	}
}

func TestRunClassifiesPodGetAPIErrors(t *testing.T) {
	tests := helperPodAPIErrors()
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			client, _ := clientWithPodPhase(t, corev1.PodRunning, "")
			client.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, test.err
			})

			runner := validRunner(client)
			err := runner.Delete(context.Background(), "worker-a", testVolumeID)
			if err == nil || !strings.Contains(err.Error(), "wait for helper Pod") {
				t.Fatalf("expected helper Pod get error, got %v", err)
			}
			if got := isRetryable(err); got != test.retryable {
				t.Fatalf("retryable = %v, want %v for %T: %v", got, test.retryable, test.err, err)
			}
		})
	}
}

func TestRunRejectsIncompleteConfiguration(t *testing.T) {
	tests := map[string]Runner{
		"relative pool": {
			Client: fake.NewClientset(), Namespace: "shiftpv-system", Pools: fakePoolResolver{pool: volumeapi.Pool{MountPath: "relative"}}, Image: "busybox", Timeout: time.Second,
		},
		"missing client": {
			Namespace: "shiftpv-system", Pools: fakePoolResolver{pool: volumeapi.Pool{MountPath: "/mnt/shiftpv"}}, Image: "busybox", Timeout: time.Second,
		},
		"missing namespace": {
			Client: fake.NewClientset(), Pools: fakePoolResolver{pool: volumeapi.Pool{MountPath: "/mnt/shiftpv"}}, Image: "busybox", Timeout: time.Second,
		},
		"missing image": {
			Client: fake.NewClientset(), Namespace: "shiftpv-system", Pools: fakePoolResolver{pool: volumeapi.Pool{MountPath: "/mnt/shiftpv"}}, Timeout: time.Second,
		},
		"invalid timeout": {
			Client: fake.NewClientset(), Namespace: "shiftpv-system", Pools: fakePoolResolver{pool: volumeapi.Pool{MountPath: "/mnt/shiftpv"}}, Image: "busybox",
		},
		"missing pool registry": {
			Client: fake.NewClientset(), Namespace: "shiftpv-system", Image: "busybox", Timeout: time.Second,
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

type apiErrorTest struct {
	err       error
	retryable bool
}

func helperPodAPIErrors() map[string]apiErrorTest {
	pods := schema.GroupResource{Resource: "pods"}
	return map[string]apiErrorTest{
		"timeout":             {err: apierrors.NewTimeoutError("timed out", 1), retryable: true},
		"server timeout":      {err: apierrors.NewServerTimeout(pods, "get", 1), retryable: true},
		"too many requests":   {err: apierrors.NewTooManyRequests("slow down", 1), retryable: true},
		"service unavailable": {err: apierrors.NewServiceUnavailable("offline"), retryable: true},
		"forbidden":           {err: apierrors.NewForbidden(pods, "helper-1", errors.New("denied")), retryable: false},
	}
}

func isRetryable(err error) bool {
	var retryable interface{ Retryable() bool }
	return errors.As(err, &retryable) && retryable.Retryable()
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
		Pools:     fakePoolResolver{pool: volumeapi.Pool{NodeName: "worker-a", MountPath: "/mnt/shiftpv"}},
		Image:     "busybox:1.37",
		Timeout:   time.Second,
	}
}
