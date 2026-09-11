//go:build linux

package ownership

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func purgeRetired(ctx context.Context, store *Store, intent localIntent) error {
	parent, err := openPurgeDirectory(int(store.control.Fd()), "retired")
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	child, err := openPurgeDirectory(parent, intent.Target.CopyID)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(child, &stat); err != nil || uint64(stat.Dev) != intent.Device || uint64(stat.Ino) != intent.Inode {
		unix.Close(child)
		return ErrIdentity
	}
	if err := purgeContents(ctx, child, 0); err != nil {
		unix.Close(child)
		return err
	}
	if err := unix.Close(child); err != nil {
		return err
	}
	if err := unix.Unlinkat(parent, intent.Target.CopyID, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return unix.Fsync(parent)
}

func openPurgeDirectory(parent int, name string) (int, error) {
	return unix.Openat2(parent, name, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV})
}

func purgeContents(ctx context.Context, fd, depth int) error {
	if depth > 256 {
		return fmt.Errorf("purge depth exceeds 256")
	}
	readerFD, err := openPurgeDirectory(fd, ".")
	if err != nil {
		return err
	}
	reader := os.NewFile(uintptr(readerFD), "retired-copy")
	defer reader.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		names, readErr := reader.Readdirnames(128)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		for _, name := range names {
			child, childErr := openPurgeDirectory(fd, name)
			if childErr == nil {
				childErr = purgeContents(ctx, child, depth+1)
				unix.Close(child)
				if childErr == nil {
					childErr = unix.Unlinkat(fd, name, unix.AT_REMOVEDIR)
				}
			} else if errors.Is(childErr, unix.ENOTDIR) || errors.Is(childErr, unix.ELOOP) {
				childErr = unix.Unlinkat(fd, name, 0)
			}
			if childErr != nil {
				return childErr
			}
		}
		if errors.Is(readErr, io.EOF) {
			return unix.Fsync(fd)
		}
	}
}
