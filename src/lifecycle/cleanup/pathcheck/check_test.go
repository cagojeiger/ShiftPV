package pathcheck

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestCheckAbsenceAndRetainedData(t *testing.T) {
	for _, relative := range []string{"volumes/volume", ".shiftpv/retired/move", ".shiftpv/aborted/move-final", ".shiftpv/aborted/move-incoming", ".shiftpv/incoming/move"} {
		t.Run(relative, func(t *testing.T) {
			root := t.TempDir()
			if err := Check(root, "move", "volume"); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(root, relative), 0700); err != nil {
				t.Fatal(err)
			}
			if err := Check(root, "move", "volume"); err == nil {
				t.Fatal("retained path accepted")
			}
		})
	}
}

func TestCheckRejectsLookupErrorsAndUnsafeAncestors(t *testing.T) {
	root := t.TempDir()
	for _, cause := range []error{syscall.EACCES, syscall.EIO, syscall.ENOTDIR, syscall.ELOOP} {
		err := inspect(root, "move", "volume", func(path string) (os.FileInfo, error) {
			if path != root {
				return nil, &os.PathError{Op: "lstat", Path: path, Err: cause}
			}
			return os.Lstat(path)
		})
		if !errors.Is(err, cause) {
			t.Fatalf("lookup error became absence: %v", err)
		}
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "volumes")); err != nil {
		t.Fatal(err)
	}
	if err := Check(root, "move", "volume"); err == nil {
		t.Fatal("symlink ancestor accepted")
	}
	for _, id := range []string{"", "..", "../escape"} {
		if err := Check(root, id, "volume"); err == nil {
			t.Fatal("unsafe identity accepted")
		}
	}
}
