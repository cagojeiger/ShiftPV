package controller

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Linux CI runs the actual embedded scripts. macOS can use the locally built
// helper image with RECOVERY_SCRIPT_IMAGE=shiftpv:dev, without a Kubernetes API.
func runRecoveryScript(t *testing.T, root, script string, committed bool) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	commit := "false"
	if committed {
		commit = "true"
	}
	var command *exec.Cmd
	if image := os.Getenv("RECOVERY_SCRIPT_IMAGE"); image != "" {
		command = exec.CommandContext(ctx, "docker", "run", "--rm", "--user", "0:0", "--mount", "type=bind,source="+root+",target=/pool", "--env", "MOVE_NAME=move", "--env", "VOLUME_ID=volume", "--env", "COMMITTED="+commit, "--entrypoint", "/bin/sh", image, "-c", script)
	} else {
		if runtime.GOOS != "linux" {
			t.Skip("Linux scripts: set RECOVERY_SCRIPT_IMAGE for local container validation")
		}
		command = exec.CommandContext(ctx, "/bin/sh", "-c", strings.ReplaceAll(script, "/pool", root))
		command.Env = append(os.Environ(), "MOVE_NAME=move", "VOLUME_ID=volume", "COMMITTED="+commit)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Logf("script exit: %v; %s", err, output)
	}
	return err
}

func TestRecoveryRetirementScript(t *testing.T) {
	for _, scenario := range []string{"partial copy", "promoted uncommitted", "committed source", "already retired", "missing committed source", "wrong marker", "symlink", "quarantine collision"} {
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
			committed, wantError := false, false
			switch scenario {
			case "partial copy":
				write(".shiftpv/incoming/move/payload", "partial")
			case "promoted uncommitted":
				write("volumes/volume/.shiftpv-move-id", "move\n")
				write("volumes/volume/payload", "data")
			case "committed source":
				committed = true
				write("volumes/volume/payload", "old source")
			case "already retired":
				committed = true
				write(".shiftpv/retired/move/payload", "old source")
			case "missing committed source":
				committed, wantError = true, true
			case "wrong marker":
				wantError = true
				write("volumes/volume/.shiftpv-move-id", "other-move")
			case "symlink":
				wantError = true
				if err := os.MkdirAll(filepath.Join(root, "volumes"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../outside", filepath.Join(root, "volumes/volume")); err != nil {
					t.Fatal(err)
				}
			case "quarantine collision":
				committed, wantError = true, true
				write("volumes/volume/payload", "original")
				write(".shiftpv/aborted/move-final/payload", "do not overwrite")
			}
			err := runRecoveryScript(t, root, recoveryRetireScript, committed)
			if (err != nil) != wantError {
				t.Fatalf("error=%v wantError=%v", err, wantError)
			}
			if !wantError {
				if err := runRecoveryScript(t, root, recoveryRetireScript, committed); err != nil {
					t.Fatalf("retry failed: %v", err)
				}
				if _, err := os.Lstat(filepath.Join(root, "volumes/volume")); !os.IsNotExist(err) {
					t.Fatal("non-owner final was not retired")
				}
			}
			if scenario == "quarantine collision" {
				data, err := os.ReadFile(filepath.Join(root, ".shiftpv/aborted/move-final/payload"))
				if err != nil || string(data) != "do not overwrite" {
					t.Fatal("quarantine was overwritten")
				}
			}
		})
	}
}

func TestRecoveryOwnerVerificationScript(t *testing.T) {
	root := t.TempDir()
	if err := runRecoveryScript(t, root, recoveryVerifyScript, false); err == nil {
		t.Fatal("missing owner directory accepted")
	}
	if err := os.MkdirAll(filepath.Join(root, "volumes/volume"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runRecoveryScript(t, root, recoveryVerifyScript, false); err != nil {
		t.Fatal(err)
	}
	if err := runRecoveryScript(t, root, recoveryVerifyScript, true); err == nil {
		t.Fatal("committed owner without promotion marker accepted")
	}
	if err := os.WriteFile(filepath.Join(root, "volumes/volume/.shiftpv-move-id"), []byte("move\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runRecoveryScript(t, root, recoveryVerifyScript, true); err != nil {
		t.Fatal(err)
	}
}
