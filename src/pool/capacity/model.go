package capacity

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Filesystem is a point-in-time statfs snapshot of a registered Pool mount.
type Filesystem struct {
	TotalBytes      int64
	AvailableBytes  int64
	AvailableInodes int64
}

// ParseStatOutput parses "blocks available-blocks block-size available-inodes".
func ParseStatOutput(output string) (Filesystem, error) {
	fields := strings.Fields(output)
	if len(fields) != 4 {
		return Filesystem{}, fmt.Errorf("statfs output must contain four integers")
	}
	values := make([]uint64, len(fields))
	for index, field := range fields {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return Filesystem{}, fmt.Errorf("parse statfs field %d: %w", index+1, err)
		}
		values[index] = value
	}
	total, err := bytes(values[0], values[2])
	if err != nil {
		return Filesystem{}, fmt.Errorf("calculate total bytes: %w", err)
	}
	available, err := bytes(values[1], values[2])
	if err != nil {
		return Filesystem{}, fmt.Errorf("calculate available bytes: %w", err)
	}
	if values[3] > math.MaxInt64 {
		return Filesystem{}, fmt.Errorf("available inode count overflows int64")
	}
	return Filesystem{TotalBytes: total, AvailableBytes: available, AvailableInodes: int64(values[3])}, nil
}

func bytes(blocks, blockSize uint64) (int64, error) {
	if blockSize != 0 && blocks > math.MaxInt64/blockSize {
		return 0, fmt.Errorf("block count and size overflow int64")
	}
	return int64(blocks * blockSize), nil
}
