//go:build linux || darwin

package ownership

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/cagojeiger/ShiftPV/src/volume"
)

type Observation struct {
	Marker   string
	Identity *volume.CopyIdentity
	Present  bool
	Problem  string
}

type Inventory struct {
	store *Store
	dir   *os.File
}

func (s *Store) Inventory() (*Inventory, error) {
	if err := s.checkMarker("pool.json", s.pool); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(s.placements.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return &Inventory{store: s, dir: os.NewFile(uintptr(fd), "shiftpv-inventory")}, nil
}

func (i *Inventory) Close() error { return i.dir.Close() }

func (i *Inventory) Page(ctx context.Context, limit int) ([]Observation, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if limit < 1 || limit > 64 {
		return nil, false, fmt.Errorf("inventory page limit must be 1..64")
	}
	if err := i.store.checkMarker("pool.json", i.store.pool); err != nil {
		return nil, false, err
	}
	entries, err := i.dir.ReadDir(limit)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	result := make([]Observation, 0, len(entries))
	for _, entry := range entries {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, false, ctxErr
		}
		if !strings.HasPrefix(entry.Name(), "placement-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		observation := Observation{Marker: entry.Name()}
		record, readErr := i.readPlacement(entry.Name())
		if readErr != nil {
			observation.Problem = readErr.Error()
		} else {
			observation.Identity = &record.Identity
			observation.Present, readErr = i.pathPresent(record)
			if readErr != nil {
				observation.Identity = nil
				observation.Problem = readErr.Error()
			}
		}
		result = append(result, observation)
	}
	return result, errors.Is(err, io.EOF), nil
}

func (i *Inventory) readPlacement(name string) (placement, error) {
	var envelope struct {
		Version int       `json:"version"`
		Value   placement `json:"value"`
	}
	file, err := i.store.openPlacement(name, unix.O_RDONLY, 0)
	if err != nil {
		return placement{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil || len(data) > 8192 || json.Unmarshal(data, &envelope) != nil || envelope.Version != 1 {
		return placement{}, ErrIdentity
	}
	if envelope.Value.Identity.Validate() != nil || name != placementMarker(envelope.Value.Identity.CopyID) {
		return placement{}, ErrIdentity
	}
	if err := i.store.checkPlacementMarker(envelope.Value.Identity.CopyID, envelope.Value); err != nil {
		return placement{}, err
	}
	return envelope.Value, nil
}

func (i *Inventory) pathPresent(record placement) (bool, error) {
	var parent int
	var directory, name string
	switch record.Identity.Role {
	case volume.RoleServing:
		parent, directory, name = int(i.store.root.Fd()), "volumes", record.Identity.VolumeID
	case volume.RoleIncoming:
		parent, directory, name = int(i.store.control.Fd()), "incoming", record.Identity.CopyID
	case volume.RoleRetired:
		parent, directory, name = int(i.store.control.Fd()), "retired", record.Identity.CopyID
	default:
		return false, ErrIdentity
	}
	directoryFD, err := unix.Openat(parent, directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer unix.Close(directoryFD)
	targetFD, err := unix.Openat(directoryFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer unix.Close(targetFD)
	actual, err := placementFor(targetFD, record.Identity)
	if err != nil || actual != record {
		return false, ErrIdentity
	}
	return true, nil
}
