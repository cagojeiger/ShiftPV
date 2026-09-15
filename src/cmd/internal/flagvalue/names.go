package flagvalue

import (
	"fmt"
	"slices"
	"strings"
)

// Names collects a repeatable command-line flag without duplicates.
type Names []string

func (names *Names) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("name must not be empty")
	}
	if !slices.Contains(*names, value) {
		*names = append(*names, value)
	}
	return nil
}

func (names *Names) String() string {
	if names == nil {
		return ""
	}
	return strings.Join(*names, ",")
}

func (names Names) Values(defaults ...string) []string {
	if len(names) == 0 {
		return slices.Clone(defaults)
	}
	return slices.Clone(names)
}
