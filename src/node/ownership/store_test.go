//go:build linux || darwin

package ownership

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cagojeiger/ShiftPV/src/volume"
)

func TestPrepareServingPersistsIdentityAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	identity := testIdentity()
	checks := 0
	authority := func(context.Context) error { checks++; return nil }
	for range 2 {
		if err := PrepareServing(context.Background(), root, identity, authority); err != nil {
			t.Fatal(err)
		}
	}
	if checks != 6 {
		t.Fatalf("authority checks=%d", checks)
	}
	store, err := OpenExisting(root, PoolIdentity{InstallationID: identity.InstallationID, PoolUID: identity.PoolUID})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.VerifyServing(identity); err != nil {
		t.Fatal(err)
	}
	changed := identity
	changed.VolumeUID = "replacement"
	if err := store.VerifyServing(changed); !errors.Is(err, ErrIdentity) {
		t.Fatalf("replacement identity accepted: %v", err)
	}
}

func TestPrepareServingDoesNotAdoptUnrecordedDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "volumes", testIdentity().VolumeID), 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, "volumes", testIdentity().VolumeID, "data")
	if err := os.WriteFile(sentinel, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	err := PrepareServing(context.Background(), root, testIdentity(), func(context.Context) error { return nil })
	if !errors.Is(err, ErrNeedsReview) {
		t.Fatalf("unrecorded directory accepted: %v", err)
	}
	data, readErr := os.ReadFile(sentinel)
	if readErr != nil || string(data) != "preserve" {
		t.Fatal("unrecorded data changed")
	}
}

func TestPrepareServingRecoversCrashBetweenStageCreationAndPlacement(t *testing.T) {
	root := t.TempDir()
	identity := testIdentity()
	store, err := Open(root, PoolIdentity{InstallationID: identity.InstallationID, PoolUID: identity.PoolUID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ensureMarker(copyMarker(identity.CopyID), identity); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".shiftpv", "incoming", "create-"+identity.CopyID), 0700); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := PrepareServing(context.Background(), root, identity, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("staged create did not recover: %v", err)
	}
	if err := VerifyServingPath(root, identity); err != nil {
		t.Fatalf("recovered serving placement is invalid: %v", err)
	}
}

