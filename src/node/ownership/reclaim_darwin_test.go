//go:build darwin

package ownership

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDarwinReclaimRetiresButPreservesWhenMountBoundaryProofIsUnavailable(t *testing.T) {
	root := t.TempDir()
	identity := testIdentity()
	authority := func(context.Context) error { return nil }
	if err := PrepareServing(context.Background(), root, identity, authority); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "volumes", identity.VolumeID, "data"), []byte("preserved"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err := Reclaim(context.Background(), root, identity, "operation-a", authority)
	if err == nil || !strings.Contains(err.Error(), "requires Linux") {
		t.Fatalf("unsupported purge result: %v", err)
	}
	data, readErr := os.ReadFile(filepath.Join(root, ".shiftpv", "retired", identity.CopyID, "data"))
	if readErr != nil || string(data) != "preserved" {
		t.Fatal("unsupported environment lost retired data")
	}
}
