//go:build linux || darwin

// Package ownership persists node-local storage identity outside workload data.
// API authority must still be checked by every caller before and under the lock.
package ownership

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/project-jelly/ShiftPV/src/volume"
)

var (
	ErrBusy        = errors.New("volume operation is already locked")
	ErrIdentity    = errors.New("node storage identity mismatch")
	ErrNeedsReview = errors.New("node storage state needs review")
)

type PoolIdentity struct {
	InstallationID string `json:"installationID"`
	PoolUID        string `json:"poolUID"`
}

type placement struct {
	Identity volume.CopyIdentity `json:"identity"`
	Device   uint64              `json:"device"`
	Inode    uint64              `json:"inode"`
}

type localIntent struct {
	OperationID string              `json:"operationID"`
	Target      volume.CopyIdentity `json:"target"`
	Device      uint64              `json:"device"`
	Inode       uint64              `json:"inode"`
}

type Receipt struct {
	OperationID string              `json:"operationID"`
	Target      volume.CopyIdentity `json:"target"`
	Device      uint64              `json:"device"`
	Inode       uint64              `json:"inode"`
	Retired     bool                `json:"retired"`
	Purged      bool                `json:"purged"`
}

type Store struct {
	control    *os.File
	placements *os.File
	root       *os.File
	pool       PoolIdentity
}

func WithLock(ctx context.Context, root string, pool PoolIdentity, volumeID string, operation func(*Store) error) error {
	return withLock(ctx, root, pool, volumeID, false, operation)
}

// WithExistingLock serializes a read-only verification with mutating volume
// operations without creating or modifying node-local state.
func WithExistingLock(ctx context.Context, root string, pool PoolIdentity, volumeID string, operation func(*Store) error) error {
	return withLock(ctx, root, pool, volumeID, true, operation)
}

// withLock runs operation under the volume lock of an existing Pool store.
// existingOnly joins the lock through an already created read-only file, which
// is what keeps a read-only verification from creating node-local state.
func withLock(ctx context.Context, root string, pool PoolIdentity, volumeID string, existingOnly bool, operation func(*Store) error) error {
	if operation == nil {
		return ErrIdentity
	}
	store, err := OpenExisting(root, pool)
	if err != nil {
		return err
	}
	defer store.Close()
	acquire := store.Acquire
	if existingOnly {
		acquire = store.AcquireExisting
	}
	lock, err := acquire(ctx, volumeID)
	if err != nil {
		return err
	}
	defer lock.Close()
	return operation(store)
}

// VerifyServingPath proves that the current serving directory still matches
// its persisted copy and inode identity without changing the Pool.
func VerifyServingPath(root string, identity volume.CopyIdentity) error {
	if identity.Role != volume.RoleServing || identity.Validate() != nil {
		return ErrIdentity
	}
	store, err := OpenExisting(root, PoolIdentity{InstallationID: identity.InstallationID, PoolUID: identity.PoolUID})
	if err != nil {
		return err
	}
	defer store.Close()
	return store.VerifyServing(identity)
}

func Open(root string, identity PoolIdentity) (*Store, error) {
	return open(root, identity, true)
}

func OpenExisting(root string, identity PoolIdentity) (*Store, error) {
	return open(root, identity, false)
}

func open(root string, identity PoolIdentity, initialize bool) (*Store, error) {
	if !filepath.IsAbs(root) || !volume.ValidIdentityToken(identity.InstallationID) || !volume.ValidIdentityToken(identity.PoolUID) {
		return nil, ErrIdentity
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	if initialize {
		if err := unix.Mkdirat(rootFD, ".shiftpv", 0700); err != nil && !errors.Is(err, unix.EEXIST) {
			unix.Close(rootFD)
			return nil, err
		}
	}
	controlFD, err := unix.Openat(rootFD, ".shiftpv", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		unix.Close(rootFD)
		return nil, err
	}
	if initialize {
		if err := unix.Mkdirat(controlFD, "placements", 0700); err != nil && !errors.Is(err, unix.EEXIST) {
			unix.Close(controlFD)
			unix.Close(rootFD)
			return nil, err
		}
	}
	placementsFD, err := unix.Openat(controlFD, "placements", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		unix.Close(controlFD)
		unix.Close(rootFD)
		return nil, err
	}
	store := &Store{
		root: os.NewFile(uintptr(rootFD), "shiftpv-pool"), control: os.NewFile(uintptr(controlFD), "shiftpv-control"),
		placements: os.NewFile(uintptr(placementsFD), "shiftpv-placements"), pool: identity,
	}
	if err := store.validateControl(); err != nil {
		store.Close()
		return nil, err
	}
	if initialize {
		err = store.ensureMarker("pool.json", identity)
	} else {
		err = store.checkMarker("pool.json", identity)
	}
	if err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	return errors.Join(s.placements.Close(), s.control.Close(), s.root.Close())
}

func (s *Store) Acquire(ctx context.Context, volumeID string) (*os.File, error) {
	return s.acquire(ctx, volumeID, unix.O_RDWR|unix.O_CREAT, 0600)
}

// AcquireExisting joins the volume lock through an existing read-only file.
// PrepareServing creates this lock before a serving copy becomes authoritative.
func (s *Store) AcquireExisting(ctx context.Context, volumeID string) (*os.File, error) {
	return s.acquire(ctx, volumeID, unix.O_RDONLY, 0)
}

func (s *Store) acquire(ctx context.Context, volumeID string, flags int, mode uint32) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := volume.ValidateID(volumeID); err != nil {
		return nil, err
	}
	file, err := s.openControl("lock-"+volumeID, flags, mode)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrBusy
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		file.Close()
		return nil, err
	}
	if err := s.checkMarker("pool.json", s.pool); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func pathExists(parent int, name string) (bool, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) verifyPath(parent int, name string, record placement, expected volume.CopyIdentity) error {
	if record.Identity != expected || record.Device == 0 || record.Inode == 0 {
		return ErrIdentity
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	actual, err := placementFor(fd, expected)
	if err != nil || actual != record {
		return ErrIdentity
	}
	return nil
}

func (s *Store) openOrCreateDirectory(parent int, name string, mode uint32) (int, error) {
	if err := unix.Mkdirat(parent, name, mode); err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, err
	}
	return unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
}

func (s *Store) validateControl() error {
	if err := validateDirectory(s.control); err != nil {
		return err
	}
	return validateDirectory(s.placements)
}

func validateDirectory(directory *os.File) error {
	info, err := directory.Stat()
	if err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &stat); err != nil {
		return err
	}
	if info.Mode().Perm()&0022 != 0 || stat.Uid != uint32(os.Geteuid()) {
		return ErrIdentity
	}
	return nil
}

func placementFor(fd int, identity volume.CopyIdentity) (placement, error) {
	var stat unix.Stat_t
	err := unix.Fstat(fd, &stat)
	return placement{Identity: identity, Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}, err
}
