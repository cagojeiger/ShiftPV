package capacity

import (
	"math"
	"testing"
)

func TestParseStatOutput(t *testing.T) {
	got, err := ParseStatOutput("100 25 4096 12\n")
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalBytes != 409600 || got.AvailableBytes != 102400 || got.AvailableInodes != 12 {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestParseStatOutputRejectsInvalidAndOverflow(t *testing.T) {
	for name, input := range map[string]string{
		"missing field":  "1 2 3",
		"invalid field":  "1 no 3 4",
		"byte overflow":  "9223372036854775807 1 2 4",
		"inode overflow": "1 1 1 9223372036854775808",
		"uint overflow":  "18446744073709551616 1 1 1",
		"max overflow":   "1 1 1 " + "18446744073709551615",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseStatOutput(input); err == nil {
				t.Fatalf("accepted %q", input)
			}
		})
	}
	if _, err := bytes(math.MaxInt64, 1); err != nil {
		t.Fatalf("max int64 should be accepted: %v", err)
	}
}
