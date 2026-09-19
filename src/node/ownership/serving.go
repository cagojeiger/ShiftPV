//go:build linux || darwin

package ownership

import (
	"context"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"github.com/project-jelly/ShiftPV/src/volume"
)

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
	switch {
	case err == nil:
		served, resumeErr := s.resumeServingPlacement(volumes, incoming, stageName, identity, record, markerExisted)
		if resumeErr != nil {
			return resumeErr
		}
		if served {
			return errors.Join(unix.Fsync(volumes), s.root.Sync())
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	default:
		if record, err = s.stageServingCopy(volumes, incoming, stageName, identity, markerExisted); err != nil {
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

// resumeServingPlacement re-enters a publish interrupted after its placement
// receipt was written. It reports whether the serving directory is already in
// place and verified, leaving only the durability barrier to the caller.
func (s *Store) resumeServingPlacement(volumes, incoming int, stageName string, identity volume.CopyIdentity, record placement, markerExisted bool) (bool, error) {
	if !markerExisted {
		return false, fmt.Errorf("%w: serving placement exists without copy intent", ErrNeedsReview)
	}
	if record.Identity != identity {
		return false, ErrIdentity
	}
	if verifyErr := s.verifyPath(volumes, identity.VolumeID, record, identity); verifyErr == nil {
		return true, nil
	} else if !errors.Is(verifyErr, unix.ENOENT) {
		return false, verifyErr
	}
	if verifyErr := s.verifyPath(incoming, stageName, record, identity); verifyErr != nil {
		return false, verifyErr
	}
	return false, nil
}

// stageServingCopy builds the staged serving directory and its receipts when no
// placement receipt exists yet. An unrecorded directory on either side is a
// state this node must not silently adopt.
func (s *Store) stageServingCopy(volumes, incoming int, stageName string, identity volume.CopyIdentity, markerExisted bool) (placement, error) {
	finalExists, err := pathExists(volumes, identity.VolumeID)
	if err != nil {
		return placement{}, err
	}
	if finalExists {
		return placement{}, fmt.Errorf("%w: serving directory exists without placement receipt", ErrNeedsReview)
	}
	stageExists, err := pathExists(incoming, stageName)
	if err != nil {
		return placement{}, err
	}
	if !markerExisted {
		if stageExists {
			return placement{}, fmt.Errorf("%w: unrecorded serving stage", ErrNeedsReview)
		}
		if err := s.ensureMarker(copyMarker(identity.CopyID), identity); err != nil {
			return placement{}, err
		}
	}
	if err := unix.Mkdirat(incoming, stageName, 0755); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return placement{}, err
		}
	}
	record, err := stagedPlacement(incoming, stageName, identity)
	if err != nil {
		return placement{}, err
	}
	if err := s.ensurePlacementMarker(identity.CopyID, record); err != nil {
		return placement{}, err
	}
	return record, nil
}

// stagedPlacement opens the staged directory without following symlinks and
// records the exact device and inode the receipt will be bound to.
func stagedPlacement(incoming int, stageName string, identity volume.CopyIdentity) (placement, error) {
	fd, err := unix.Openat(incoming, stageName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return placement{}, err
	}
	if err := unix.Fchmod(fd, 0755); err != nil {
		unix.Close(fd)
		return placement{}, err
	}
	record, err := placementFor(fd, identity)
	unix.Close(fd)
	if err != nil {
		return placement{}, err
	}
	return record, nil
}

func (s *Store) copyMarkerExists(identity volume.CopyIdentity) (bool, error) {
	if err := s.checkMarker(copyMarker(identity.CopyID), identity); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return false, nil
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
