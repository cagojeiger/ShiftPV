package docs_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMobilityContractContainsTargetSafetyBoundary(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test location")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	content, err := os.ReadFile(filepath.Join(root, "docs", "spec", "volume-mobility.md"))
	if err != nil {
		t.Fatal(err)
	}

	contract := string(content)
	for _, required := range []string{
		"## Authority",
		"## State Table",
		"## Publication And Inventory Fence",
		"## Recovery Matrix",
		"## Safety Invariants",
		"owner CAS",
		"actual-publish proof",
		"API purge receipt",
		"fresh absence proof",
		"`Succeeded`",
		"`Aborted`",
		"`NeedsReview`",
	} {
		if !strings.Contains(contract, required) {
			t.Errorf("0.4 mobility contract is missing %q", required)
		}
	}
	for _, forbidden := range []string{"ShiftPVCleanup", "reservation ConfigMap", "0.3.1"} {
		if strings.Contains(contract, forbidden) {
			t.Errorf("0.4 mobility contract retains removed or historical term %q", forbidden)
		}
	}
}
