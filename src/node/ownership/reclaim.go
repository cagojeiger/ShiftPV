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

	"github.com/cagojeiger/ShiftPV/src/volume"
)

// Reclaim retires and purges exactly one API-authorized copy. Retries use the
// same operation ID and local intent, including after rename or response loss.
func Reclaim(ctx context.Context, root string, target volume.CopyIdentity, operationID string, authority func(context.Context) error) (Receipt, string, error) {
	return reclaim(ctx, root, target, operationID, authority, preflightPurge, purgeRetired)
}

func reclaim(
	ctx context.Context,
	root string,
	target volume.CopyIdentity,
	operationID string,
	authority func(context.Context) error,
	preflight func(*Store) error,
	purge func(context.Context, *Store, localIntent) error,
) (Receipt, string, error) {
	if authority == nil || !volume.ValidIdentityToken(operationID) || target.Validate() != nil {
		return Receipt{}, "", ErrIdentity
	}
	if err := authority(ctx); err != nil {
		return Receipt{}, "", err
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
	if err := authority(ctx); err != nil {
		return Receipt{}, "", err
	}
	var completed Receipt
	if err := store.readMarker(operationMarker("receipt", operationID), &completed); err == nil {
		if completed.OperationID != operationID || completed.Target != target || !completed.Retired || !completed.Purged {
			return Receipt{}, "", ErrIdentity
		}
		if err := store.verifyAbsent(target); err != nil {
			return Receipt{}, "", err
		}
		if err := store.removeCopyMetadata(target, completed.Device, completed.Inode); err != nil {
			return Receipt{}, "", fmt.Errorf("finish cleanup metadata: %w", err)
		}
		digest, digestErr := receiptDigest(completed)
		return completed, digest, digestErr
	} else if !errors.Is(err, os.ErrNotExist) {
		return Receipt{}, "", err
	}
	if err := store.checkMarker(copyMarker(target.CopyID), target); err != nil {
		return Receipt{}, "", fmt.Errorf("verify copy identity: %w", err)
	}
	placement, err := store.readPlacement(target.CopyID)
	if err != nil || placement.Identity != target {
		return Receipt{}, "", fmt.Errorf("verify copy placement: %w", errors.Join(err, ErrIdentity))
	}
	intent, err := store.ensureLocalIntent(target, operationID, placement)
	if err != nil {
		return Receipt{}, "", fmt.Errorf("persist local cleanup intent: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Receipt{}, "", err
	}
	if err := authority(ctx); err != nil {
		return Receipt{}, "", fmt.Errorf("recheck cleanup authority before filesystem effect: %w", err)
	}
	if err := preflight(store); err != nil {
		return Receipt{}, "", fmt.Errorf("verify safe purge support: %w", err)
	}
	if err := store.retire(target, intent); err != nil {
		return Receipt{}, "", fmt.Errorf("retire cleanup target: %w", err)
	}
	if err := purge(ctx, store, intent); err != nil {
		return Receipt{}, "", fmt.Errorf("purge retired target: %w", err)
	}
	receipt := Receipt{OperationID: operationID, Target: target, Device: intent.Device, Inode: intent.Inode, Retired: true, Purged: true}
	if err := store.ensureMarker(operationMarker("receipt", operationID), receipt); err != nil {
		return Receipt{}, "", fmt.Errorf("persist local cleanup receipt: %w", err)
	}
	if err := store.verifyAbsent(target); err != nil {
		return Receipt{}, "", fmt.Errorf("verify cleanup absence: %w", err)
	}
	if err := store.removeCopyMetadata(target, receipt.Device, receipt.Inode); err != nil {
		return Receipt{}, "", fmt.Errorf("finish cleanup metadata: %w", err)
	}
	digest, err := receiptDigest(receipt)
	return receipt, digest, err
}

func VerifyReceipt(root string, target volume.CopyIdentity, operationID, digest string) (Receipt, error) {
	if !volume.ValidIdentityToken(operationID) || target.Validate() != nil {
		return Receipt{}, ErrIdentity
	}
	store, err := OpenExisting(root, PoolIdentity{InstallationID: target.InstallationID, PoolUID: target.PoolUID})
	if err != nil {
		return Receipt{}, err
	}
	defer store.Close()
	var receipt Receipt
	if err := store.readMarker(operationMarker("receipt", operationID), &receipt); err != nil {
		return Receipt{}, err
	}
	if receipt.OperationID != operationID || receipt.Target != target || !receipt.Retired || !receipt.Purged {
		return Receipt{}, ErrIdentity
	}
	want, err := receiptDigest(receipt)
	if err != nil || want != digest {
		return Receipt{}, ErrIdentity
	}
	if err := store.verifyAbsent(target); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
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
