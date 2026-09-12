//go:build linux || darwin

package ownership

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReleaseEmptyPoolAllowsNewPoolIdentity(t *testing.T) {
	root := t.TempDir()
	oldIdentity := PoolIdentity{InstallationID: "installation", PoolUID: "old-pool-uid"}
	store, err := Open(root, oldIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseEmptyPool(context.Background(), root, oldIdentity); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseEmptyPool(context.Background(), root, oldIdentity); err != nil {
		t.Fatalf("idempotent release: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".shiftpv", "pool.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Pool identity remains: %v", err)
	}
	replacement, err := Open(root, PoolIdentity{InstallationID: oldIdentity.InstallationID, PoolUID: "new-pool-uid"})
	if err != nil {
		t.Fatalf("new Pool identity was not admitted: %v", err)
	}
	replacement.Close()
}

func TestReleaseEmptyPoolPreservesManagedData(t *testing.T) {
	root := t.TempDir()
	identity := testIdentity()
	if err := PrepareServing(context.Background(), root, identity, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	pool := PoolIdentity{InstallationID: identity.InstallationID, PoolUID: identity.PoolUID}
	if err := ReleaseEmptyPool(context.Background(), root, pool); !errors.Is(err, ErrNeedsReview) {
		t.Fatalf("non-empty Pool release error = %v", err)
	}
	if err := VerifyServingPath(root, identity); err != nil {
		t.Fatalf("serving data changed after denied release: %v", err)
	}
}

func TestReleaseEmptyPoolPreservesUnrecordedManagedPath(t *testing.T) {
	root := t.TempDir()
	pool := PoolIdentity{InstallationID: "installation", PoolUID: "pool-uid"}
	store, err := Open(root, pool)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	path := filepath.Join(root, ".shiftpv", "incoming", "unrecorded")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseEmptyPool(context.Background(), root, pool); !errors.Is(err, ErrNeedsReview) {
		t.Fatalf("unrecorded path release error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unrecorded path changed: %v", err)
	}
}
