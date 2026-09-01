package mount

import (
	"fmt"
	"os"
	"path/filepath"

	mountutils "k8s.io/mount-utils"
)

type Interface interface {
	Mount(source, target, fstype string, options []string) error
	Unmount(target string) error
	IsMountPoint(file string) (bool, error)
}

type Binder struct {
	Mounter Interface
}

func NewBinder() *Binder {
	return &Binder{Mounter: mountutils.New("")}
}

func (b *Binder) Publish(source, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("stat source directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("source path %q is not a directory", source)
	}
	if err := os.MkdirAll(target, 0o750); err != nil {
		return fmt.Errorf("create target directory: %w", err)
	}
	mounted, err := b.Mounter.IsMountPoint(target)
	if err != nil {
		return fmt.Errorf("inspect target mount: %w", err)
	}
	if mounted {
		targetInfo, statErr := os.Stat(target)
		if statErr != nil {
			return fmt.Errorf("stat mounted target: %w", statErr)
		}
		if !os.SameFile(info, targetInfo) {
			return fmt.Errorf("target %q is already mounted from a different source", target)
		}
		return nil
	}
	if err := b.Mounter.Mount(source, target, "", []string{"bind"}); err != nil {
		return fmt.Errorf("bind mount %q to %q: %w", source, target, err)
	}
	return nil
}

func (b *Binder) Unpublish(target string) error {
	mounted, err := b.Mounter.IsMountPoint(target)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect target mount: %w", err)
	}
	if mounted {
		if err := b.Mounter.Unmount(target); err != nil {
			return fmt.Errorf("unmount target %q: %w", target, err)
		}
	}
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove target directory: %w", err)
	}
	return nil
}

func ValidateTarget(root, target string) error {
	if !filepath.IsAbs(root) || !filepath.IsAbs(target) {
		return fmt.Errorf("target root and target path must be absolute")
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || len(rel) >= 3 && rel[:3] == "../" {
		return fmt.Errorf("target path %q is outside %q", target, root)
	}
	return nil
}
