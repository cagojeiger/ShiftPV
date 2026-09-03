package admission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
)

type fakeVolumes struct {
	state volumeapi.State
	err   error
}

func (f fakeVolumes) Get(context.Context, string) (volumeapi.State, error) { return f.state, f.err }

func TestMutatePodUsesDynamicOwner(t *testing.T) {
	client := fake.NewSimpleClientset(boundObjects("worker-old")...)
	handler := Handler{Client: client, Volumes: fakeVolumes{state: volumeapi.State{Phase: volumeapi.PhaseReady, OwnerNode: "worker-new"}}}
	pod := claimPod()
	operations, err := handler.MutatePod(context.Background(), "test", pod)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(operations)
	if !strings.Contains(string(encoded), "worker-new") || strings.Contains(string(encoded), "worker-old") {
		t.Fatalf("patch does not use dynamic owner: %s", encoded)
	}
}

func TestMutatePodGatesMovingAndCordonedOwner(t *testing.T) {
	for name, state := range map[string]volumeapi.State{
		"moving":   {Phase: volumeapi.PhaseMoving, OwnerNode: "worker-a"},
		"blocked":  {Phase: volumeapi.PhaseBlocked, OwnerNode: "worker-a"},
		"cordoned": {Phase: volumeapi.PhaseReady, OwnerNode: "worker-a"},
	} {
		t.Run(name, func(t *testing.T) {
			objects := boundObjects("worker-a")
			if name == "cordoned" {
				objects[2].(*corev1.Node).Spec.Unschedulable = true
			}
			handler := Handler{Client: fake.NewSimpleClientset(objects...), Volumes: fakeVolumes{state: state}}
			operations, err := handler.MutatePod(context.Background(), "test", claimPod())
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(operations)
			if !strings.Contains(string(encoded), PlacementHold) {
				t.Fatalf("patch has no placement hold: %s", encoded)
			}
		})
	}
}

func TestMutatePodSkipsUnboundPVC(t *testing.T) {
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "test"}}
	handler := Handler{Client: fake.NewSimpleClientset(claim), Volumes: fakeVolumes{err: errors.New("must not be called")}}
	operations, err := handler.MutatePod(context.Background(), "test", claimPod())
	if err != nil || len(operations) != 0 {
		t.Fatalf("operations=%v err=%v", operations, err)
	}
}

func TestMutatePodRejectsConflictingSelector(t *testing.T) {
	handler := Handler{Client: fake.NewSimpleClientset(boundObjects("worker-a")...), Volumes: fakeVolumes{state: volumeapi.State{Phase: volumeapi.PhaseReady, OwnerNode: "worker-a"}}}
	pod := claimPod()
	pod.Spec.NodeSelector = map[string]string{HostnameLabel: "worker-b"}
	if _, err := handler.MutatePod(context.Background(), "test", pod); err == nil {
		t.Fatal("conflicting selector was accepted")
	}
}

func TestMutatePodRejectsInvalidStateAndDependencies(t *testing.T) {
	if _, err := (&Handler{}).MutatePod(context.Background(), "test", claimPod()); err == nil {
		t.Fatal("unconfigured handler was accepted")
	}

	for name, handler := range map[string]Handler{
		"missing owner": {
			Client:  fake.NewSimpleClientset(boundObjects("worker-a")...),
			Volumes: fakeVolumes{state: volumeapi.State{Phase: volumeapi.PhaseReady}},
		},
		"missing owner node": {
			Client: fake.NewSimpleClientset(boundObjects("worker-a")[:2]...),
			Volumes: fakeVolumes{state: volumeapi.State{
				Phase: volumeapi.PhaseReady, OwnerNode: "worker-a",
			}},
		},
		"volume registry failure": {
			Client:  fake.NewSimpleClientset(boundObjects("worker-a")...),
			Volumes: fakeVolumes{err: errors.New("registry unavailable")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := handler.MutatePod(context.Background(), "test", claimPod()); err == nil {
				t.Fatal("invalid admission state was accepted")
			}
		})
	}
}

func TestMutatePodSkipsNonShiftPVAndPinsExistingSelector(t *testing.T) {
	nonShiftObjects := boundObjects("worker-a")
	nonShiftObjects[1].(*corev1.PersistentVolume).Spec.CSI.Driver = "example.invalid/csi"
	handler := Handler{Client: fake.NewSimpleClientset(nonShiftObjects...), Volumes: fakeVolumes{err: errors.New("must not be called")}}
	operations, err := handler.MutatePod(context.Background(), "test", claimPod())
	if err != nil || len(operations) != 0 {
		t.Fatalf("non-ShiftPV operations=%v err=%v", operations, err)
	}

	handler = Handler{Client: fake.NewSimpleClientset(boundObjects("worker-a")...), Volumes: fakeVolumes{state: volumeapi.State{Phase: volumeapi.PhaseReady, OwnerNode: "worker-a"}}}
	pod := claimPod()
	pod.Annotations = map[string]string{PlacementKey: "stale"}
	pod.Spec.NodeSelector = map[string]string{HostnameLabel: "worker-a", "disk": "fast"}
	operations, err = handler.MutatePod(context.Background(), "test", pod)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].Operation != "replace" || operations[0].Path != "/metadata/annotations/shiftpv.io~1placement" {
		t.Fatalf("operations = %#v", operations)
	}
}

