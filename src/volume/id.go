package volume

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
)

const idPrefix = "shiftpv-"

var validID = regexp.MustCompile(`^shiftpv-[a-f0-9]{32}$`)

// IDFromName returns a stable, filesystem-safe CSI volume ID.
func IDFromName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("volume name is required")
	}
	sum := sha256.Sum256([]byte(name))
	return idPrefix + hex.EncodeToString(sum[:16]), nil
}

func ValidateID(id string) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("invalid ShiftPV volume ID %q", id)
	}
	return nil
}
