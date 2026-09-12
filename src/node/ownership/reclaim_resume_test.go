//go:build linux || darwin

package ownership

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReclaimResumesExactJournalAfterRetireBeforeAPIReceipt(t *testing.T) {
	root := t.TempDir()
	identity := testIdentity()
	if err := PrepareServing(context.Background(), root, identity, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	interrupted := errors.New("helper interrupted before receipt")
	_, _, err := reclaimWithState(
		context.Background(), root, identity, "operation-resume", func(context.Context, bool) error { return nil },
		func(*Store) error { return nil },
		func(context.Context, *Store, localIntent) error { return interrupted },
	)
	if !errors.Is(err, interrupted) {
		t.Fatalf("interrupted retire result: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "volumes", identity.VolumeID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("serving path was not retired: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".shiftpv", "retired", identity.CopyID)); err != nil {
		t.Fatalf("retired copy is unavailable for retry: %v", err)
	}
	checks := 0
	receipt, digest, err := reclaimWithState(
		context.Background(), root, identity, "operation-resume",
		func(_ context.Context, effectStarted bool) error {
			checks++
			if !effectStarted {
				return errors.New("invalid inventory must block a fresh effect")
			}
			return nil
		},
		func(*Store) error { return nil },
		func(context.Context, *Store, localIntent) error {
			return os.RemoveAll(filepath.Join(root, ".shiftpv", "retired", identity.CopyID))
		},
	)
	if err != nil || checks != 2 || !receipt.Retired || !receipt.Purged || digest == "" {
		t.Fatalf("journaled cleanup did not resume: receipt=%#v digest=%q checks=%d err=%v", receipt, digest, checks, err)
	}
	if _, err := VerifyReceipt(root, identity, "operation-resume", digest); err != nil {
		t.Fatalf("resumed receipt is invalid: %v", err)
	}
}
