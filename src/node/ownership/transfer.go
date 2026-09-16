//go:build linux || darwin

package ownership

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/project-jelly/ShiftPV/src/volume"
)

type transferIntent struct {
	OperationID string              `json:"operationID"`
	Incoming    volume.CopyIdentity `json:"incoming"`
	Destination volume.CopyIdentity `json:"destination"`
	Device      uint64              `json:"device"`
	Inode       uint64              `json:"inode"`
}

type transferReceipt struct {
	OperationID string              `json:"operationID"`
	Identity    volume.CopyIdentity `json:"identity"`
	Device      uint64              `json:"device"`
	Inode       uint64              `json:"inode"`
}

// PopulateIncoming owns the destination volume lock for the complete copy.
// The callback only receives a path derived from the validated copy identity.
func PopulateIncoming(ctx context.Context, root string, identity volume.CopyIdentity, operationID string, authority func(context.Context) error, populate func(context.Context, string) error) error {
	if identity.Role != volume.RoleIncoming || !volume.ValidIdentityToken(operationID) || authority == nil || populate == nil || identity.Validate() != nil {
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
	if err := store.ensureIncoming(identity); err != nil {
		return err
	}
	var receipt transferReceipt
	receiptName := operationMarker("copy-receipt", operationID)
	if err := store.readMarker(receiptName, &receipt); err == nil {
		if receipt.OperationID != operationID || receipt.Identity != identity {
			return ErrIdentity
		}
		return store.VerifyIncoming(identity)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := populate(ctx, filepath.Join(root, ".shiftpv", "incoming", identity.CopyID)); err != nil {
		return err
	}
	if err := authority(ctx); err != nil {
		return err
	}
	placed, err := store.readPlacement(identity.CopyID)
	if err != nil || store.VerifyIncoming(identity) != nil {
		return errors.Join(err, ErrIdentity)
	}
	receipt = transferReceipt{OperationID: operationID, Identity: identity, Device: placed.Device, Inode: placed.Inode}
	if err := store.ensureMarker(receiptName, receipt); err != nil {
		return err
	}
	return authority(ctx)
}

func (s *Store) ensureIncoming(identity volume.CopyIdentity) error {
	if identity.Role != volume.RoleIncoming || identity.InstallationID != s.pool.InstallationID || identity.PoolUID != s.pool.PoolUID {
		return ErrIdentity
	}
	markerExisted, err := s.copyMarkerExists(identity)
	if err != nil {
		return err
	}
	incoming, err := s.openOrCreateDirectory(int(s.control.Fd()), "incoming", 0700)
	if err != nil {
		return err
	}
	defer unix.Close(incoming)
	if current, err := s.readPlacement(identity.CopyID); err == nil {
		if !markerExisted {
			return fmt.Errorf("%w: incoming placement exists without copy intent", ErrNeedsReview)
		}
		return s.verifyPath(incoming, identity.CopyID, current, identity)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	directoryExists, err := pathExists(incoming, identity.CopyID)
	if err != nil {
		return err
	}
	if !markerExisted {
		if directoryExists {
			return fmt.Errorf("%w: unrecorded incoming directory", ErrNeedsReview)
		}
		if err := s.ensureMarker(copyMarker(identity.CopyID), identity); err != nil {
			return err
		}
	}
	if err := unix.Mkdirat(incoming, identity.CopyID, 0700); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return err
		}
	}
	fd, err := unix.Openat(incoming, identity.CopyID, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	placed, err := placementFor(fd, identity)
	unix.Close(fd)
	if err != nil {
		return err
	}
	if err := s.ensurePlacementMarker(identity.CopyID, placed); err != nil {
		return fmt.Errorf("%w: incoming directory created before placement receipt: %v", ErrNeedsReview, err)
	}
	return errors.Join(unix.Fsync(incoming), s.control.Sync())
}

func (s *Store) VerifyIncoming(identity volume.CopyIdentity) error {
	if identity.Role != volume.RoleIncoming {
		return ErrIdentity
	}
	if err := s.checkMarker(copyMarker(identity.CopyID), identity); err != nil {
		return err
	}
	placed, err := s.readPlacement(identity.CopyID)
	if err != nil {
		return err
	}
	incoming, err := unix.Openat(int(s.control.Fd()), "incoming", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(incoming)
	return s.verifyPath(incoming, identity.CopyID, placed, identity)
}

// PromoteIncoming atomically publishes the sealed incoming inode as the new
// serving copy and persists a receipt before returning success.
func PromoteIncoming(ctx context.Context, root string, incomingIdentity, servingIdentity volume.CopyIdentity, operationID string, authority func(context.Context) error) error {
	if err := validatePromotionRequest(incomingIdentity, servingIdentity, operationID, authority); err != nil {
		return err
	}
	if err := authority(ctx); err != nil {
		return err
	}
	store, err := OpenExisting(root, PoolIdentity{InstallationID: incomingIdentity.InstallationID, PoolUID: incomingIdentity.PoolUID})
	if err != nil {
		return err
	}
	defer store.Close()
	lock, err := store.Acquire(ctx, incomingIdentity.VolumeID)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := authority(ctx); err != nil {
		return err
	}
	receiptName := operationMarker("promotion-receipt", operationID)
	if replayed, replayErr := store.replayPromotionReceipt(receiptName, operationID, incomingIdentity, servingIdentity); replayed {
		return replayErr
	}
	intent, err := store.journalPromotion(operationID, incomingIdentity, servingIdentity)
	if err != nil {
		return err
	}
	incoming, err := unix.Openat(int(store.control.Fd()), "incoming", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(incoming)
	volumes, err := store.openOrCreateDirectory(int(store.root.Fd()), "volumes", 0755)
	if err != nil {
		return err
	}
	defer unix.Close(volumes)
	if err := publishPromotedDirectory(incoming, volumes, intent, incomingIdentity, servingIdentity); err != nil {
		return err
	}
	servingPlacement, err := store.sealPromotedPlacement(incoming, volumes, intent, servingIdentity)
	if err != nil {
		return err
	}
	return store.commitPromotion(ctx, receiptName, operationID, incomingIdentity, servingIdentity, servingPlacement, authority)
}

// validatePromotionRequest rejects every promotion whose two identities are not
// the same copy pair of the same volume on the same node.
func validatePromotionRequest(incomingIdentity, servingIdentity volume.CopyIdentity, operationID string, authority func(context.Context) error) error {
	if authority == nil || !volume.ValidIdentityToken(operationID) || incomingIdentity.Validate() != nil || servingIdentity.Validate() != nil ||
		incomingIdentity.Role != volume.RoleIncoming || servingIdentity.Role != volume.RoleServing || incomingIdentity.CopyID == servingIdentity.CopyID ||
		incomingIdentity.InstallationID != servingIdentity.InstallationID || incomingIdentity.PoolName != servingIdentity.PoolName || incomingIdentity.PoolUID != servingIdentity.PoolUID ||
		incomingIdentity.VolumeID != servingIdentity.VolumeID || incomingIdentity.VolumeUID != servingIdentity.VolumeUID || incomingIdentity.NodeName != servingIdentity.NodeName {
		return ErrIdentity
	}
	return nil
}

// replayPromotionReceipt reports whether a receipt of this exact promotion is
// already durable; the caller then returns the replayed outcome unchanged.
func (s *Store) replayPromotionReceipt(receiptName, operationID string, incomingIdentity, servingIdentity volume.CopyIdentity) (bool, error) {
	var receipt transferReceipt
	if err := s.readMarker(receiptName, &receipt); err == nil {
		if receipt.OperationID != operationID || receipt.Identity != servingIdentity {
			return true, ErrIdentity
		}
		if err := s.VerifyServing(servingIdentity); err != nil {
			return true, err
		}
		return true, s.removeCopyMetadata(incomingIdentity, receipt.Device, receipt.Inode)
	} else if !errors.Is(err, os.ErrNotExist) {
		return true, err
	}
	return false, nil
}

// journalPromotion proves the sealed incoming inode and persists the intent and
// the destination copy identity before any directory moves.
func (s *Store) journalPromotion(operationID string, incomingIdentity, servingIdentity volume.CopyIdentity) (transferIntent, error) {
	if err := s.checkMarker(copyMarker(incomingIdentity.CopyID), incomingIdentity); err != nil {
		return transferIntent{}, err
	}
	incomingPlacement, err := s.readPlacement(incomingIdentity.CopyID)
	if err != nil || incomingPlacement.Identity != incomingIdentity {
		return transferIntent{}, errors.Join(err, ErrIdentity)
	}
	intent := transferIntent{OperationID: operationID, Incoming: incomingIdentity, Destination: servingIdentity, Device: incomingPlacement.Device, Inode: incomingPlacement.Inode}
	if err := s.ensureMarker(operationMarker("promotion", operationID), intent); err != nil {
		return transferIntent{}, err
	}
	if err := s.ensureMarker(copyMarker(servingIdentity.CopyID), servingIdentity); err != nil {
		return transferIntent{}, err
	}
	return intent, nil
}

// publishPromotedDirectory moves the journaled incoming inode into place. A
// missing source means the move already happened before an interruption.
func publishPromotedDirectory(incoming, volumes int, intent transferIntent, incomingIdentity, servingIdentity volume.CopyIdentity) error {
	source, sourceErr := openIdentityDirectory(incoming, incomingIdentity.CopyID, localIntent{Device: intent.Device, Inode: intent.Inode})
	if sourceErr != nil {
		if !errors.Is(sourceErr, unix.ENOENT) {
			return sourceErr
		}
		return nil
	}
	unix.Close(source)
	if target, targetErr := unix.Openat(volumes, servingIdentity.VolumeID, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0); targetErr == nil {
		unix.Close(target)
		return ErrIdentity
	} else if !errors.Is(targetErr, unix.ENOENT) {
		return targetErr
	}
	return renameDirectory(incoming, incomingIdentity.CopyID, volumes, servingIdentity.VolumeID)
}

// sealPromotedPlacement proves the published directory is the journaled inode
// and makes that placement durable.
func (s *Store) sealPromotedPlacement(incoming, volumes int, intent transferIntent, servingIdentity volume.CopyIdentity) (placement, error) {
	target, err := unix.Openat(volumes, servingIdentity.VolumeID, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return placement{}, err
	}
	servingPlacement, err := placementFor(target, servingIdentity)
	unix.Close(target)
	if err != nil || servingPlacement.Device != intent.Device || servingPlacement.Inode != intent.Inode {
		return placement{}, errors.Join(err, ErrIdentity)
	}
	if err := s.ensurePlacementMarker(servingIdentity.CopyID, servingPlacement); err != nil {
		return placement{}, err
	}
	if err := errors.Join(unix.Fsync(volumes), unix.Fsync(incoming), s.root.Sync(), s.control.Sync()); err != nil {
		return placement{}, err
	}
	return servingPlacement, nil
}

// commitPromotion persists the receipt under live authority and then drops the
// superseded incoming metadata.
func (s *Store) commitPromotion(
	ctx context.Context, receiptName, operationID string,
	incomingIdentity, servingIdentity volume.CopyIdentity, servingPlacement placement,
	authority func(context.Context) error,
) error {
	if err := authority(ctx); err != nil {
		return err
	}
	receipt := transferReceipt{OperationID: operationID, Identity: servingIdentity, Device: servingPlacement.Device, Inode: servingPlacement.Inode}
	if err := s.ensureMarker(receiptName, receipt); err != nil {
		return err
	}
	if err := authority(ctx); err != nil {
		return err
	}
	if err := s.removeCopyMetadata(incomingIdentity, receipt.Device, receipt.Inode); err != nil {
		return fmt.Errorf("finish promotion metadata: %w", err)
	}
	return authority(ctx)
}
