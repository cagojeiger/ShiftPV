//go:build !linux

package mount

import "fmt"

func verifyMountedVolume(_, target, _ string) error {
	return fmt.Errorf("verify mounted volume %q: unsupported platform", target)
}
