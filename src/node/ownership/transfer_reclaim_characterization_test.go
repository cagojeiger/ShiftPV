//go:build linux || darwin

package ownership

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/project-jelly/ShiftPV/src/volume"
)

// TestPromoteAndReclaimCharacterization pins the observable behaviour of
// PromoteIncoming and reclaimWithState: returned values, the error sentinel
// classes and message, and the resulting on-disk control/store state for every
// reachable branch. The goldens were captured before the two functions were
// split into steps and must keep matching afterwards.
//
// The default run only asserts against the recorded goldens. Setting
// UPDATE_CHARACTERIZATION rewrites them from current behaviour and always fails,
// because a recording run proves nothing.
func TestPromoteAndReclaimCharacterization(t *testing.T) {
	// The goldens record directory permissions, so the process umask has to be
	// the same everywhere the suite runs. No test in this package is parallel.
	previousUmask := unix.Umask(0022)
	t.Cleanup(func() { unix.Umask(previousUmask) })
	recording := os.Getenv("UPDATE_CHARACTERIZATION") != ""
	recorded := make(map[string]string)
	for _, test := range characterizationCases() {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			observed := test.run(t, root) + "\n" + snapshotTree(t, root)
			if recording {
				recorded[test.name] = observed
				return
			}
			golden, known := characterizationGolden[test.name]
			if !known {
				t.Fatalf("no golden recorded for %q; rerun with UPDATE_CHARACTERIZATION=1 to record it", test.name)
			}
			if observed != golden {
				t.Fatalf("behaviour changed for %q\n--- want ---\n%s\n--- got ---\n%s", test.name, golden, observed)
			}
		})
	}
	if recording {
		if err := writeCharacterizationGolden(recorded); err != nil {
			t.Fatalf("rewrite %s: %v", characterizationGoldenFile, err)
		}
		// A recording run asserts nothing, so it must never be mistaken for a
		// passing verification.
		t.Fatalf("UPDATE_CHARACTERIZATION was set: rewrote %d goldens in %s from the CURRENT behaviour without verifying anything; unset it and rerun to verify",
			len(recorded), characterizationGoldenFile)
	}
}

const characterizationGoldenFile = "transfer_reclaim_characterization_golden_test.go"

// writeCharacterizationGolden regenerates the golden table from a recording
// run. Tests run with the package directory as the working directory.
func writeCharacterizationGolden(recorded map[string]string) error {
	names := make([]string, 0, len(recorded))
	for name := range recorded {
		names = append(names, name)
	}
	sort.Strings(names)
	var builder strings.Builder
	builder.WriteString("//go:build linux || darwin\n\npackage ownership\n\n")
	builder.WriteString("// characterizationGolden holds the observable behaviour recorded before\n")
	builder.WriteString("// PromoteIncoming and reclaimWithState were split into steps: the returned\n")
	builder.WriteString("// values, the exact error class and message, and the resulting on-disk state.\n")
	builder.WriteString("// Regenerate with UPDATE_CHARACTERIZATION=1 go test ./src/node/ownership/.\n")
	builder.WriteString("var characterizationGolden = map[string]string{\n")
	for _, name := range names {
		if strings.Contains(recorded[name], "`") {
			return fmt.Errorf("golden for %q contains a backtick and cannot be written as a raw string", name)
		}
		fmt.Fprintf(&builder, "\t%q: `%s`,\n", name, recorded[name])
	}
	builder.WriteString("}\n")
	return os.WriteFile(characterizationGoldenFile, []byte(builder.String()), 0600)
}

type characterizationCase struct {
	name string
	run  func(t *testing.T, root string) string
}

func characterizationCases() []characterizationCase {
	return append(promoteCharacterizationCases(), reclaimCharacterizationCases()...)
}

