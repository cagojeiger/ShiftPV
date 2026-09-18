//go:build !linux

package mount

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// secureTargetPath preserves the no-symlink contract for non-Linux developer
// builds. Production node plugins run on Linux and use the descriptor-anchored
// openat2 implementation.
func secureTargetPath(root, target string, create bool) error {
	if err := ValidateTarget(root, target); err != nil {
		return err
	}
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	if err != nil {
		return err
	}
	current := filepath.Clean(root)
	if err := rejectSymlink(current); err != nil {
		return fmt.Errorf("open target root: %w", err)
	}
	for _, name := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, name)
		if create {
			if err := os.Mkdir(current, 0o750); err != nil && !os.IsExist(err) {
				return fmt.Errorf("create target component %q: %w", name, err)
			}
		}
		if err := rejectSymlink(current); err != nil {
			return fmt.Errorf("open target component %q: %w", name, err)
		}
	}
	return nil
}

func rejectSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("path is not a real directory")
	}
	return nil
}
