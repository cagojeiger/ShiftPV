package docs_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTarget04DocsDoNotRetainRemovedDurableResources(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test location")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	paths := []string{
		"README.md",
		"charts/shiftpv/README.md",
		"docs/README.md",
		"docs/adr/README.md",
		"docs/adr/0005-pool-filesystem-capacity-admission.md",
		"docs/adr/0006-node-reported-pool-readiness.md",
		"docs/adr/0007-automatic-cordon-volume-mobility.md",
		"docs/adr/0008-nondisruptive-mobility-preflight.md",
		"docs/adr/0009-explicit-owner-recovery.md",
		"docs/adr/0010-operator-visible-mobility-diagnostics.md",
		"docs/adr/0011-fail-closed-uninstall-guard.md",
		"docs/spec/README.md",
		"docs/spec/csi-driver.md",
		"docs/spec/storage-class.md",
		"docs/spec/volume-mobility.md",
		"docs/spec/source-cleanup.md",
		"docs/spec/metrics.md",
	}
	for _, relative := range paths {
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatalf("read %s: %v", relative, err)
		}
		for _, forbidden := range []string{"ShiftPVCleanup", "reservation ConfigMap", "0.3.1"} {
			if strings.Contains(string(content), forbidden) {
				t.Errorf("%s retains removed or historical term %q", relative, forbidden)
			}
		}
	}
}

func TestTarget04ContractNamesThreeDurableAPIs(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test location")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	content, err := os.ReadFile(filepath.Join(root, "docs", "spec", "csi-driver.md"))
	if err != nil {
		t.Fatal(err)
	}
	contract := string(content)
	for _, kind := range []string{"`ShiftPVPool`", "`ShiftPVVolume`", "`ShiftPVMove`"} {
		if strings.Count(contract, kind) == 0 {
			t.Errorf("CSI contract is missing durable API %s", kind)
		}
	}
	if !strings.Contains(contract, "durable API는 정확히 세 개") {
		t.Error("CSI contract does not close the durable API surface at three resources")
	}
}