func promoteCharacterizationCases() []characterizationCase {
	return []characterizationCase{
		{name: "promote/nil-authority", run: func(_ *testing.T, root string) string {
			incoming, serving := transferIdentities()
			return promoteResult(PromoteIncoming(context.Background(), root, incoming, serving, "promote-operation", nil))
		}},
		{name: "promote/invalid-operation-id", run: func(_ *testing.T, root string) string {
			incoming, serving := transferIdentities()
			return promoteResult(PromoteIncoming(context.Background(), root, incoming, serving, "", okAuthority))
		}},
		{name: "promote/invalid-incoming-identity", run: func(_ *testing.T, root string) string {
			incoming, serving := transferIdentities()
			incoming.NodeName = ""
			return promoteResult(PromoteIncoming(context.Background(), root, incoming, serving, "promote-operation", okAuthority))
		}},
		{name: "promote/wrong-serving-role", run: func(_ *testing.T, root string) string {
			incoming, serving := transferIdentities()
			serving.Role = volume.RoleIncoming
			return promoteResult(PromoteIncoming(context.Background(), root, incoming, serving, "promote-operation", okAuthority))
		}},
		{name: "promote/same-copy-id", run: func(_ *testing.T, root string) string {
			incoming, serving := transferIdentities()
			serving.CopyID = incoming.CopyID
			return promoteResult(PromoteIncoming(context.Background(), root, incoming, serving, "promote-operation", okAuthority))
		}},
		{name: "promote/mismatched-volume-uid", run: func(_ *testing.T, root string) string {
			incoming, serving := transferIdentities()
			serving.VolumeUID = "replacement"
			return promoteResult(PromoteIncoming(context.Background(), root, incoming, serving, "promote-operation", okAuthority))
		}},
		{name: "promote/authority-denied-before-open", run: func(_ *testing.T, root string) string {
			incoming, serving := transferIdentities()
			return promoteResult(PromoteIncoming(context.Background(), root, incoming, serving, "promote-operation", denyAuthorityAt(1)))
		}},
		{name: "promote/missing-pool", run: func(_ *testing.T, root string) string {
			incoming, serving := transferIdentities()
			return promoteResult(PromoteIncoming(context.Background(), root, incoming, serving, "promote-operation", okAuthority))
		}},
		{name: "promote/missing-incoming-copy-marker", run: func(t *testing.T, root string) string {
			incoming, serving := transferIdentities()
			mustPrepareServing(t, root, serving)
			return promoteResult(PromoteIncoming(context.Background(), root, incoming, serving, "promote-operation", okAuthority))
		}},
		{name: "promote/success", run: func(t *testing.T, root string) string {
			incoming, serving := transferIdentities()
			mustPopulate(t, root, incoming, "preserve")
			return promoteResult(PromoteIncoming(context.Background(), root, incoming, serving, "promote-operation", okAuthority))
		}},
		{name: "promote/replay-after-success", run: func(t *testing.T, root string) string {
			incoming, serving := transferIdentities()
			mustPopulate(t, root, incoming, "preserve")
			first := PromoteIncoming(context.Background(), root, incoming, serving, "promote-operation", okAuthority)
			second := PromoteIncoming(context.Background(), root, incoming, serving, "promote-operation", okAuthority)
			return "first=" + errorText(first) + " " + promoteResult(second)
		}},
		{name: "promote/receipt-identity-mismatch", run: func(t *testing.T, root string) string {
			incoming, serving := transferIdentities()
			mustPopulate(t, root, incoming, "preserve")
			if err := PromoteIncoming(context.Background(), root, incoming, serving, "promote-operation", okAuthority); err != nil {
				t.Fatal(err)
			}
			other := serving
			other.CopyID = "other-serving-copy"
			return promoteResult(PromoteIncoming(context.Background(), root, incoming, other, "promote-operation", okAuthority))
		}},
		{name: "promote/target-already-occupied", run: func(t *testing.T, root string) string {
			incoming, serving := transferIdentities()
			mustPopulate(t, root, incoming, "preserve")
			mustPrepareServing(t, root, serving)
			return promoteResult(PromoteIncoming(context.Background(), root, incoming, serving, "promote-operation", okAuthority))
		}},
		{name: "promote/resume-after-rename", run: func(t *testing.T, root string) string {
			incoming, serving := transferIdentities()
			mustPopulate(t, root, incoming, "preserve")
			renameIncomingBehindTheBack(t, root, incoming, serving, "promote-operation")
			return promoteResult(PromoteIncoming(context.Background(), root, incoming, serving, "promote-operation", okAuthority))
		}},
		{name: "promote/authority-denied-before-receipt", run: func(t *testing.T, root string) string {
			incoming, serving := transferIdentities()
			mustPopulate(t, root, incoming, "preserve")
			return promoteResult(PromoteIncoming(context.Background(), root, incoming, serving, "promote-operation", denyAuthorityAt(3)))
		}},
		{name: "promote/authority-denied-after-receipt", run: func(t *testing.T, root string) string {
			incoming, serving := transferIdentities()
			mustPopulate(t, root, incoming, "preserve")
			return promoteResult(PromoteIncoming(context.Background(), root, incoming, serving, "promote-operation", denyAuthorityAt(4)))
		}},
	}
}

