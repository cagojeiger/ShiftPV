//go:build linux || darwin

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

	"golang.org/x/sys/unix"

	"github.com/project-jelly/ShiftPV/src/volume"
)

// ReclaimWithResume distinguishes a fresh destructive effect from replay of an
// exact locally journaled effect. Callers may relax only observation checks
// that the effect itself necessarily invalidated; API ownership stays required.
func ReclaimWithResume(ctx context.Context, root string, target volume.CopyIdentity, operationID string, authority func(context.Context, bool) error) (Receipt, string, error) {
	return reclaimWithState(ctx, root, target, operationID, authority, preflightPurge, purgeRetired)
}

func reclaimWithState(
	ctx context.Context,
	root string,
	target volume.CopyIdentity,
	operationID string,
	authority func(context.Context, bool) error,
	preflight func(*Store) error,
	purge func(context.Context, *Store, localIntent) error,
) (Receipt, string, error) {
	if authority == nil || !volume.ValidIdentityToken(operationID) || target.Validate() != nil {
		return Receipt{}, "", ErrIdentity
	}
	store, err := OpenExisting(root, PoolIdentity{InstallationID: target.InstallationID, PoolUID: target.PoolUID})
	if err != nil {
		return Receipt{}, "", err
	}
	defer store.Close()
	lock, err := store.Acquire(ctx, target.VolumeID)
	if err != nil {
		return Receipt{}, "", err
	}
	defer lock.Close()
	if replayed, completed, digest, replayErr := store.replayReclaimReceipt(ctx, target, operationID, authority); replayed {
		return completed, digest, replayErr
	}
	intent, effectStarted, err := store.prepareReclaimIntent(ctx, target, operationID, authority)
	if err != nil {
		return Receipt{}, "", err
	}
	if err := store.applyReclaimEffect(ctx, target, intent, effectStarted, authority, preflight, purge); err != nil {
		return Receipt{}, "", err
	}
	return store.settleReclaim(target, operationID, intent)
}

// replayReclaimReceipt reports whether this exact cleanup already has a durable
// receipt; the caller then returns the replayed outcome unchanged.
func (s *Store) replayReclaimReceipt(ctx context.Context, target volume.CopyIdentity, operationID string, authority func(context.Context, bool) error) (bool, Receipt, string, error) {
	var completed Receipt
	if err := s.readMarker(operationMarker("receipt", operationID), &completed); err == nil {
		if completed.OperationID != operationID || completed.Target != target || !completed.Retired || !completed.Purged {
			return true, Receipt{}, "", ErrIdentity
		}
		if err := authority(ctx, true); err != nil {
			return true, Receipt{}, "", err
		}
		if err := s.verifyAbsent(target); err != nil {
			return true, Receipt{}, "", err
		}
		if err := s.removeCopyMetadata(target, completed.Device, completed.Inode); err != nil {
			return true, Receipt{}, "", fmt.Errorf("finish cleanup metadata: %w", err)
		}
		digest, digestErr := receiptDigest(completed)
		return true, completed, digest, digestErr
	} else if !errors.Is(err, os.ErrNotExist) {
		return true, Receipt{}, "", err
	}
	return false, Receipt{}, "", nil
}

// prepareReclaimIntent reads any journaled effect, checks API authority against
// what that journal already permits, and returns the intent to apply.
func (s *Store) prepareReclaimIntent(ctx context.Context, target volume.CopyIdentity, operationID string, authority func(context.Context, bool) error) (localIntent, bool, error) {
	intent, intentExists, err := s.localIntent(target, operationID)
	if err != nil {
		return localIntent{}, false, fmt.Errorf("read local cleanup intent: %w", err)
	}
	effectStarted := false
	if intentExists {
		effectStarted, err = s.cleanupEffectStarted(target, intent)
		if err != nil {
			return localIntent{}, false, fmt.Errorf("inspect local cleanup effect: %w", err)
		}
	}
	if err := authority(ctx, effectStarted); err != nil {
		return localIntent{}, false, err
	}
	if effectStarted {
		return intent, true, nil
	}
	intent, err = s.confirmFreshReclaimTarget(target, operationID, intent, intentExists)
	return intent, false, err
}

