package pathcheck

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Check performs a bounded set of lstat calls. Only ENOENT proves absence.
// Permission, I/O, non-directory and symlink errors never become success.
func Check(root, move, volume string) error {
	return inspect(root, move, volume, os.Lstat)
}

func inspect(root, move, volume string, lstat func(string) (os.FileInfo, error)) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" {
		return fmt.Errorf("invalid cleanup root")
	}
	for _, id := range []string{move, volume} {
		if id == "" || id == "." || id == ".." || filepath.Base(id) != id {
			return fmt.Errorf("invalid cleanup identity")
		}
	}
	info, err := lstat(root)
	if err != nil {
		return fmt.Errorf("inspect Pool: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("Pool is not an ordinary directory")
	}
	for _, relative := range []string{
		"volumes/" + volume, ".shiftpv/retired/" + move, ".shiftpv/aborted/" + move + "-final",
		".shiftpv/aborted/" + move + "-incoming", ".shiftpv/incoming/" + move,
	} {
		parts := strings.Split(relative, "/")
		current := root
		for index, part := range parts {
			current = filepath.Join(current, part)
			info, err = lstat(current)
			if os.IsNotExist(err) {
				break
			}
			if err != nil {
				return fmt.Errorf("inspect %s: %w", current, err)
			}
			if index == len(parts)-1 {
				return fmt.Errorf("cleanup path still exists: %s", current)
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("cleanup ancestor is not an ordinary directory: %s", current)
			}
		}
	}
	return nil
}