func reclaimCharacterizationCases() []characterizationCase {
	return []characterizationCase{
		{name: "reclaim/nil-authority", run: func(_ *testing.T, root string) string {
			return reclaimResult(reclaimWithState(context.Background(), root, testIdentity(), "operation-a", nil, okPreflight, noopPurge))
		}},
		{name: "reclaim/invalid-operation-id", run: func(_ *testing.T, root string) string {
			return reclaimResult(reclaimWithState(context.Background(), root, testIdentity(), "", okResumeAuthority, okPreflight, noopPurge))
		}},
		{name: "reclaim/invalid-target", run: func(_ *testing.T, root string) string {
			target := testIdentity()
			target.Role = "Unknown"
			return reclaimResult(reclaimWithState(context.Background(), root, target, "operation-a", okResumeAuthority, okPreflight, noopPurge))
		}},
		{name: "reclaim/missing-pool", run: func(_ *testing.T, root string) string {
			return reclaimResult(reclaimWithState(context.Background(), root, testIdentity(), "operation-a", okResumeAuthority, okPreflight, noopPurge))
		}},
		{name: "reclaim/missing-copy-marker", run: func(t *testing.T, root string) string {
			target := testIdentity()
			mustPrepareServing(t, root, target)
			if err := os.Remove(filepath.Join(root, ".shiftpv", copyMarker(target.CopyID))); err != nil {
				t.Fatal(err)
			}
			return reclaimResult(reclaimWithState(context.Background(), root, target, "operation-a", okResumeAuthority, okPreflight, purgeRetiredDirectory(root, target)))
		}},
		{name: "reclaim/missing-placement", run: func(t *testing.T, root string) string {
			target := testIdentity()
			mustPrepareServing(t, root, target)
			if err := os.Remove(filepath.Join(root, ".shiftpv", "placements", placementMarker(target.CopyID))); err != nil {
				t.Fatal(err)
			}
			return reclaimResult(reclaimWithState(context.Background(), root, target, "operation-a", okResumeAuthority, okPreflight, purgeRetiredDirectory(root, target)))
		}},
		{name: "reclaim/serving-success", run: func(t *testing.T, root string) string {
			target := testIdentity()
			mustPrepareServing(t, root, target)
			mustWrite(t, filepath.Join(root, "volumes", target.VolumeID, "data"), "remove")
			return reclaimResult(reclaimWithState(context.Background(), root, target, "operation-a", okResumeAuthority, okPreflight, purgeRetiredDirectory(root, target)))
		}},
		{name: "reclaim/replay-after-receipt", run: func(t *testing.T, root string) string {
			target := testIdentity()
			mustPrepareServing(t, root, target)
			first, firstDigest, err := reclaimWithState(context.Background(), root, target, "operation-a", okResumeAuthority, okPreflight, purgeRetiredDirectory(root, target))
			if err != nil {
				t.Fatal(err)
			}
			second, secondDigest, secondErr := reclaimWithState(context.Background(), root, target, "operation-a", okResumeAuthority, okPreflight, noopPurge)
			return fmt.Sprintf("stable=%t %s", first == second && firstDigest == secondDigest, reclaimResult(second, secondDigest, secondErr))
		}},
		{name: "reclaim/receipt-target-mismatch", run: func(t *testing.T, root string) string {
			target := testIdentity()
			mustPrepareServing(t, root, target)
			if _, _, err := reclaimWithState(context.Background(), root, target, "operation-a", okResumeAuthority, okPreflight, purgeRetiredDirectory(root, target)); err != nil {
				t.Fatal(err)
			}
			other := target
			other.VolumeUID = "replacement"
			return reclaimResult(reclaimWithState(context.Background(), root, other, "operation-a", okResumeAuthority, okPreflight, noopPurge))
		}},
		{name: "reclaim/preflight-error", run: func(t *testing.T, root string) string {
			target := testIdentity()
			mustPrepareServing(t, root, target)
			mustWrite(t, filepath.Join(root, "volumes", target.VolumeID, "data"), "preserve")
			return reclaimResult(reclaimWithState(context.Background(), root, target, "operation-a", okResumeAuthority,
				func(*Store) error { return unix.ENOSYS }, noopPurge))
		}},
		{name: "reclaim/purge-error", run: func(t *testing.T, root string) string {
			target := testIdentity()
			mustPrepareServing(t, root, target)
			mustWrite(t, filepath.Join(root, "volumes", target.VolumeID, "data"), "remove")
			return reclaimResult(reclaimWithState(context.Background(), root, target, "operation-a", okResumeAuthority, okPreflight,
				func(context.Context, *Store, localIntent) error { return errors.New("purge interrupted") }))
		}},
		{name: "reclaim/authority-denied-first-check", run: func(t *testing.T, root string) string {
			target := testIdentity()
			mustPrepareServing(t, root, target)
			return reclaimResult(reclaimWithState(context.Background(), root, target, "operation-a", denyResumeAuthorityAt(1), okPreflight, noopPurge))
		}},
		{name: "reclaim/authority-denied-before-effect", run: func(t *testing.T, root string) string {
			target := testIdentity()
			mustPrepareServing(t, root, target)
			mustWrite(t, filepath.Join(root, "volumes", target.VolumeID, "data"), "preserve")
			return reclaimResult(reclaimWithState(context.Background(), root, target, "operation-a", denyResumeAuthorityAt(2), okPreflight, noopPurge))
		}},
		{name: "reclaim/resume-after-retire", run: func(t *testing.T, root string) string {
			target := testIdentity()
			mustPrepareServing(t, root, target)
			interrupted := reclaimWithStateError(context.Background(), root, target, "operation-a", okResumeAuthority, okPreflight,
				func(context.Context, *Store, localIntent) error { return errors.New("purge interrupted") })
			started := []bool{}
			receipt, digest, err := reclaimWithState(context.Background(), root, target, "operation-a",
				func(_ context.Context, effectStarted bool) error {
					started = append(started, effectStarted)
					return nil
				},
				okPreflight, purgeRetiredDirectory(root, target))
			return fmt.Sprintf("interrupted=%s effectStarted=%v %s", interrupted, started, reclaimResult(receipt, digest, err))
		}},
		{name: "reclaim/intent-placement-mismatch", run: func(t *testing.T, root string) string {
			target := testIdentity()
			mustPrepareServing(t, root, target)
			writeForeignIntent(t, root, target, "operation-a")
			return reclaimResult(reclaimWithState(context.Background(), root, target, "operation-a", okResumeAuthority, okPreflight, noopPurge))
		}},
		{name: "reclaim/cancelled-context", run: func(t *testing.T, root string) string {
			target := testIdentity()
			mustPrepareServing(t, root, target)
			ctx, cancel := context.WithCancel(context.Background())
			checks := 0
			receipt, digest, err := reclaimWithState(ctx, root, target, "operation-a",
				func(context.Context, bool) error {
					checks++
					if checks == 1 {
						cancel()
					}
					return nil
				}, okPreflight, noopPurge)
			return reclaimResult(receipt, digest, err)
		}},
		{name: "reclaim/incoming-success", run: func(t *testing.T, root string) string {
			incoming, _ := transferIdentities()
			mustPopulate(t, root, incoming, "remove")
			return reclaimResult(reclaimWithState(context.Background(), root, incoming, "operation-a", okResumeAuthority, okPreflight, purgeRetiredDirectory(root, incoming)))
		}},
		{name: "reclaim/retired-success", run: func(t *testing.T, root string) string {
			target := testIdentity()
			target.Role = volume.RoleRetired
			stageRetiredCopy(t, root, target)
			return reclaimResult(reclaimWithState(context.Background(), root, target, "operation-a", okResumeAuthority, okPreflight, purgeRetiredDirectory(root, target)))
		}},
	}
}

