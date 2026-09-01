package mount

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type fakeMounter struct {
	mounted    bool
	mounts     int
	unmounts   int
	mountErr   error
	unmountErr error
	inspectErr error
}

func TestNewBinderHasMounter(t *testing.T) {
	if binder := NewBinder(); binder == nil || binder.Mounter == nil {
		t.Fatal("NewBinder returned an unconfigured binder")
	}
}

func (f *fakeMounter) Mount(string, string, string, []string) error {
	if f.mountErr != nil {
		return f.mountErr
	}
	f.mounts++
	f.mounted = true
	return nil
}

func (f *fakeMounter) Unmount(string) error {
	if f.unmountErr != nil {
		return f.unmountErr
	}
	f.unmounts++
	f.mounted = false
	return nil
}

func (f *fakeMounter) IsMountPoint(string) (bool, error) { return f.mounted, f.inspectErr }

func TestPublishMountsDirectory(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "target")
	if err := os.Mkdir(source, 0o750); err != nil {
		t.Fatal(err)
	}
	mounter := &fakeMounter{}
	binder := &Binder{Mounter: mounter}
	if err := binder.Publish(source, target); err != nil {
		t.Fatal(err)
	}
	if mounter.mounts != 1 {
		t.Fatalf("expected one mount, got %d", mounter.mounts)
	}
}

func TestPublishRejectsDifferentExistingMount(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "target")
	if err := os.Mkdir(source, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	binder := &Binder{Mounter: &fakeMounter{mounted: true}}
	if err := binder.Publish(source, target); err == nil {
		t.Fatal("expected mismatched existing mount to fail")
	}
}

func TestPublishRejectsMissingOrNonDirectorySource(t *testing.T) {
	root := t.TempDir()
	binder := &Binder{Mounter: &fakeMounter{}}
	if err := binder.Publish(filepath.Join(root, "missing"), filepath.Join(root, "target")); err == nil {
		t.Fatal("expected missing source to fail")
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := binder.Publish(file, filepath.Join(root, "target")); err == nil {
		t.Fatal("expected non-directory source to fail")
	}
}

func TestPublishIsIdempotentForSameExistingMount(t *testing.T) {
	source := t.TempDir()
	mounter := &fakeMounter{mounted: true}
	binder := &Binder{Mounter: mounter}
	if err := binder.Publish(source, source); err != nil {
		t.Fatal(err)
	}
	if mounter.mounts != 0 {
		t.Fatalf("unexpected remount count: %d", mounter.mounts)
	}
}

func TestPublishReportsMountFailure(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o750); err != nil {
		t.Fatal(err)
	}
	binder := &Binder{Mounter: &fakeMounter{mountErr: errors.New("mount failed")}}
	if err := binder.Publish(source, filepath.Join(root, "target")); err == nil {
		t.Fatal("expected mount failure")
	}
}

func TestUnpublishUnmountsAndRemovesTarget(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	mounter := &fakeMounter{mounted: true}
	if err := (&Binder{Mounter: mounter}).Unpublish(target); err != nil {
		t.Fatal(err)
	}
	if mounter.unmounts != 1 {
		t.Fatalf("expected one unmount, got %d", mounter.unmounts)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("expected target removal, got %v", err)
	}
}

func TestUnpublishIsIdempotentForMissingTarget(t *testing.T) {
	binder := &Binder{Mounter: &fakeMounter{inspectErr: os.ErrNotExist}}
	if err := binder.Unpublish("/missing"); err != nil {
		t.Fatal(err)
	}
}

func TestUnpublishPreservesTargetOnUnmountFailure(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	binder := &Binder{Mounter: &fakeMounter{mounted: true, unmountErr: errors.New("busy")}}
	if err := binder.Unpublish(target); err == nil {
		t.Fatal("expected unmount failure")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("target removed after unmount failure: %v", err)
	}
}

func TestValidateTarget(t *testing.T) {
	if err := ValidateTarget("/var/lib/kubelet/pods", "/var/lib/kubelet/pods/uid/volumes/csi/mount"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTarget("/var/lib/kubelet/pods", "/var/lib/kubelet/pods-evil/uid"); err == nil {
		t.Fatal("expected sibling path to fail")
	}
	if err := ValidateTarget("relative", "/var/lib/kubelet/pods/uid"); err == nil {
		t.Fatal("expected relative root to fail")
	}
	if err := ValidateTarget("/var/lib/kubelet/pods", "/var/lib/kubelet/pods"); err == nil {
		t.Fatal("expected root itself to fail")
	}
}