func TestPrepareServingDoesNotAdoptUnrecordedStage(t *testing.T) {
	root := t.TempDir()
	identity := testIdentity()
	store, err := Open(root, PoolIdentity{InstallationID: identity.InstallationID, PoolUID: identity.PoolUID})
	if err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(root, ".shiftpv", "incoming", "create-"+identity.CopyID)
	if err := os.MkdirAll(stage, 0700); err != nil {
		store.Close()
		t.Fatal(err)
	}
	sentinel := filepath.Join(stage, "data")
	if err := os.WriteFile(sentinel, []byte("preserve"), 0600); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := PrepareServing(context.Background(), root, identity, func(context.Context) error { return nil }); !errors.Is(err, ErrNeedsReview) {
			t.Fatalf("unrecorded stage accepted on retry: %v", err)
		}
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "preserve" {
		t.Fatal("unrecorded stage changed")
	}
	if _, err := os.Stat(filepath.Join(root, ".shiftpv", copyMarker(identity.CopyID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unrecorded stage gained copy authority: %v", err)
	}
}

func TestVolumeLockIsSharedAndPoolReplacementIsRejected(t *testing.T) {
	root := t.TempDir()
	pool := PoolIdentity{InstallationID: "installation", PoolUID: "pool-uid"}
	first, err := Open(root, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenExisting(root, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	lock, err := first.Acquire(context.Background(), testIdentity().VolumeID)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if other, err := second.Acquire(context.Background(), testIdentity().VolumeID); !errors.Is(err, ErrBusy) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("concurrent operation admitted: %v", err)
	}
	if replaced, err := OpenExisting(root, PoolIdentity{InstallationID: "new-installation", PoolUID: pool.PoolUID}); !errors.Is(err, ErrIdentity) {
		if replaced != nil {
			replaced.Close()
		}
		t.Fatalf("replacement installation adopted Pool: %v", err)
	}
}

func TestWithLockAndReceiptVerificationUseExactIdentity(t *testing.T) {
	root := t.TempDir()
	identity := testIdentity()
	authority := func(context.Context) error { return nil }
	if err := PrepareServing(context.Background(), root, identity, authority); err != nil {
		t.Fatal(err)
	}
	if err := VerifyServingPath(root, identity); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := WithLock(context.Background(), root, PoolIdentity{InstallationID: identity.InstallationID, PoolUID: identity.PoolUID}, identity.VolumeID, func(store *Store) error {
		called = true
		return store.VerifyServing(identity)
	}); err != nil || !called {
		t.Fatalf("locked verification called=%v err=%v", called, err)
	}
	if err := WithLock(context.Background(), root, PoolIdentity{InstallationID: identity.InstallationID, PoolUID: identity.PoolUID}, identity.VolumeID, nil); !errors.Is(err, ErrIdentity) {
		t.Fatalf("nil locked operation accepted: %v", err)
	}
	readOnlyCalled := false
	if err := WithExistingLock(context.Background(), root, PoolIdentity{InstallationID: identity.InstallationID, PoolUID: identity.PoolUID}, identity.VolumeID, func(store *Store) error {
		readOnlyCalled = true
		return store.VerifyServing(identity)
	}); err != nil || !readOnlyCalled {
		t.Fatalf("read-only locked verification called=%v err=%v", readOnlyCalled, err)
	}
	if err := WithExistingLock(context.Background(), root, PoolIdentity{InstallationID: identity.InstallationID, PoolUID: identity.PoolUID}, "shiftpv-ffffffffffffffffffffffffffffffff", func(*Store) error { return nil }); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only verification created a missing lock: %v", err)
	}

	store, err := OpenExisting(root, PoolIdentity{InstallationID: identity.InstallationID, PoolUID: identity.PoolUID})
	if err != nil {
		t.Fatal(err)
	}
	placed, err := store.readPlacement(identity.CopyID)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "volumes", identity.VolumeID)); err != nil {
		store.Close()
		t.Fatal(err)
	}
	receipt := Receipt{OperationID: "operation-receipt", Target: identity, Device: placed.Device, Inode: placed.Inode, Retired: true, Purged: true}
	if err := store.ensureMarker(operationMarker("receipt", receipt.OperationID), receipt); err != nil {
		store.Close()
		t.Fatal(err)
	}
	digest, err := receiptDigest(receipt)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyReceipt(root, identity, receipt.OperationID, digest)
	if err != nil || verified != receipt {
		t.Fatalf("verified receipt=%#v err=%v", verified, err)
	}
	if _, err := VerifyReceipt(root, identity, receipt.OperationID, "changed"); !errors.Is(err, ErrIdentity) {
		t.Fatalf("changed receipt digest accepted: %v", err)
	}
	if _, err := VerifyReceipt(root, identity, "invalid/operation", digest); !errors.Is(err, ErrIdentity) {
		t.Fatalf("invalid receipt identity accepted: %v", err)
	}
}

func TestInventoryIsBoundedAndPreservesMalformedMarkers(t *testing.T) {
	root := t.TempDir()
	if err := PrepareServing(context.Background(), root, testIdentity(), func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(root, ".shiftpv", "placements", "placement-bad.json")
	if err := os.WriteFile(bad, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenExisting(root, PoolIdentity{InstallationID: "installation", PoolUID: "pool-uid"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	inventory, err := store.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	defer inventory.Close()
	var valid, malformed int
	done := false
	for !done {
		observations, nextDone, pageErr := inventory.Page(context.Background(), 1)
		if pageErr != nil {
			t.Fatal(pageErr)
		}
		done = nextDone
		for _, observation := range observations {
			if observation.Identity != nil && observation.Present {
				valid++
			} else if observation.Problem != "" {
				malformed++
			}
		}
	}
	if valid != 1 || malformed != 1 {
		t.Fatalf("valid=%d malformed=%d", valid, malformed)
	}
	if _, _, err := inventory.Page(context.Background(), 65); err == nil {
		t.Fatal("unbounded page accepted")
	}
	if data, err := os.ReadFile(bad); err != nil || string(data) != "bad" {
		t.Fatal("malformed marker changed")
	}
}

func testIdentity() volume.CopyIdentity {
	return volume.CopyIdentity{
		InstallationID: "installation", PoolName: "pool-a", PoolUID: "pool-uid",
		VolumeID: "shiftpv-11111111111111111111111111111111", VolumeUID: "volume-uid",
		CopyID: "copy-id", NodeName: "node-a", Role: volume.RoleServing,
	}
}
