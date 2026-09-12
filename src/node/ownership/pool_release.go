//go:build linux || darwin

package ownership

import (
	"context"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// ReleaseEmptyPool removes only the exact Pool identity marker. Copy data and
// copy metadata are never removed here; their presence keeps the Pool intact.
func ReleaseEmptyPool(ctx context.Context, root string, identity PoolIdentity) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store, err := OpenExisting(root, identity)
	if errors.Is(err, os.ErrNotExist) {
		empty, inspectErr := managedPoolDataEmpty(root)
		if inspectErr != nil {
			return inspectErr
		}
		if !empty {
			return ErrNeedsReview
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer store.Close()

	lock, err := store.openControl("identity.lock", unix.O_RDWR|unix.O_CREAT, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return ErrBusy
		}
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.checkMarker("pool.json", identity); err != nil {
		return err
	}
	empty, err := store.managedDataEmpty()
	if err != nil {
		return err
	}
	if !empty {
		return ErrNeedsReview
	}
	if err := unix.Unlinkat(int(store.control.Fd()), "pool.json", 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return store.control.Sync()
}

func (s *Store) managedDataEmpty() (bool, error) {
	for _, candidate := range []struct {
		parent *os.File
		name   string
	}{
		{parent: s.root, name: "volumes"},
		{parent: s.control, name: "incoming"},
		{parent: s.control, name: "retired"},
	} {
		empty, err := directoryEmptyAt(int(candidate.parent.Fd()), candidate.name)
		if err != nil || !empty {
			return empty, err
		}
	}
	return readDirectoryEmpty(s.placements)
}

func managedPoolDataEmpty(root string) (bool, error) {
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false, err
	}
	defer unix.Close(rootFD)
	empty, err := directoryEmptyAt(rootFD, "volumes")
	if err != nil || !empty {
		return empty, err
	}
	controlFD, err := unix.Openat(rootFD, ".shiftpv", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer unix.Close(controlFD)
	for _, name := range []string{"placements", "incoming", "retired"} {
		empty, err := directoryEmptyAt(controlFD, name)
		if err != nil || !empty {
			return empty, err
		}
	}
	return true, nil
}

func directoryEmptyAt(parent int, name string) (bool, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	directory := os.NewFile(uintptr(fd), name)
	defer directory.Close()
	return readDirectoryEmpty(directory)
}

func readDirectoryEmpty(directory *os.File) (bool, error) {
	names, err := directory.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return len(names) == 0, nil
	}
	return false, err
}
