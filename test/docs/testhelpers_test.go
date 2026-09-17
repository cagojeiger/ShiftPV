package docs_test

import (
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
)

// forbiddenHistoricalTerms lists the removed or historical identifiers that
// must not reappear in the durable 0.4 documentation surface. The retired
// standalone API is matched at a word boundary so that current identifiers
// which merely start with the same letters — the ShiftPVCleanupNeedsReview
// alert the chart still ships — are not mistaken for it.
var forbiddenHistoricalTerms = []*regexp.Regexp{
	regexp.MustCompile(`ShiftPVCleanups?\b`),
	regexp.MustCompile(`reservation ConfigMap`),
	regexp.MustCompile(`0\.3\.1`),
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test location")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}
