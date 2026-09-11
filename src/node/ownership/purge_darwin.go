//go:build darwin

package ownership

import (
	"context"
	"fmt"
)

func purgeRetired(context.Context, *Store, localIntent) error {
	return fmt.Errorf("purge requires Linux openat2 mount-boundary enforcement")
}
