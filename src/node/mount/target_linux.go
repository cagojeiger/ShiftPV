//go:build linux

package mount

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// secureTargetPath anchors every target component below an already opened
// root and rejects symlinks at the mount boundary. The kubelet-owned target
// tree is assumed not to be adversarially replaced after this check.
func secureTargetPath(root, target string, create bool) error {
	if err := ValidateTarget(root, target); err != nil {
		return err
	}
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	if err != nil {
		return err
	}

	current, err := unix.Open(filepath.Clean(root), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open target root: %w", err)
	}
	defer func() { _ = unix.Close(current) }()

	for _, name := range strings.Split(relative, string(filepath.Separator)) {
		if create {
			if err := unix.Mkdirat(current, name, 0o750); err != nil && !errors.Is(err, unix.EEXIST) {
				return fmt.Errorf("create target component %q: %w", name, err)
			}
		}
		next, err := unix.Openat2(current, name, &unix.OpenHow{
			Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW,
			Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
		})
		if err != nil {
			return fmt.Errorf("open target component %q: %w", name, err)
		}
		_ = unix.Close(current)
		current = next
	}
	return nil
}