func okAuthority(context.Context) error { return nil }

func okResumeAuthority(context.Context, bool) error { return nil }

func okPreflight(*Store) error { return nil }

func noopPurge(context.Context, *Store, localIntent) error { return nil }

func denyAuthorityAt(call int) func(context.Context) error {
	checks := 0
	return func(context.Context) error {
		checks++
		if checks == call {
			return errors.New("authority revoked")
		}
		return nil
	}
}

func denyResumeAuthorityAt(call int) func(context.Context, bool) error {
	checks := 0
	return func(context.Context, bool) error {
		checks++
		if checks == call {
			return errors.New("authority revoked")
		}
		return nil
	}
}

func purgeRetiredDirectory(root string, target volume.CopyIdentity) func(context.Context, *Store, localIntent) error {
	return func(context.Context, *Store, localIntent) error {
		return os.RemoveAll(filepath.Join(root, ".shiftpv", "retired", target.CopyID))
	}
}

func reclaimWithStateError(
	ctx context.Context, root string, target volume.CopyIdentity, operationID string,
	authority func(context.Context, bool) error, preflight func(*Store) error,
	purge func(context.Context, *Store, localIntent) error,
) string {
	_, _, err := reclaimWithState(ctx, root, target, operationID, authority, preflight, purge)
	return errorText(err)
}

