package docs_test

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/cleanupapi"
)

func TestCRDsUseKubernetesStructuralObjectSchemas(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test location")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	paths, err := filepath.Glob(filepath.Join(root, "charts", "shiftpv", "crds", "*.yaml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("list CRDs: %v", err)
	}
	for _, path := range paths {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if bytes.Contains(data, []byte("additionalProperties: false")) {
			t.Errorf("%s uses a JSON Schema closure rejected by the Kubernetes CRD structural schema", filepath.Base(path))
		}
	}
}

func TestCleanupCRDMatchesRuntimeContract(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test location")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	data, err := os.ReadFile(filepath.Join(root, "charts/shiftpv/crds/shiftpv.io_shiftpvcleanups.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}
	versions, found, err := unstructured.NestedSlice(crd, "spec", "versions")
	if err != nil || !found || len(versions) != 1 {
		t.Fatalf("cleanup CRD versions: %v", err)
	}
	version := versions[0].(map[string]any)
	phases, found, err := unstructured.NestedStringSlice(version, "schema", "openAPIV3Schema", "properties", "status", "properties", "phase", "enum")
	if err != nil || !found {
		t.Fatalf("cleanup phase enum: %v", err)
	}
	want := []string{cleanupapi.PhasePending, cleanupapi.PhaseRunning, cleanupapi.PhaseVerifying, cleanupapi.PhaseCompleted, cleanupapi.PhaseNeedsReview}
	if len(phases) != len(want) {
		t.Fatalf("phases=%v want=%v", phases, want)
	}
	for index := range want {
		if phases[index] != want[index] {
			t.Fatalf("phases=%v want=%v", phases, want)
		}
	}
	specValidation, found, err := unstructured.NestedSlice(version, "schema", "openAPIV3Schema", "properties", "spec", "x-kubernetes-validations")
	if err != nil || !found || len(specValidation) != 2 {
		t.Fatalf("immutable cleanup spec validation is missing: %v", err)
	}
	identityRule, _, _ := unstructured.NestedString(specValidation[0].(map[string]any), "rule")
	approvalRule, _, _ := unstructured.NestedString(specValidation[1].(map[string]any), "rule")
	if !bytes.Contains([]byte(identityRule), []byte("self.operationID == oldSelf.operationID")) || approvalRule != "!oldSelf.approved || self.approved" {
		t.Fatalf("cleanup identity/approval rules=%q / %q", identityRule, approvalRule)
	}
	specProperties, found, err := unstructured.NestedMap(version, "schema", "openAPIV3Schema", "properties", "spec", "properties")
	if err != nil || !found {
		t.Fatalf("cleanup spec properties: %v", err)
	}
	if _, hasPath := specProperties["path"]; hasPath {
		t.Fatal("cleanup intent exposes an arbitrary filesystem path")
	}
	if _, hasReservationUID := specProperties["reservationUID"]; !hasReservationUID {
		t.Fatal("cleanup intent cannot bind an exact capacity reservation UID")
	}
}
