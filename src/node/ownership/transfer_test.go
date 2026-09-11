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

func TestPopulateAndPromoteIncomingConvergesAfterRetries(t *testing.T) {
	root := t.TempDir()
	incoming, serving := transferIdentities()
	checks := 0
	authority := func(context.Context) error { checks++; return nil }
	populateCalls := 0
	populate := func(_ context.Context, path string) error {
		populateCalls++
		return os.WriteFile(filepath.Join(path, "payload"), []byte("preserve"), 0600)
	}
	for range 2 {
		if err := PopulateIncoming(context.Background(), root, incoming, "copy-operation", authority, populate); err != nil {
			t.Fatal(err)
		}
	}
	if populateCalls != 1 {
		t.Fatalf("populate calls=%d", populateCalls)
	}
	for range 2 {
		if err := PromoteIncoming(context.Background(), root, incoming, serving, "promote-operation", authority); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(root, "volumes", serving.VolumeID, "payload"))
	if err != nil || string(data) != "preserve" {
		t.Fatalf("promoted payload=%q err=%v", data, err)
	}
	store, err := OpenExisting(root, PoolIdentity{InstallationID: serving.InstallationID, PoolUID: serving.PoolUID})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.VerifyServing(serving); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		filepath.Join(root, ".shiftpv", copyMarker(incoming.CopyID)),
		filepath.Join(root, ".shiftpv", "placements", placementMarker(incoming.CopyID)),
	} {
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("superseded incoming marker remains at %s: %v", marker, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".shiftpv", "placements", placementMarker(serving.CopyID))); err != nil {
		t.Fatalf("serving placement marker missing: %v", err)
	}
	if checks < 8 {
		t.Fatalf("authority checks=%d", checks)
	}
}

func TestTransferRejectsIdentityChangeAndPreservesExistingData(t *testing.T) {
	root := t.TempDir()
	incoming, serving := transferIdentities()
	if err := PopulateIncoming(context.Background(), root, incoming, "copy-operation", func(context.Context) error { return nil }, func(_ context.Context, path string) error {
		return os.WriteFile(filepath.Join(path, "payload"), []byte("preserve"), 0600)
	}); err != nil {
		t.Fatal(err)
	}
	changed := serving
	changed.VolumeUID = "replacement"
	if err := PromoteIncoming(context.Background(), root, incoming, changed, "promote-operation", func(context.Context) error { return nil }); !errors.Is(err, ErrIdentity) {
		t.Fatalf("changed identity accepted: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".shiftpv", "incoming", incoming.CopyID, "payload"))
	if err != nil || string(data) != "preserve" {
		t.Fatal("incoming data changed after rejected promotion")
	}
}

func TestPopulateIncomingRecoversCrashBetweenDirectoryAndPlacement(t *testing.T) {
	root := t.TempDir()
	incoming, _ := transferIdentities()
	store, err := Open(root, PoolIdentity{InstallationID: incoming.InstallationID, PoolUID: incoming.PoolUID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ensureMarker(copyMarker(incoming.CopyID), incoming); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".shiftpv", "incoming", incoming.CopyID), 0700); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := PopulateIncoming(context.Background(), root, incoming, "copy-operation", func(context.Context) error { return nil }, func(_ context.Context, path string) error {
		return os.WriteFile(filepath.Join(path, "payload"), []byte("recovered"), 0600)
	}); err != nil {
		t.Fatalf("incoming stage did not recover: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".shiftpv", "incoming", incoming.CopyID, "payload"))
	if err != nil || string(data) != "recovered" {
		t.Fatalf("recovered incoming payload=%q err=%v", data, err)
	}
}

func TestPopulateIncomingDoesNotAdoptUnrecordedDirectory(t *testing.T) {
	root := t.TempDir()
	incoming, _ := transferIdentities()
	store, err := Open(root, PoolIdentity{InstallationID: incoming.InstallationID, PoolUID: incoming.PoolUID})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".shiftpv", "incoming", incoming.CopyID)
	if err := os.MkdirAll(path, 0700); err != nil {
		store.Close()
		t.Fatal(err)
	}
	sentinel := filepath.Join(path, "data")
	if err := os.WriteFile(sentinel, []byte("preserve"), 0600); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	populateCalled := false
	for range 2 {
		err = PopulateIncoming(context.Background(), root, incoming, "copy-operation", func(context.Context) error { return nil }, func(context.Context, string) error {
			populateCalled = true
			return nil
		})
		if !errors.Is(err, ErrNeedsReview) || populateCalled {
			t.Fatalf("unrecorded incoming accepted on retry: err=%v populateCalled=%t", err, populateCalled)
		}
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "preserve" {
		t.Fatal("unrecorded incoming changed")
	}
	if _, err := os.Stat(filepath.Join(root, ".shiftpv", copyMarker(incoming.CopyID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unrecorded incoming gained copy authority: %v", err)
	}
}

func transferIdentities() (volume.CopyIdentity, volume.CopyIdentity) {
	incoming := testIdentity()
	incoming.CopyID = "incoming-copy"
	incoming.Role = volume.RoleIncoming
	serving := incoming
	serving.CopyID = "serving-copy"
	serving.Role = volume.RoleServing
	return incoming, serving
}
