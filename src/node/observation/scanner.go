//go:build linux || darwin

package observation

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/node/ownership"
	"github.com/cagojeiger/ShiftPV/src/volume"
)

type Installation interface {
	InstallationID(context.Context) (string, error)
}

type Publications interface {
	HasPublishedTarget(source, targetRoot string) (bool, error)
}

type Scanner struct {
	HostRoot     string
	TargetRoot   string
	Installation Installation
	Publications Publications
	Limit        int
}

func (s *Scanner) ReleasePool(ctx context.Context, pool volumeapi.Pool) error {
	if s == nil || s.Installation == nil || !filepath.IsAbs(s.HostRoot) || pool.UID == "" {
		return ownership.ErrIdentity
	}
	installationID, err := s.Installation.InstallationID(ctx)
	if err != nil {
		return err
	}
	root := filepath.Join(s.HostRoot, strings.TrimPrefix(filepath.Clean(pool.MountPath), string(filepath.Separator)))
	return ownership.ReleaseEmptyPool(ctx, root, ownership.PoolIdentity{InstallationID: installationID, PoolUID: pool.UID})
}

func (s *Scanner) Scan(ctx context.Context, pool volumeapi.Pool, now time.Time) volumeapi.PoolInventory {
	result := volumeapi.PoolInventory{ObservedAt: metav1.NewTime(now.UTC())}
	if s == nil || s.Installation == nil || s.Publications == nil || !filepath.IsAbs(s.HostRoot) || !filepath.IsAbs(s.TargetRoot) || pool.UID == "" || s.Limit < 1 || s.Limit > 256 {
		result.Message = "InventoryConfigurationInvalid"
		return result
	}
	installationID, err := s.Installation.InstallationID(ctx)
	if err != nil {
		result.Message = "InstallationIdentityUnavailable: " + err.Error()
		return result
	}
	root := filepath.Join(s.HostRoot, strings.TrimPrefix(filepath.Clean(pool.MountPath), string(filepath.Separator)))
	known := map[string]struct{}{}
	store, err := ownership.OpenExisting(root, ownership.PoolIdentity{InstallationID: installationID, PoolUID: pool.UID})
	if err == nil {
		defer store.Close()
		inventory, inventoryErr := store.Inventory()
		if inventoryErr != nil {
			result.Message = "InventoryOpenFailed: " + inventoryErr.Error()
			return result
		}
		defer inventory.Close()
		done := false
		for !done && len(result.Copies) < s.Limit {
			page, pageDone, pageErr := inventory.Page(ctx, min(64, s.Limit-len(result.Copies)))
			if pageErr != nil {
				result.Message = "InventoryReadFailed: " + pageErr.Error()
				return result
			}
			for _, item := range page {
				observation := volumeapi.CopyObservation{Marker: item.Marker, Identity: item.Identity, Present: item.Present, Problem: item.Problem}
				if item.Identity != nil && (item.Identity.InstallationID != installationID || item.Identity.PoolName != pool.Name ||
					item.Identity.PoolUID != pool.UID || item.Identity.NodeName != pool.NodeName) {
					observation.Identity = nil
					observation.Problem = "PoolIdentityMismatch"
				}
				if observation.Identity != nil && observation.Present && observation.Problem == "" {
					source := filepath.Join(root, filepath.FromSlash(physicalKey(*observation.Identity)))
					observation.Published, err = s.Publications.HasPublishedTarget(source, s.TargetRoot)
					if err != nil {
						observation.Problem = "PublicationObservationFailed: " + err.Error()
					}
				}
				result.Copies = append(result.Copies, observation)
				if item.Identity != nil && item.Present {
					known[physicalKey(*item.Identity)] = struct{}{}
				}
			}
			done = pageDone
		}
		for !done && len(result.Copies) == s.Limit {
			page, pageDone, pageErr := inventory.Page(ctx, 1)
			if pageErr != nil {
				result.Message = "InventoryReadFailed: " + pageErr.Error()
				return result
			}
			if len(page) > 0 {
				break
			}
			done = pageDone
		}
		result.Truncated = !done
	} else if !errors.Is(err, os.ErrNotExist) {
		result.Message = "PoolIdentityInvalid: " + err.Error()
	}
	unknown, truncated, err := scanPhysical(ctx, root, known, s.Limit-len(result.Copies))
	if err != nil {
		result.Message = "PhysicalInventoryFailed: " + err.Error()
		return result
	}
	result.Copies = append(result.Copies, unknown...)
	result.Truncated = result.Truncated || truncated
	if result.Message == "" {
		for _, observed := range result.Copies {
			if observed.Problem != "" {
				result.Message = "CopyObservationProblem"
				break
			}
		}
	}
	result.Valid = result.Message == ""
	return result
}

func physicalKey(identity volume.CopyIdentity) string {
	switch identity.Role {
	case volume.RoleServing:
		return "volumes/" + identity.VolumeID
	case volume.RoleIncoming:
		return ".shiftpv/incoming/" + identity.CopyID
	case volume.RoleRetired:
		return ".shiftpv/retired/" + identity.CopyID
	default:
		return ""
	}
}

func scanPhysical(ctx context.Context, root string, known map[string]struct{}, limit int) ([]volumeapi.CopyObservation, bool, error) {
	if limit < 0 {
		return nil, false, errors.New("physical inventory limit cannot be negative")
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, false, err
	}
	defer unix.Close(rootFD)
	type directory struct {
		parent     int
		path, name string
	}
	directories := []directory{{rootFD, "volumes", "volumes"}}
	controlFD, controlErr := unix.Openat(rootFD, ".shiftpv", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if controlErr == nil {
		defer unix.Close(controlFD)
		directories = append(directories, directory{controlFD, ".shiftpv/incoming", "incoming"}, directory{controlFD, ".shiftpv/retired", "retired"})
	} else if !errors.Is(controlErr, unix.ENOENT) {
		return nil, false, controlErr
	}
	result := []volumeapi.CopyObservation{}
	for _, candidate := range directories {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		fd, err := unix.Openat(candidate.parent, candidate.name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return nil, false, err
		}
		file := os.NewFile(uintptr(fd), candidate.path)
		for {
			entries, readErr := file.ReadDir(min(64, limit-len(result)+1))
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				file.Close()
				return nil, false, readErr
			}
			for _, entry := range entries {
				key := candidate.path + "/" + entry.Name()
				if _, exists := known[key]; exists {
					continue
				}
				if len(result) == limit {
					file.Close()
					return result, true, nil
				}
				problem := "UnrecordedPath"
				if !entry.IsDir() {
					problem = "UnexpectedPathType"
				}
				result = append(result, volumeapi.CopyObservation{Marker: "path:" + key, Present: true, Problem: problem})
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
		}
		file.Close()
	}
	return result, false, nil
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}
