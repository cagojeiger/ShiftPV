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

func IsWithin(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && rel != "." && !filepath.IsAbs(rel) && len(rel) > 0 && rel[:1] != "."
}
