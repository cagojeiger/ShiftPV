//go:build linux

package ownership

import "golang.org/x/sys/unix"

func renameDirectory(from int, name string, to int, target string) error {
	return unix.Renameat2(from, name, to, target, unix.RENAME_NOREPLACE)
}
