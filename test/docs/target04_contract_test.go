package docs_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestTarget04DocsDoNotRetainRemovedDurableResources(t *testing.T) {
	root := repositoryRoot(t)
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
		for _, forbidden := range forbiddenHistoricalTerms {
			if match := forbidden.FindString(string(content)); match != "" {
				t.Errorf("%s retains removed or historical term %q", relative, match)
			}
		}
	}
}

// shiftPVResourceName matches a backtick-quoted ShiftPV resource identifier,
// e.g. `ShiftPVPool`.
var shiftPVResourceName = regexp.MustCompile("`ShiftPV[A-Za-z]+`")

func TestTarget04ContractNamesThreeDurableAPIs(t *testing.T) {
	root := repositoryRoot(t)
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

	// The durable-API surface is closed structurally: find the statement that
	// introduces it and the table naming each API, and require it to list
	// exactly ShiftPVPool, ShiftPVVolume, and ShiftPVMove and no other
	// ShiftPV* resource, rather than pinning to the literal Korean sentence.
	statementStart := strings.Index(contract, "durable API")
	if statementStart == -1 {
		t.Fatal("CSI contract has no durable-API statement")
	}
	afterStatement := contract[statementStart:]
	statementEnd := strings.Index(afterStatement, "Helper Job")
	if statementEnd == -1 {
		t.Fatal("durable-API statement has no closing paragraph naming Helper Job as a non-source-of-truth")
	}
	statement := afterStatement[:statementEnd]

	names := shiftPVResourceName.FindAllString(statement, -1)
	unique := map[string]bool{}
	for _, name := range names {
		unique[name] = true
	}
	want := map[string]bool{"`ShiftPVPool`": true, "`ShiftPVVolume`": true, "`ShiftPVMove`": true}
	if len(unique) != len(want) {
		t.Fatalf("durable-API statement names %v, want exactly ShiftPVPool, ShiftPVVolume, ShiftPVMove", names)
	}
	for name := range want {
		if !unique[name] {
			t.Errorf("durable-API statement is missing %s", name)
		}
	}
}
