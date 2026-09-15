package docs_test

import (
	"path/filepath"
	"runtime"
	"testing"
)

// forbiddenHistoricalTerms lists the removed or historical identifiers that
// must not reappear in the durable 0.4 documentation surface.
var forbiddenHistoricalTerms = []string{"ShiftPVCleanup", "reservation ConfigMap", "0.3.1"}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test location")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}
