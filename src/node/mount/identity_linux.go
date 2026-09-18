//go:build linux

package mount

import (
	"fmt"
	"path/filepath"

	mountutils "k8s.io/mount-utils"
)

func verifyMountedVolume(mountInfoPath, target, volumeID string) error {
	mounts, err := mountutils.ParseMountInfo(mountInfoPath)
	if err != nil {
		return fmt.Errorf("read mount identity: %w", err)
	}
	target = filepath.Clean(target)
	for i := len(mounts) - 1; i >= 0; i-- {
		if filepath.Clean(mounts[i].MountPoint) != target {
			continue
		}
		root := filepath.Clean(mounts[i].Root)
		if filepath.Base(root) == volumeID && filepath.Base(filepath.Dir(root)) == "volumes" {
			return nil
		}
		return fmt.Errorf("%w: mount root %q", ErrTargetVolumeMismatch, mounts[i].Root)
	}
	return fmt.Errorf("inspect target mount %q: mountinfo entry not found", target)
}
