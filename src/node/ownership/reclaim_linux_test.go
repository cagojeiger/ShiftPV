//go:build linux

package ownership

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReclaimIsIdentityBoundIdempotentAndDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	identity := testIdentity()
	authority := func(context.Context) error { return nil }
	if err := PrepareServing(context.Background(), root, identity, authority); err != nil {
		t.Fatal(err)
	}
	dataset := filepath.Join(root, "volumes", identity.VolumeID)
	if err := os.WriteFile(filepath.Join(dataset, "data"), []byte("remove"), 0600); err != nil {
		t.Fatal(err)
	}
	userData := filepath.Join(root, "operator-data", "preserve")
	foreignVolume := filepath.Join(root, "volumes", "shiftpv-ffffffffffffffffffffffffffffffff", "preserve")
	foreignIncoming := filepath.Join(root, ".shiftpv", "incoming", "unrecorded-copy", "preserve")
	for _, path := range []string{userData, foreignVolume, foreignIncoming} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("preserve"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	external := filepath.Join(t.TempDir(), "preserve")
	if err := os.WriteFile(external, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(dataset, "link")); err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	var digest string
	for range 2 {
		got, gotDigest, err := Reclaim(context.Background(), root, identity, "operation-a", authority)
		if err != nil {
			t.Fatal(err)
		}
		if digest != "" && (got != receipt || gotDigest != digest) {
			t.Fatal("retry changed durable receipt")
		}
		receipt, digest = got, gotDigest
	}
	if _, err := VerifyReceipt(root, identity, "operation-a", digest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dataset); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dataset remains: %v", err)
	}
	for _, marker := range []string{
		filepath.Join(root, ".shiftpv", copyMarker(identity.CopyID)),
		filepath.Join(root, ".shiftpv", "placements", placementMarker(identity.CopyID)),
	} {
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("settled copy marker remains at %s: %v", marker, err)
		}
	}
	data, err := os.ReadFile(external)
	if err != nil || string(data) != "preserve" {
		t.Fatal("symlink target changed")
	}
	for _, path := range []string{userData, foreignVolume, foreignIncoming} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "preserve" {
			t.Fatalf("non-target data changed at %s: data=%q err=%v", path, data, err)
		}
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Fatalf("Pool root changed: info=%v err=%v", info, err)
	}
	changed := identity
	changed.VolumeUID = "replacement"
	if _, _, err := Reclaim(context.Background(), root, changed, "operation-a", authority); !errors.Is(err, ErrIdentity) {
		t.Fatalf("replacement identity accepted: %v", err)
	}
}

func TestReclaimChecksAuthorityBeforeDiskEffect(t *testing.T) {
	root := t.TempDir()
	identity := testIdentity()
	allow := func(context.Context) error { return nil }
	if err := PrepareServing(context.Background(), root, identity, allow); err != nil {
		t.Fatal(err)
	}
	denied := errors.New("authority revoked")
	checks := 0
	_, _, err := Reclaim(context.Background(), root, identity, "operation-a", func(context.Context) error {
		checks++
		if checks == 2 {
			return denied
		}
		return nil
	})
	if !errors.Is(err, denied) {
		t.Fatalf("revoked authority result: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "volumes", identity.VolumeID)); err != nil {
		t.Fatal("revoked operation changed data")
	}
}

func TestReclaimRechecksAuthorityImmediatelyBeforeFilesystemEffect(t *testing.T) {
	root := t.TempDir()
	identity := testIdentity()
	if err := PrepareServing(context.Background(), root, identity, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "volumes", identity.VolumeID, "data"), []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	denied := errors.New("authority appeared")
	checks := 0
	_, _, err := Reclaim(context.Background(), root, identity, "operation-recheck", func(context.Context) error {
		checks++
		if checks == 3 {
			return denied
		}
		return nil
	})
	if !errors.Is(err, denied) {
		t.Fatalf("post-retire authority change was ignored: %v", err)
	}
	serving := filepath.Join(root, "volumes", identity.VolumeID, "data")
	if data, readErr := os.ReadFile(serving); readErr != nil || string(data) != "preserve" {
		t.Fatalf("data changed after authority was revoked: data=%q err=%v", data, readErr)
	}
	if _, _, err := Reclaim(context.Background(), root, identity, "operation-recheck", func(context.Context) error { return nil }); err != nil {
		t.Fatalf("cleanup did not resume after authority became safe: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".shiftpv", "retired", identity.CopyID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resumed cleanup did not purge retired copy: %v", err)
	}
}

func TestReclaimPurgesAnExactIncomingCopy(t *testing.T) {
	root := t.TempDir()
	incoming, _ := transferIdentities()
	authority := func(context.Context) error { return nil }
	if err := PopulateIncoming(context.Background(), root, incoming, "populate-incoming", authority, func(_ context.Context, path string) error {
		return os.WriteFile(filepath.Join(path, "data"), []byte("remove"), 0600)
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Reclaim(context.Background(), root, incoming, "reclaim-incoming", authority); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".shiftpv", "incoming", incoming.CopyID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incoming copy remains: %v", err)
	}
}

func TestReclaimIncomingPreservesServingCopyOfSameVolume(t *testing.T) {
	root := t.TempDir()
	incoming, serving := transferIdentities()
	authority := func(context.Context) error { return nil }
	if err := PopulateIncoming(context.Background(), root, incoming, "populate-incoming", authority, func(_ context.Context, path string) error {
		return os.WriteFile(filepath.Join(path, "staging"), []byte("remove"), 0600)
	}); err != nil {
		t.Fatal(err)
	}
	if err := PrepareServing(context.Background(), root, serving, authority); err != nil {
		t.Fatal(err)
	}
	servingPath := filepath.Join(root, "volumes", serving.VolumeID)
	if err := os.WriteFile(filepath.Join(servingPath, "payload"), []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Reclaim(context.Background(), root, incoming, "reclaim-incoming", authority); err != nil {
		t.Fatalf("incoming cleanup was blocked by the serving copy: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(servingPath, "payload")); err != nil || string(data) != "preserve" {
		t.Fatalf("serving copy changed: data=%q err=%v", data, err)
	}
}