// confirmFreshReclaimTarget proves the untouched copy still is the recorded one
// and journals the intent that the destructive effect will replay from.
func (s *Store) confirmFreshReclaimTarget(target volume.CopyIdentity, operationID string, intent localIntent, intentExists bool) (localIntent, error) {
	if err := s.checkMarker(copyMarker(target.CopyID), target); err != nil {
		return localIntent{}, fmt.Errorf("verify copy identity: %w", err)
	}
	placement, err := s.readPlacement(target.CopyID)
	if err != nil || placement.Identity != target {
		return localIntent{}, fmt.Errorf("verify copy placement: %w", errors.Join(err, ErrIdentity))
	}
	if intentExists {
		if intent.Device != placement.Device || intent.Inode != placement.Inode {
			return localIntent{}, ErrIdentity
		}
		return intent, nil
	}
	journaled, err := s.ensureLocalIntent(target, operationID, placement)
	if err != nil {
		return localIntent{}, fmt.Errorf("persist local cleanup intent: %w", err)
	}
	return journaled, nil
}

// applyReclaimEffect rechecks authority immediately before the destructive
// effect and then retires and purges the journaled inode.
func (s *Store) applyReclaimEffect(
	ctx context.Context, target volume.CopyIdentity, intent localIntent, effectStarted bool,
	authority func(context.Context, bool) error, preflight func(*Store) error,
	purge func(context.Context, *Store, localIntent) error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := authority(ctx, effectStarted); err != nil {
		return fmt.Errorf("recheck cleanup authority before filesystem effect: %w", err)
	}
	if err := preflight(s); err != nil {
		return fmt.Errorf("verify safe purge support: %w", err)
	}
	if err := s.retire(target, intent); err != nil {
		return fmt.Errorf("retire cleanup target: %w", err)
	}
	if err := purge(ctx, s, intent); err != nil {
		return fmt.Errorf("purge retired target: %w", err)
	}
	return nil
}

// settleReclaim persists the receipt, proves the target is gone and drops the
// copy metadata the receipt supersedes.
func (s *Store) settleReclaim(target volume.CopyIdentity, operationID string, intent localIntent) (Receipt, string, error) {
	receipt := Receipt{OperationID: operationID, Target: target, Device: intent.Device, Inode: intent.Inode, Retired: true, Purged: true}
	if err := s.ensureMarker(operationMarker("receipt", operationID), receipt); err != nil {
		return Receipt{}, "", fmt.Errorf("persist local cleanup receipt: %w", err)
	}
	if err := s.verifyAbsent(target); err != nil {
		return Receipt{}, "", fmt.Errorf("verify cleanup absence: %w", err)
	}
	if err := s.removeCopyMetadata(target, receipt.Device, receipt.Inode); err != nil {
		return Receipt{}, "", fmt.Errorf("finish cleanup metadata: %w", err)
	}
	digest, err := receiptDigest(receipt)
	return receipt, digest, err
}

func (s *Store) ensureLocalIntent(target volume.CopyIdentity, operationID string, placed placement) (localIntent, error) {
	name := operationMarker("cleanup", operationID)
	var existing localIntent
	if err := s.readMarker(name, &existing); err == nil {
		if existing.OperationID != operationID || existing.Target != target || existing.Device != placed.Device || existing.Inode != placed.Inode {
			return localIntent{}, ErrIdentity
		}
		return existing, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return localIntent{}, err
	}
	if err := s.verifyCurrentPath(placed); err != nil {
		return localIntent{}, err
	}
	intent := localIntent{OperationID: operationID, Target: target, Device: placed.Device, Inode: placed.Inode}
	if err := s.ensureMarker(name, intent); err != nil {
		return localIntent{}, err
	}
	return intent, nil
}

func (s *Store) localIntent(target volume.CopyIdentity, operationID string) (localIntent, bool, error) {
	var intent localIntent
	err := s.readMarker(operationMarker("cleanup", operationID), &intent)
	if errors.Is(err, os.ErrNotExist) {
		return localIntent{}, false, nil
	}
	if err != nil {
		return localIntent{}, false, err
	}
	if intent.OperationID != operationID || intent.Target != target {
		return localIntent{}, false, ErrIdentity
	}
	return intent, true, nil
}

func (s *Store) cleanupEffectStarted(target volume.CopyIdentity, intent localIntent) (bool, error) {
	sourceParent, sourceName, closeParent, err := s.openRoleParent(target)
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return false, err
	}
	if closeParent && err == nil {
		defer unix.Close(sourceParent)
	}
	sourcePresent := false
	if err == nil {
		sourcePresent, err = exactDirectoryPresent(sourceParent, sourceName, intent)
		if err != nil {
			return false, err
		}
	}
	if target.Role == volume.RoleRetired {
		return !sourcePresent, nil
	}
	retiredParent, err := unix.Openat(int(s.control.Fd()), "retired", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return false, err
	}
	retiredPresent := false
	if err == nil {
		defer unix.Close(retiredParent)
		retiredPresent, err = exactDirectoryPresent(retiredParent, target.CopyID, intent)
		if err != nil {
			return false, err
		}
	}
	if sourcePresent && retiredPresent {
		return false, ErrIdentity
	}
	return !sourcePresent || retiredPresent, nil
}

