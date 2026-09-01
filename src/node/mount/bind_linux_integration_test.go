//go:build linux

package mount

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

const (
	linuxMountIntegrationEnv = "SHIFTPV_LINUX_MOUNT_INTEGRATION"
	permissionHelperEnv      = "SHIFTPV_PERMISSION_HELPER"
)

func requireLinuxMountIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv(linuxMountIntegrationEnv) != "1" {
		t.Skip("set SHIFTPV_LINUX_MOUNT_INTEGRATION=1 inside an isolated mount namespace")
	}
	if os.Geteuid() != 0 {
		t.Fatal("Linux mount integration parent must run as root")
	}
	parentNamespace := os.Getenv("SHIFTPV_PARENT_MOUNT_NAMESPACE")
	currentNamespace, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		t.Fatalf("read mount namespace: %v", err)
	}
	if parentNamespace == "" || currentNamespace == parentNamespace {
		t.Fatalf("test is not isolated from parent mount namespace: parent=%q current=%q", parentNamespace, currentNamespace)
	}
}

func TestLinuxMountIntegrationPublishAndUnpublish(t *testing.T) {
	requireLinuxMountIntegration(t)

	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "target")
	if err := os.Mkdir(source, 0o750); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(source, "sentinel")
	if err := os.WriteFile(sentinel, []byte("shiftpv-bind-mount"), 0o640); err != nil {
		t.Fatal(err)
	}

	binder := NewBinder()
	t.Cleanup(func() {
		mounted, err := binder.Mounter.IsMountPoint(target)
		if err == nil && mounted {
			_ = binder.Mounter.Unmount(target)
		}
	})

	if err := binder.Publish(source, target); err != nil {
		t.Fatalf("publish real bind mount: %v", err)
	}
	mounted, err := binder.Mounter.IsMountPoint(target)
	if err != nil || !mounted {
		t.Fatalf("target is not a mount point: mounted=%v err=%v", mounted, err)
	}
	data, err := os.ReadFile(filepath.Join(target, "sentinel"))
	if err != nil {
		t.Fatalf("read through bind mount: %v", err)
	}
	if string(data) != "shiftpv-bind-mount" {
		t.Fatalf("unexpected mounted data: %q", data)
	}
	if err := binder.Publish(source, target); err != nil {
		t.Fatalf("idempotent publish: %v", err)
	}
	if err := binder.Unpublish(target); err != nil {
		t.Fatalf("unpublish real bind mount: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target remained after unpublish: %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("source changed after unpublish: %v", err)
	}
}

func TestLinuxMountIntegrationPermissionFailure(t *testing.T) {
	if os.Getenv(permissionHelperEnv) == "1" {
		runPermissionFailureHelper(t)
		return
	}
	requireLinuxMountIntegration(t)

	root, err := os.MkdirTemp("/tmp", "shiftpv-permission-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "sentinel"), []byte("preserve-me"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")

	command := exec.Command(os.Args[0], "-test.run=^TestLinuxMountIntegrationPermissionFailure$")
	command.Env = append(os.Environ(),
		permissionHelperEnv+"=1",
		"SHIFTPV_PERMISSION_SOURCE="+source,
		"SHIFTPV_PERMISSION_TARGET="+target,
	)
	command.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: 65534, Gid: 65534},
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("permission helper failed: %v\n%s", err, output)
	}

	mounted, err := NewBinder().Mounter.IsMountPoint(target)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("inspect failed target: %v", err)
	}
	if mounted {
		t.Fatalf("permission failure leaked a mount at %q", target)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("permission failure cleanup left target: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(source, "sentinel"))
	if err != nil || string(data) != "preserve-me" {
		t.Fatalf("source changed after permission failure: data=%q err=%v", data, err)
	}
}

func runPermissionFailureHelper(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("permission helper unexpectedly retained root privileges")
	}
	source := os.Getenv("SHIFTPV_PERMISSION_SOURCE")
	target := os.Getenv("SHIFTPV_PERMISSION_TARGET")
	if source == "" || target == "" {
		t.Fatal("permission helper paths are not configured")
	}
	binder := NewBinder()
	if err := binder.Publish(source, target); err == nil {
		t.Fatal("unprivileged bind mount unexpectedly succeeded")
	}
	mounted, err := binder.Mounter.IsMountPoint(target)
	if err != nil {
		t.Fatalf("inspect failed publish: %v", err)
	}
	if mounted {
		t.Fatalf("failed publish left a mount at %q", target)
	}
	if err := binder.Unpublish(target); err != nil {
		t.Fatalf("clean failed publish target: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("failed publish target remained: %v", err)
	}
	if _, err := os.Stat(filepath.Join(source, "sentinel")); err != nil {
		t.Fatalf("source was not preserved: %v", err)
	}
	fmt.Fprintln(os.Stdout, "permission failure remained fail-closed and clean")
}
