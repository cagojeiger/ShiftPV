//go:build darwin

package ownership

import "golang.org/x/sys/unix"

func renameDirectory(from int, name string, to int, target string) error {
	return unix.RenameatxNp(from, name, to, target, unix.RENAME_EXCL)
}