func exactDirectoryPresent(parent int, name string, intent localIntent) (bool, error) {
	fd, err := openIdentityDirectory(parent, name, intent)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, unix.Close(fd)
}

func (s *Store) retire(target volume.CopyIdentity, intent localIntent) error {
	retired, err := s.openOrCreateDirectory(int(s.control.Fd()), "retired", 0700)
	if err != nil {
		return err
	}
	defer unix.Close(retired)
	sourceParent, sourceName, closeParent, err := s.openRoleParent(target)
	if err != nil {
		return err
	}
	if closeParent {
		defer unix.Close(sourceParent)
	}
	source, sourceErr := openIdentityDirectory(sourceParent, sourceName, intent)
	if target.Role == volume.RoleRetired {
		if sourceErr == nil {
			return unix.Close(source)
		}
		if errors.Is(sourceErr, unix.ENOENT) {
			return nil
		}
		return sourceErr
	}
	destination, destinationErr := openIdentityDirectory(retired, target.CopyID, intent)
	if sourceErr == nil {
		unix.Close(source)
		if destinationErr == nil {
			unix.Close(destination)
			return ErrIdentity
		}
		if !errors.Is(destinationErr, unix.ENOENT) {
			return destinationErr
		}
		if err := renameDirectory(sourceParent, sourceName, retired, target.CopyID); err != nil {
			return err
		}
		return errors.Join(unix.Fsync(sourceParent), unix.Fsync(retired), s.control.Sync())
	}
	if !errors.Is(sourceErr, unix.ENOENT) {
		return sourceErr
	}
	if errors.Is(destinationErr, unix.ENOENT) {
		return nil
	}
	if destinationErr != nil {
		return destinationErr
	}
	return unix.Close(destination)
}

func (s *Store) verifyCurrentPath(record placement) error {
	parent, name, closeParent, err := s.openRoleParent(record.Identity)
	if err != nil {
		return err
	}
	if closeParent {
		defer unix.Close(parent)
	}
	return s.verifyPath(parent, name, record, record.Identity)
}

func (s *Store) openRoleParent(identity volume.CopyIdentity) (int, string, bool, error) {
	switch identity.Role {
	case volume.RoleServing:
		parent, err := unix.Openat(int(s.root.Fd()), "volumes", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		return parent, identity.VolumeID, true, err
	case volume.RoleIncoming:
		parent, err := unix.Openat(int(s.control.Fd()), "incoming", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		return parent, identity.CopyID, true, err
	case volume.RoleRetired:
		parent, err := unix.Openat(int(s.control.Fd()), "retired", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		return parent, identity.CopyID, true, err
	default:
		return -1, "", false, ErrIdentity
	}
}

func (s *Store) verifyAbsent(target volume.CopyIdentity) error {
	parentRoot, directory, name := int(s.control.Fd()), "", target.CopyID
	switch target.Role {
	case volume.RoleServing:
		parentRoot, directory, name = int(s.root.Fd()), "volumes", target.VolumeID
	case volume.RoleIncoming:
		directory = "incoming"
	case volume.RoleRetired:
		directory = "retired"
	default:
		return ErrIdentity
	}
	parent, err := unix.Openat(parentRoot, directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err == nil {
		unix.Close(fd)
		return fmt.Errorf("%w: cleanup target still exists", ErrNeedsReview)
	}
	if !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

func (s *Store) readMarker(name string, destination any) error {
	file, err := s.openControl(name, unix.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil || len(data) > 8192 {
		return ErrIdentity
	}
	envelope := struct {
		Version int `json:"version"`
		Value   any `json:"value"`
	}{Value: destination}
	if json.Unmarshal(data, &envelope) != nil || envelope.Version != 1 {
		return ErrIdentity
	}
	return s.checkMarker(name, destination)
}

func openIdentityDirectory(parent int, name string, expected localIntent) (int, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		unix.Close(fd)
		return -1, err
	}
	if uint64(stat.Dev) != expected.Device || uint64(stat.Ino) != expected.Inode {
		unix.Close(fd)
		return -1, ErrIdentity
	}
	return fd, nil
}

func receiptDigest(receipt Receipt) (string, error) {
	data, err := canonical(receipt)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
