package controller

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
)

func TestCopyJobRequiresAnEmptyItemizedChecksumDiff(t *testing.T) {
	ctx := context.Background()
	move := volumeapi.Move{
		Name: "move-test",
		Spec: volumeapi.MoveSpec{
			VolumeID:   "shiftpv-0123456789abcdef0123456789abcdef",
			SourceNode: "source",
		},
		Status: volumeapi.MoveStatus{DestinationNode: "destination"},
	}
	names := namesFor(move.Name)
	client := fake.NewSimpleClientset()
	reconciler := &Reconciler{
		Client:      client,
		Repository:  &memoryRepository{pools: []volumeapi.Pool{{Name: "destination", NodeName: "destination", MountPath: "/destination-pool"}}},
		Namespace:   "system",
		HelperImage: "helper",
	}
	if err := reconciler.ensureCopyJob(ctx, move, names); err != nil {
		t.Fatal(err)
	}
	job, err := client.BatchV1().Jobs("system").Get(ctx, names.CopyJob, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	script := job.Spec.Template.Spec.Containers[0].Command[2]
	if !strings.Contains(script, "--checksum --delete --dry-run --itemize-changes") {
		t.Fatalf("copy verification does not request an itemized diff:\n%s", script)
	}
	check := strings.Index(script, "test ! -s /tmp/rsync-diff")
	marker := strings.Index(script, `printf '%s\n' "${MOVE_NAME}"`)
	if check < 0 || marker < 0 || check > marker {
		t.Fatalf("copy marker can be written before diff validation:\n%s", script)
	}
}

func TestCleanupSourceScriptPurgesSourceAndIsIdempotent(t *testing.T) {
	for _, scenario := range []string{"source", "retired", "absent", "source symlink", "retired symlink", "collision"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			write := func(path, content string) {
				t.Helper()
				full := filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			wantError := false
			switch scenario {
			case "source":
				write("volumes/volume/payload", "old source")
			case "retired":
				write(".shiftpv/retired/move/payload", "old source")
			case "absent":
			case "source symlink":
				wantError = true
				write("outside/payload", "preserve")
				if err := os.MkdirAll(filepath.Join(root, "volumes"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(root, "volumes/volume")); err != nil {
					t.Fatal(err)
				}
			case "retired symlink":
				wantError = true
				write("outside/payload", "preserve")
				if err := os.MkdirAll(filepath.Join(root, ".shiftpv/retired"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(root, ".shiftpv/retired/move")); err != nil {
					t.Fatal(err)
				}
			case "collision":
				wantError = true
				write("volumes/volume/payload", "source")
				write(".shiftpv/retired/move/payload", "retired")
			}

			err := runRecoveryScript(t, root, cleanupSourceScript, false)
			if (err != nil) != wantError {
				t.Fatalf("error=%v wantError=%v", err, wantError)
			}
			if wantError {
				if scenario == "source symlink" || scenario == "retired symlink" {
					data, err := os.ReadFile(filepath.Join(root, "outside/payload"))
					if err != nil || string(data) != "preserve" {
						t.Fatal("symlink target was modified")
					}
				}
				if scenario == "collision" {
					for path, want := range map[string]string{
						"volumes/volume/payload":        "source",
						".shiftpv/retired/move/payload": "retired",
					} {
						data, err := os.ReadFile(filepath.Join(root, path))
						if err != nil || string(data) != want {
							t.Fatalf("collision path %s was modified", path)
						}
					}
				}
				return
			}
			if err := runRecoveryScript(t, root, cleanupSourceScript, false); err != nil {
				t.Fatalf("retry failed: %v", err)
			}
			for _, path := range []string{"volumes/volume", ".shiftpv/retired/move"} {
				if _, err := os.Lstat(filepath.Join(root, path)); !os.IsNotExist(err) {
					t.Fatalf("cleanup left %s", path)
				}
			}
		})
	}
}

func TestItemizedChecksumDryRunReportsDifferentContent(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync is not installed")
	}
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "destination")
	for _, directory := range []string{source, destination} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "payload"), []byte("original-data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "payload"), []byte("modified-data\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	diff := runItemizedChecksumDryRun(t, source, destination)
	if strings.TrimSpace(diff) == "" {
		t.Fatal("different same-size content produced an empty rsync diff")
	}
	if err := os.WriteFile(filepath.Join(destination, "payload"), []byte("original-data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceInfo, err := os.Stat(filepath.Join(source, "payload"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(destination, "payload"), sourceInfo.ModTime(), sourceInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	if diff := runItemizedChecksumDryRun(t, source, destination); strings.TrimSpace(diff) != "" {
		t.Fatalf("identical content produced a diff: %q", diff)
	}

	if err := os.WriteFile(filepath.Join(source, "source-only"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if diff := runItemizedChecksumDryRun(t, source, destination); strings.TrimSpace(diff) == "" {
		t.Fatal("missing destination file produced an empty rsync diff")
	}
	if err := os.Remove(filepath.Join(source, "source-only")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "destination-only"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if diff := runItemizedChecksumDryRun(t, source, destination); strings.TrimSpace(diff) == "" {
		t.Fatal("additional destination file produced an empty rsync diff")
	}
}

func runItemizedChecksumDryRun(t *testing.T, source, destination string) string {
	t.Helper()
	command := exec.Command("rsync", "-a", "--checksum", "--delete", "--dry-run", "--itemize-changes", source+"/", destination+"/")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("rsync dry-run failed: %v: %s", err, output)
	}
	return string(output)
}