func TestMutatePodGatingIsIdempotentAndNodeReadyRequiresCondition(t *testing.T) {
	pod := claimPod()
	pod.Annotations = map[string]string{}
	pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: PlacementHold}}
	handler := Handler{Client: fake.NewSimpleClientset(boundObjects("worker-a")...), Volumes: fakeVolumes{state: volumeapi.State{Phase: volumeapi.PhaseMoving, OwnerNode: "worker-a"}}}
	operations, err := handler.MutatePod(context.Background(), "test", pod)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].Path != "/metadata/annotations/shiftpv.io~1placement" {
		t.Fatalf("idempotent placement hold operations = %#v", operations)
	}
	if NodeReady(&corev1.Node{}) {
		t.Fatal("node without Ready condition was considered ready")
	}
}

func TestCordonedOwnerStaysPinnedOutsideMobilityNamespace(t *testing.T) {
	objects := boundObjects("worker-a")
	objects[2].(*corev1.Node).Spec.Unschedulable = true
	objects[len(objects)-1].(*corev1.Namespace).Labels = nil
	handler := Handler{Client: fake.NewSimpleClientset(objects...), Volumes: fakeVolumes{state: volumeapi.State{Phase: volumeapi.PhaseReady, OwnerNode: "worker-a"}}}
	operations, err := handler.MutatePod(context.Background(), "test", claimPod())
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(operations)
	if strings.Contains(string(encoded), PlacementHold) || !strings.Contains(string(encoded), "worker-a") {
		t.Fatalf("non-mobility patch = %s", encoded)
	}
}

func TestMutatePodRejectsMultipleVolumesAndSkipsMissingBindings(t *testing.T) {
	handler := Handler{Client: fake.NewSimpleClientset(boundObjects("worker-a")...), Volumes: fakeVolumes{state: volumeapi.State{Phase: volumeapi.PhaseReady, OwnerNode: "worker-a"}}}
	pod := claimPod()
	pod.Spec.Volumes = append(pod.Spec.Volumes, pod.Spec.Volumes[0])
	if _, err := handler.MutatePod(context.Background(), "test", pod); err == nil {
		t.Fatal("multiple ShiftPV volumes were accepted")
	}

	handler.Client = fake.NewSimpleClientset()
	if operations, err := handler.MutatePod(context.Background(), "test", claimPod()); err != nil || len(operations) != 0 {
		t.Fatalf("missing PVC should be a no-op: operations=%v err=%v", operations, err)
	}

	objects := boundObjects("worker-a")
	handler.Client = fake.NewSimpleClientset(objects[0])
	if operations, err := handler.MutatePod(context.Background(), "test", claimPod()); err != nil || len(operations) != 0 {
		t.Fatalf("missing PV should be a no-op: operations=%v err=%v", operations, err)
	}
}

func TestAdmissionHTTPAndResponsePaths(t *testing.T) {
	handler := Handler{Client: fake.NewSimpleClientset(boundObjects("worker-a")...), Volumes: fakeVolumes{state: volumeapi.State{Phase: volumeapi.PhaseMoving, OwnerNode: "worker-a"}}}
	podJSON, _ := json.Marshal(claimPod())
	review := admissionv1.AdmissionReview{Request: &admissionv1.AdmissionRequest{
		UID: types.UID("request"), Operation: admissionv1.Create, Namespace: "test",
		Resource: metav1.GroupVersionResource{Version: "v1", Resource: "pods"},
		Object:   runtime.RawExtension{Raw: podJSON},
	}}
	body, _ := json.Marshal(review)
	request := httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	var result admissionv1.AdmissionReview
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Response == nil || !result.Response.Allowed || result.Response.PatchType == nil || !strings.Contains(string(result.Response.Patch), PlacementHold) {
		t.Fatalf("response = %#v", result.Response)
	}

	badRequest := httptest.NewRequest(http.MethodPost, "/mutate", strings.NewReader("not-json"))
	badResponse := httptest.NewRecorder()
	handler.ServeHTTP(badResponse, badRequest)
	if badResponse.Code != http.StatusBadRequest {
		t.Fatalf("bad request status = %d", badResponse.Code)
	}
	if handler.Admit(context.Background(), nil).Allowed {
		t.Fatal("nil request was admitted")
	}
	ignored := handler.Admit(context.Background(), &admissionv1.AdmissionRequest{Operation: admissionv1.Update})
	if !ignored.Allowed || len(ignored.Patch) != 0 {
		t.Fatalf("ignored response = %#v", ignored)
	}
	invalid := handler.Admit(context.Background(), &admissionv1.AdmissionRequest{
		UID: types.UID("invalid"), Operation: admissionv1.Create,
		Resource: metav1.GroupVersionResource{Version: "v1", Resource: "pods"}, Object: runtime.RawExtension{Raw: []byte("{")},
	})
	if invalid.Allowed {
		t.Fatal("invalid Pod was admitted")
	}
}

func claimPod() *corev1.Pod {
	return &corev1.Pod{Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}}}}}
}

func boundObjects(staticOwner string) []runtime.Object {
	return []runtime.Object{
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "test"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv-data"}},
		&corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-data"}, Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: DriverName, VolumeHandle: "shiftpv-0123456789abcdef0123456789abcdef", VolumeAttributes: map[string]string{"shiftpv.io/node": staticOwner}}}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-a"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-new"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test", Labels: map[string]string{MobilityNamespaceLabel: "enabled"}}},
	}
}