func mustPrepareServing(t *testing.T, root string, identity volume.CopyIdentity) {
	t.Helper()
	if err := PrepareServing(context.Background(), root, identity, okAuthority); err != nil {
		t.Fatal(err)
	}
}

func mustPopulate(t *testing.T, root string, incoming volume.CopyIdentity, payload string) {
	t.Helper()
	if err := PopulateIncoming(context.Background(), root, incoming, "copy-operation", okAuthority, func(_ context.Context, path string) error {
		return os.WriteFile(filepath.Join(path, "payload"), []byte(payload), 0600)
	}); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// renameIncomingBehindTheBack reproduces a crash between the promotion rename
// and the serving placement receipt.
func renameIncomingBehindTheBack(t *testing.T, root string, incoming, serving volume.CopyIdentity, operationID string) {
	t.Helper()
	store, err := OpenExisting(root, PoolIdentity{InstallationID: incoming.InstallationID, PoolUID: incoming.PoolUID})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	placed, err := store.readPlacement(incoming.CopyID)
	if err != nil {
		t.Fatal(err)
	}
	intent := transferIntent{OperationID: operationID, Incoming: incoming, Destination: serving, Device: placed.Device, Inode: placed.Inode}
	if err := store.ensureMarker(operationMarker("promotion", operationID), intent); err != nil {
		t.Fatal(err)
	}
	if err := store.ensureMarker(copyMarker(serving.CopyID), serving); err != nil {
		t.Fatal(err)
	}
	incomingFD, err := unix.Openat(int(store.control.Fd()), "incoming", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(incomingFD)
	volumesFD, err := store.openOrCreateDirectory(int(store.root.Fd()), "volumes", 0755)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(volumesFD)
	if err := renameDirectory(incomingFD, incoming.CopyID, volumesFD, serving.VolumeID); err != nil {
		t.Fatal(err)
	}
}

// writeForeignIntent journals a cleanup intent that points at a different inode
// than the recorded placement.
func writeForeignIntent(t *testing.T, root string, target volume.CopyIdentity, operationID string) {
	t.Helper()
	store, err := OpenExisting(root, PoolIdentity{InstallationID: target.InstallationID, PoolUID: target.PoolUID})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	placed, err := store.readPlacement(target.CopyID)
	if err != nil {
		t.Fatal(err)
	}
	intent := localIntent{OperationID: operationID, Target: target, Device: placed.Device, Inode: placed.Inode + 1}
	if err := store.ensureMarker(operationMarker("cleanup", operationID), intent); err != nil {
		t.Fatal(err)
	}
}

// stageRetiredCopy builds the node-local state of a copy that already lives in
// the retired directory.
func stageRetiredCopy(t *testing.T, root string, target volume.CopyIdentity) {
	t.Helper()
	store, err := Open(root, PoolIdentity{InstallationID: target.InstallationID, PoolUID: target.PoolUID})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	retired, err := store.openOrCreateDirectory(int(store.control.Fd()), "retired", 0700)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(retired)
	if err := unix.Mkdirat(retired, target.CopyID, 0700); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Openat(retired, target.CopyID, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	placed, err := placementFor(fd, target)
	unix.Close(fd)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ensureMarker(copyMarker(target.CopyID), target); err != nil {
		t.Fatal(err)
	}
	if err := store.ensurePlacementMarker(target.CopyID, placed); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, ".shiftpv", "retired", target.CopyID, "data"), "remove")
}

func promoteResult(err error) string {
	return "err=" + errorText(err)
}

func reclaimResult(receipt Receipt, digest string, err error) string {
	return fmt.Sprintf(
		"receipt={operationID=%s copyID=%s role=%s device=%s inode=%s retired=%t purged=%t} digest=%s err=%s",
		receipt.OperationID, receipt.Target.CopyID, receipt.Target.Role,
		presence(receipt.Device != 0), presence(receipt.Inode != 0),
		receipt.Retired, receipt.Purged, presence(digest != ""), errorText(err),
	)
}

func presence(set bool) string {
	if set {
		return "set"
	}
	return "unset"
}

// errorText records the sentinel classes a returned error belongs to before its
// message. Callers branch on the classes, so those are the load-bearing part;
// the message is recorded too but is the only OS-sensitive field.
func errorText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return "[" + strings.Join(errorClasses(err), ",") + "] " + strings.ReplaceAll(err.Error(), "\n", "; ")
}

