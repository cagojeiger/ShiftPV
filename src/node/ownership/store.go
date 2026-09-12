//go:build linux || darwin

// Package ownership persists node-local storage identity outside workload data.
// API authority must still be checked by every caller before and under the lock.
package ownership

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/util/uuid"

	"github.com/cagojeiger/ShiftPV/src/volume"
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
	if operation == nil {
		return ErrIdentity
	}
	store, err := OpenExisting(root, pool)
	if err != nil {
		return err
	}
	defer store.Close()
	lock, err := store.Acquire(ctx, volumeID)
	if err != nil {
		return err
	}
	defer lock.Close()
	return operation(store)
}

// WithExistingLock serializes a read-only verification with mutating volume
// operations without creating or modifying node-local state.
func WithExistingLock(ctx context.Context, root string, pool PoolIdentity, volumeID string, operation func(*Store) error) error {
	if operation == nil {
		return ErrIdentity
	}
	store, err := OpenExisting(root, pool)
	if err != nil {
		return err
	}
	defer store.Close()
	lock, err := store.AcquireExisting(ctx, volumeID)
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

// PrepareServing performs one API-authorized create effect. The callback must
// re-read live authority; a captured boolean is not an authority check.
func PrepareServing(ctx context.Context, root string, identity volume.CopyIdentity, authority func(context.Context) error) error {
	if authority == nil {
		return ErrIdentity
	}
	if err := authority(ctx); err != nil {
		return err
	}
	store, err := Open(root, PoolIdentity{InstallationID: identity.InstallationID, PoolUID: identity.PoolUID})
	if err != nil {
		return err
	}
	defer store.Close()
	lock, err := store.Acquire(ctx, identity.VolumeID)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := authority(ctx); err != nil {
		return err
	}
	if err := store.ensureServing(identity); err != nil {
		return err
	}
	return authority(ctx)
}

func (s *Store) ensureServing(identity volume.CopyIdentity) error {
	if identity.Role != volume.RoleServing || identity.InstallationID != s.pool.InstallationID || identity.PoolUID != s.pool.PoolUID {
		return ErrIdentity
	}
	if err := identity.Validate(); err != nil {
		return err
	}
	markerExisted, err := s.copyMarkerExists(identity)
	if err != nil {
		return err
	}
	volumes, err := s.openOrCreateDirectory(int(s.root.Fd()), "volumes", 0755)
	if err != nil {
		return err
	}
	defer unix.Close(volumes)
	incoming, err := s.openOrCreateDirectory(int(s.control.Fd()), "incoming", 0700)
	if err != nil {
		return err
	}
	defer unix.Close(incoming)
	stageName := "create-" + identity.CopyID
	record, err := s.readPlacement(identity.CopyID)
	if err == nil {
		if !markerExisted {
			return fmt.Errorf("%w: serving placement exists without copy intent", ErrNeedsReview)
		}
		if record.Identity != identity {
			return ErrIdentity
		}
		if verifyErr := s.verifyPath(volumes, identity.VolumeID, record, identity); verifyErr == nil {
			return errors.Join(unix.Fsync(volumes), s.root.Sync())
		} else if !errors.Is(verifyErr, unix.ENOENT) {
			return verifyErr
		}
		if verifyErr := s.verifyPath(incoming, stageName, record, identity); verifyErr != nil {
			return verifyErr
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else {
		finalExists, existsErr := pathExists(volumes, identity.VolumeID)
		if existsErr != nil {
			return existsErr
		}
		if finalExists {
			return fmt.Errorf("%w: serving directory exists without placement receipt", ErrNeedsReview)
		}
		stageExists, existsErr := pathExists(incoming, stageName)
		if existsErr != nil {
			return existsErr
		}
		if !markerExisted {
			if stageExists {
				return fmt.Errorf("%w: unrecorded serving stage", ErrNeedsReview)
			}
			if err := s.ensureMarker(copyMarker(identity.CopyID), identity); err != nil {
				return err
			}
		}
		if err := unix.Mkdirat(incoming, stageName, 0755); err != nil {
			if !errors.Is(err, unix.EEXIST) {
				return err
			}
		}
		fd, openErr := unix.Openat(incoming, stageName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			return openErr
		}
		if err := unix.Fchmod(fd, 0755); err != nil {
			unix.Close(fd)
			return err
		}
		record, err = placementFor(fd, identity)
		unix.Close(fd)
		if err != nil {
			return err
		}
		if err := s.ensurePlacementMarker(identity.CopyID, record); err != nil {
			return err
		}
	}
	if err := renameDirectory(incoming, stageName, volumes, identity.VolumeID); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("%w: unrecorded serving directory", ErrNeedsReview)
		}
		return err
	}
	if err := s.verifyPath(volumes, identity.VolumeID, record, identity); err != nil {
		return err
	}
	return errors.Join(unix.Fsync(incoming), unix.Fsync(volumes), s.control.Sync(), s.root.Sync())
}

func (s *Store) copyMarkerExists(identity volume.CopyIdentity) (bool, error) {
	if err := s.checkMarker(copyMarker(identity.CopyID), identity); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return false, nil
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

func (s *Store) VerifyServing(identity volume.CopyIdentity) error {
	if err := s.checkMarker(copyMarker(identity.CopyID), identity); err != nil {
		return err
	}
	record, err := s.readPlacement(identity.CopyID)
	if err != nil {
		return err
	}
	volumes, err := unix.Openat(int(s.root.Fd()), "volumes", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(volumes)
	return s.verifyPath(volumes, identity.VolumeID, record, identity)
}

func (s *Store) ensureMarker(name string, value any) error {
	return s.ensureMarkerAt(s.control, name, value)
}

func (s *Store) ensurePlacementMarker(copyID string, value placement) error {
	return s.ensureMarkerAt(s.placements, placementMarker(copyID), value)
}

func (s *Store) ensureMarkerAt(directory *os.File, name string, value any) error {
	lock, err := s.openControl("identity.lock", unix.O_RDWR|unix.O_CREAT, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	if err := s.checkMarkerAt(directory, name, value); err == nil {
		return directory.Sync()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := canonical(value)
	if err != nil {
		return err
	}
	temporary := "pending-" + string(uuid.NewUUID())
	file, err := openMarker(directory, temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(int(directory.Fd()), temporary, 0)
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	if err := unix.Renameat(int(directory.Fd()), temporary, int(directory.Fd()), name); err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		return err
	}
	return s.checkMarkerAt(directory, name, value)
}

func (s *Store) checkMarker(name string, expected any) error {
	return s.checkMarkerAt(s.control, name, expected)
}

func (s *Store) checkPlacementMarker(copyID string, expected placement) error {
	return s.checkMarkerAt(s.placements, placementMarker(copyID), expected)
}

func (s *Store) checkMarkerAt(directory *os.File, name string, expected any) error {
	file, err := openMarker(directory, name, unix.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil {
		return err
	}
	want, err := canonical(expected)
	if err != nil {
		return err
	}
	if len(data) > 8192 || string(data) != string(want) {
		return ErrIdentity
	}
	return nil
}

func (s *Store) removeMarkerAt(directory *os.File, name string, expected any) error {
	lock, err := s.openControl("identity.lock", unix.O_RDWR|unix.O_CREAT, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	if err := s.checkMarkerAt(directory, name, expected); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := unix.Unlinkat(int(directory.Fd()), name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return directory.Sync()
}

func (s *Store) removeCopyMetadata(identity volume.CopyIdentity, device, inode uint64) error {
	placed := placement{Identity: identity, Device: device, Inode: inode}
	if err := s.removeMarkerAt(s.placements, placementMarker(identity.CopyID), placed); err != nil {
		return err
	}
	return s.removeMarkerAt(s.control, copyMarker(identity.CopyID), identity)
}

func (s *Store) openControl(name string, flags int, mode uint32) (*os.File, error) {
	return openMarker(s.control, name, flags, mode)
}

func (s *Store) openPlacement(name string, flags int, mode uint32) (*os.File, error) {
	return openMarker(s.placements, name, flags, mode)
}

func openMarker(directory *os.File, name string, flags int, mode uint32) (*os.File, error) {
	if name == "" || strings.ContainsAny(name, "/\\\x00") {
		return nil, ErrIdentity
	}
	fd, err := unix.Openat(int(directory.Fd()), name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, mode)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) {
		file.Close()
		return nil, ErrIdentity
	}
	return file, nil
}

func (s *Store) readPlacement(copyID string) (placement, error) {
	var envelope struct {
		Version int       `json:"version"`
		Value   placement `json:"value"`
	}
	file, err := s.openPlacement(placementMarker(copyID), unix.O_RDONLY, 0)
	if err != nil {
		return placement{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil || len(data) > 8192 || json.Unmarshal(data, &envelope) != nil || envelope.Version != 1 {
		return placement{}, ErrIdentity
	}
	if err := s.checkPlacementMarker(copyID, envelope.Value); err != nil {
		return placement{}, err
	}
	return envelope.Value, nil
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

func canonical(value any) ([]byte, error) {
	return json.Marshal(struct {
		Version int `json:"version"`
		Value   any `json:"value"`
	}{Version: 1, Value: value})
}

func copyMarker(copyID string) string      { return "copy-" + copyID + ".json" }
func placementMarker(copyID string) string { return "placement-" + copyID + ".json" }

func operationMarker(prefix, operationID string) string {
	sum := sha256.Sum256([]byte(operationID))
	return prefix + "-" + hex.EncodeToString(sum[:16]) + ".json"
}
