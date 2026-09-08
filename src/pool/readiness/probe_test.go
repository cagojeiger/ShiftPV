package readiness

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	poolcapacity "github.com/cagojeiger/ShiftPV/src/pool/capacity"
)

func TestProbeInspect(t *testing.T) {
	permission := &os.PathError{Op: "mkdir", Path: "/host/pool", Err: os.ErrPermission}
	readOnly := &os.PathError{Op: "mkdir", Path: "/host/pool", Err: syscall.EROFS}
	noSpace := &os.PathError{Op: "mkdir", Path: "/host/pool", Err: syscall.ENOSPC}
	for name, test := range map[string]struct {
		inspectErr  error
		writeErr    error
		statErr     error
		ready       bool
		readyReason string
	}{
		"ordinary directory": {ready: true, readyReason: "PoolReady"},
		"missing":            {inspectErr: os.ErrNotExist, readyReason: "PathMissing"},
		"not a directory":    {inspectErr: syscall.ENOTDIR, readyReason: "NotDirectory"},
		"permission denied":  {writeErr: permission, readyReason: "PermissionDenied"},
		"read only":          {writeErr: readOnly, readyReason: "ReadOnly"},
		"no space":           {writeErr: noSpace, readyReason: "NoSpace"},
		"statfs failure":     {statErr: errors.New("statfs failed"), readyReason: "ProbeFailed"},
	} {
		t.Run(name, func(t *testing.T) {
			probe := &Probe{
				HostRoot: "/host",
				inspect: func(path string) error {
					if path != "/host/pool" {
						t.Fatalf("path = %q", path)
					}
					return test.inspectErr
				},
				write:  func(string) error { return test.writeErr },
				statFS: func(string) (poolcapacity.Filesystem, error) { return poolcapacity.Filesystem{}, test.statErr },
			}
			result := probe.Inspect(volumeapi.Pool{MountPath: "/pool"})
			conditions := conditions(result, 1, testTime)
			ready := conditions[len(conditions)-1]
			if (ready.Status == "True") != test.ready || ready.Reason != test.readyReason {
				t.Fatalf("ready = %#v", ready)
			}
		})
	}
}

func TestNewProbeAcceptsOrdinaryDirectory(t *testing.T) {
	hostRoot := t.TempDir()
	poolPath := filepath.Join(hostRoot, "var", "lib", "shiftpv")
	if err := os.MkdirAll(poolPath, 0o700); err != nil {
		t.Fatal(err)
	}
	result := NewProbe(hostRoot).Inspect(volumeapi.Pool{MountPath: "/var/lib/shiftpv"})
	conditions := conditions(result, 1, testTime)
	ready := conditions[len(conditions)-1]
	if ready.Status != "True" || ready.Reason != "PoolReady" {
		t.Fatalf("ordinary directory readiness = %#v", ready)
	}
	entries, err := os.ReadDir(poolPath)
	if err != nil || len(entries) != 0 {
		t.Fatalf("probe leftovers = %v err=%v", entries, err)
	}
}

func TestInspectDirectoryRejectsNonDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pool")
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := inspectDirectory(path)
	if err == nil || !strings.Contains(err.Error(), "not a directory") || !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("inspect error = %v", err)
	}
}

func TestWriteProbeCleansUp(t *testing.T) {
	root := t.TempDir()
	if err := writeProbe(root); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("probe leftovers = %v err=%v", entries, err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeProbe(file); err == nil {
		t.Fatal("write probe accepted a file")
	}
}

func TestHostPathValidation(t *testing.T) {
	if got, err := hostPath("/host", "/mnt/data"); err != nil || got != "/host/mnt/data" {
		t.Fatalf("host path = %q err=%v", got, err)
	}
	for _, paths := range [][2]string{{"relative", "/pool"}, {"/host", "relative"}, {"/host", "/"}} {
		if _, err := hostPath(paths[0], paths[1]); err == nil {
			t.Fatalf("accepted hostRoot=%q mountPath=%q", paths[0], paths[1])
		}
	}
}