func errorClasses(err error) []string {
	classes := []string{}
	for _, class := range []struct {
		name     string
		sentinel error
	}{
		{"ErrIdentity", ErrIdentity},
		{"ErrNeedsReview", ErrNeedsReview},
		{"ErrBusy", ErrBusy},
		{"fs.ErrNotExist", os.ErrNotExist},
		{"context.Canceled", context.Canceled},
		{"unix.ENOSYS", unix.ENOSYS},
		{"unix.EEXIST", unix.EEXIST},
	} {
		if errors.Is(err, class.sentinel) {
			classes = append(classes, class.name)
		}
	}
	if len(classes) == 0 {
		classes = append(classes, "unclassified")
	}
	return classes
}

var identityNumbers = regexp.MustCompile(`"(device|inode)":[0-9]+`)

// snapshotTree records every path below root with its permissions, plus the
// content of each file. Marker contents are hashed after device and inode
// numbers are masked, because those vary per run.
func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			lines = append(lines, fmt.Sprintf("d %04o %s", info.Mode().Perm(), relative))
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("f %04o %s %s", info.Mode().Perm(), relative, fileFingerprint(relative, data)))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func fileFingerprint(relative string, data []byte) string {
	if !strings.HasSuffix(relative, ".json") {
		if len(data) == 0 {
			return "<empty>"
		}
		return fmt.Sprintf("%q", string(data))
	}
	masked := identityNumbers.ReplaceAll(data, []byte(`"$1":N`))
	sum := sha256.Sum256(masked)
	return "sha256:" + hex.EncodeToString(sum[:4])
}
