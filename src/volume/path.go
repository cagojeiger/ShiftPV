package volume

import (
	"fmt"
	"path/filepath"
)

func Path(poolRoot, id string) (string, error) {
	if !filepath.IsAbs(poolRoot) {
		return "", fmt.Errorf("pool root must be absolute")
	}
	if err := ValidateID(id); err != nil {
		return "", err
	}
	return filepath.Join(filepath.Clean(poolRoot), "volumes", id), nil
}
