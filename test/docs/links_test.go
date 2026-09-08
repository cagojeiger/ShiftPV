package docs_test

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

var markdownLink = regexp.MustCompile(`\]\(([^)]+)\)`)
var adrHeading = regexp.MustCompile(`(?m)^## (.+)$`)

func TestLocalMarkdownLinksResolve(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test file location")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	paths := []string{filepath.Join(root, "README.md")}
	for _, directory := range []string{"docs", "charts", "test"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() && strings.EqualFold(filepath.Ext(path), ".md") {
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", directory, err)
		}
	}

	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, match := range markdownLink.FindAllStringSubmatch(string(content), -1) {
			target := strings.Trim(match[1], "<>")
			if strings.HasPrefix(target, "#") || strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			target = strings.SplitN(target, "#", 2)[0]
			if target == "" {
				continue
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(path), filepath.FromSlash(target)))
			if _, err := os.Stat(resolved); err != nil {
				relative, _ := filepath.Rel(root, path)
				t.Errorf("%s: link %q does not resolve: %v", relative, match[1], err)
			}
		}
	}
}

func TestADRHeadingsAreConsistent(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test file location")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	adrDir := filepath.Join(root, "docs", "adr")
	want := []string{"Context", "Decision", "Alternatives considered", "Consequences"}

	entries, err := os.ReadDir(adrDir)
	if err != nil {
		t.Fatalf("read ADR directory: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") || entry.Name() == "README.md" {
			continue
		}
		content, err := os.ReadFile(filepath.Join(adrDir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		matches := adrHeading.FindAllStringSubmatch(string(content), -1)
		got := make([]string, 0, len(matches))
		for _, match := range matches {
			got = append(got, match[1])
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s: headings = %q, want %q", entry.Name(), got, want)
		}
	}
}
