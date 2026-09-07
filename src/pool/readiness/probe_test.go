package readiness

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
)

func TestProbeInspect(t *testing.T) {
	permission := &os.PathError{Op: "mkdir", Path: "/host/pool", Err: os.ErrPermission}
	readOnly := &os.PathError{Op: "mkdir", Path: "/host/pool", Err: syscall.EROFS}
	noSpace := &os.PathError{Op: "mkdir", Path: "/host/pool", Err: syscall.ENOSPC}
	for name, test := range map[string]struct {
		mounted     bool
		mountErr    error
		writeErr    error
		statErr     error
		ready       bool
		readyReason string
	}{
		"ready":              {mounted: true, ready: true, readyReason: "PoolReady"},
		"missing":            {mountErr: os.ErrNotExist, readyReason: "PathMissing"},
		"ordinary directory": {readyReason: "NotMounted"},
		"permission denied":  {mounted: true, writeErr: permission, readyReason: "PermissionDenied"},
		"read only":          {mounted: true, writeErr: readOnly, readyReason: "ReadOnly"},
		"no space":           {mounted: true, writeErr: noSpace, readyReason: "NoSpace"},
		"statfs failure":     {mounted: true, statErr: errors.New("statfs failed"), readyReason: "ProbeFailed"},
	} {
		t.Run(name, func(t *testing.T) {
			probe := &Probe{
				HostRoot: "/host",
				isMountPoint: func(path string) (bool, error) {
					if path != "/host/pool" {
						t.Fatalf("path = %q", path)
					}
					return test.mounted, test.mountErr
				},
				write:  func(string) error { return test.writeErr },
				statFS: func(string) error { return test.statErr },
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
