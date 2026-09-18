package mount

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const testVolumeID = "shiftpv-0123456789abcdef0123456789abcdef"

type fakeMounter struct {
	mounted    bool
	mounts     int
	unmounts   int
	mountErr   error
	unmountErr error
	inspectErr error
	mountRefs  []string
	refsErr    error
}

func TestNewBinderHasMounter(t *testing.T) {
	root := t.TempDir()
	if binder := NewBinder(root); binder == nil || binder.Mounter == nil || binder.TargetRoot != root || binder.MountInfoPath == "" {
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
func (f *fakeMounter) GetMountRefs(string) ([]string, error) {
	return append([]string(nil), f.mountRefs...), f.refsErr
}

func TestPublishMountsDirectory(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "target")
	if err := os.Mkdir(source, 0o750); err != nil {
		t.Fatal(err)
	}
	mounter := &fakeMounter{}
	binder := &Binder{Mounter: mounter, TargetRoot: root}
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
	binder := &Binder{Mounter: &fakeMounter{mounted: true}, TargetRoot: root}
	if err := binder.Publish(source, target); err == nil {
		t.Fatal("expected mismatched existing mount to fail")
	}
}

func TestPublishRejectsMissingOrNonDirectorySource(t *testing.T) {
	root := t.TempDir()
	binder := &Binder{Mounter: &fakeMounter{}, TargetRoot: root}
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
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o750); err != nil {
		t.Fatal(err)
	}
	mounter := &fakeMounter{mounted: true}
	binder := &Binder{Mounter: mounter, TargetRoot: root}
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
	binder := &Binder{Mounter: &fakeMounter{mountErr: errors.New("mount failed")}, TargetRoot: root}
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
	mountInfo := writeTestMountInfo(t, target, "/pool/volumes/"+testVolumeID)
	if err := (&Binder{Mounter: mounter, TargetRoot: filepath.Dir(target), MountInfoPath: mountInfo}).Unpublish(testVolumeID, target); err != nil {
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
	root := t.TempDir()
	binder := &Binder{Mounter: &fakeMounter{inspectErr: os.ErrNotExist}, TargetRoot: root}
	if err := binder.Unpublish(testVolumeID, filepath.Join(root, "missing")); err != nil {
		t.Fatal(err)
	}
}

func TestUnpublishPreservesTargetOnUnmountFailure(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	mountInfo := writeTestMountInfo(t, target, "/pool/volumes/"+testVolumeID)
	binder := &Binder{Mounter: &fakeMounter{mounted: true, unmountErr: errors.New("busy")}, TargetRoot: filepath.Dir(target), MountInfoPath: mountInfo}
	if err := binder.Unpublish(testVolumeID, target); err == nil {
		t.Fatal("expected unmount failure")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("target removed after unmount failure: %v", err)
	}
}

func TestHasPublishedTargetFiltersKubeletMountReferences(t *testing.T) {
	targetRoot := "/var/lib/kubelet/pods"
	binder := &Binder{Mounter: &fakeMounter{mountRefs: []string{
		"/host/var/lib/shiftpv/volumes/volume-a",
		"/var/lib/kubelet/pods/pod-a/volumes/kubernetes.io~csi/pvc-a/mount",
	}}}
	published, err := binder.HasPublishedTarget("/host/var/lib/shiftpv/volumes/volume-a", targetRoot)
	if err != nil || !published {
		t.Fatalf("expected kubelet publication: published=%v err=%v", published, err)
	}

	binder.Mounter = &fakeMounter{mountRefs: []string{"/host/var/lib/shiftpv/volumes/volume-a"}}
	published, err = binder.HasPublishedTarget("/host/var/lib/shiftpv/volumes/volume-a", targetRoot)
	if err != nil || published {
		t.Fatalf("unexpected publication outside target root: published=%v err=%v", published, err)
	}
}

func TestHasPublishedTargetReportsInspectionFailure(t *testing.T) {
	binder := &Binder{Mounter: &fakeMounter{refsErr: errors.New("mountinfo unavailable")}}
	if _, err := binder.HasPublishedTarget("/source", "/var/lib/kubelet/pods"); err == nil {
		t.Fatal("expected mount reference inspection failure")
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

func TestPublishRejectsSymlinkedTargetAncestor(t *testing.T) {
	root := t.TempDir()
	targetRoot := filepath.Join(root, "pods")
	source := filepath.Join(root, "source")
	outside := t.TempDir()
	if err := os.Mkdir(targetRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(source, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(targetRoot, "pod-uid")); err != nil {
		t.Fatal(err)
	}
	mounter := &fakeMounter{}
	binder := &Binder{Mounter: mounter, TargetRoot: targetRoot}
	target := filepath.Join(targetRoot, "pod-uid", "volumes", "csi", "mount")
	if err := binder.Publish(source, target); err == nil {
		t.Fatal("expected symlinked target ancestor to fail")
	}
	if mounter.mounts != 0 {
		t.Fatalf("unsafe target reached mount: mounts=%d", mounter.mounts)
	}
	if _, err := os.Stat(filepath.Join(outside, "volumes")); !os.IsNotExist(err) {
		t.Fatalf("publish escaped target root: %v", err)
	}
}

func TestUnpublishRejectsSymlinkedTargetAncestor(t *testing.T) {
	root := t.TempDir()
	targetRoot := filepath.Join(root, "pods")
	outside := t.TempDir()
	if err := os.Mkdir(targetRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(targetRoot, "pod-uid")); err != nil {
		t.Fatal(err)
	}
	mounter := &fakeMounter{mounted: true}
	binder := &Binder{Mounter: mounter, TargetRoot: targetRoot}
	target := filepath.Join(targetRoot, "pod-uid", "volumes", "csi", "mount")
	if err := binder.Unpublish(testVolumeID, target); err == nil {
		t.Fatal("expected symlinked target ancestor to fail")
	}
	if mounter.unmounts != 0 {
		t.Fatalf("unsafe target reached unmount: unmounts=%d", mounter.unmounts)
	}
}

func TestUnpublishRejectsDifferentMountedVolume(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	other := "shiftpv-11111111111111111111111111111111"
	mounter := &fakeMounter{mounted: true}
	binder := &Binder{
		Mounter: mounter, TargetRoot: filepath.Dir(target),
		MountInfoPath: writeTestMountInfo(t, target, "/pool/volumes/"+other),
	}
	if err := binder.Unpublish(testVolumeID, target); !errors.Is(err, ErrTargetVolumeMismatch) {
		t.Fatalf("mismatched mount error = %v", err)
	}
	if mounter.unmounts != 0 {
		t.Fatalf("different volume reached unmount: %d", mounter.unmounts)
	}
}

func writeTestMountInfo(t *testing.T, target, root string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mountinfo")
	line := "42 21 8:1 " + root + " " + target + " rw,relatime - ext4 /dev/sda1 rw\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
