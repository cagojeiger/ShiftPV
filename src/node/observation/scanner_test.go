//go:build linux || darwin

package observation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/node/ownership"
	"github.com/cagojeiger/ShiftPV/src/volume"
)

type installation struct {
	id  string
	err error
}

func (i installation) InstallationID(context.Context) (string, error) { return i.id, i.err }

type publications struct {
	published bool
	err       error
}

func (p publications) HasPublishedTarget(string, string) (bool, error) { return p.published, p.err }

func TestScannerReportsExactCopiesAndPreservesUnrecordedPaths(t *testing.T) {
	host := t.TempDir()
	root := filepath.Join(host, "pool")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	identity := volume.CopyIdentity{
		InstallationID: "installation", PoolName: "pool-a", PoolUID: "pool-uid",
		VolumeID: "shiftpv-11111111111111111111111111111111", VolumeUID: "volume-uid", CopyID: "copy-id", NodeName: "node-a", Role: volume.RoleServing,
	}
	if err := ownership.PrepareServing(context.Background(), root, identity, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(root, "volumes", "shiftpv-22222222222222222222222222222222")
	if err := os.Mkdir(unknown, 0755); err != nil {
		t.Fatal(err)
	}
	scanner := &Scanner{HostRoot: host, TargetRoot: "/kubelet/pods", Installation: installation{id: identity.InstallationID}, Publications: publications{published: true}, Limit: 256}
	result := scanner.Scan(context.Background(), volumeapi.Pool{Name: identity.PoolName, UID: identity.PoolUID, NodeName: identity.NodeName, MountPath: "/pool"}, time.Unix(1, 0))
	if !result.Valid || result.Truncated || len(result.Copies) != 2 {
		t.Fatalf("inventory=%#v", result)
	}
	var exact, preserved bool
	for _, item := range result.Copies {
		exact = exact || item.Identity != nil && *item.Identity == identity && item.Present && item.Published
		preserved = preserved || item.Identity == nil && item.Present && item.Problem == "UnrecordedPath"
	}
	if !exact || !preserved {
		t.Fatalf("exact=%v preserved=%v inventory=%#v", exact, preserved, result.Copies)
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatal("inventory modified unrecorded data")
	}
}

func TestScannerIsBoundedAndSurfacesIdentityFailure(t *testing.T) {
	host := t.TempDir()
	root := filepath.Join(host, "pool")
	if err := os.MkdirAll(filepath.Join(root, "volumes"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one", "two"} {
		if err := os.Mkdir(filepath.Join(root, "volumes", name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	pool := volumeapi.Pool{Name: "pool-a", UID: "pool-uid", NodeName: "node-a", MountPath: "/pool"}
	result := (&Scanner{HostRoot: host, TargetRoot: "/kubelet/pods", Installation: installation{id: "installation"}, Publications: publications{}, Limit: 1}).Scan(context.Background(), pool, time.Now())
	if !result.Valid || !result.Truncated || len(result.Copies) != 1 {
		t.Fatalf("bounded inventory=%#v", result)
	}
	failed := (&Scanner{HostRoot: host, TargetRoot: "/kubelet/pods", Installation: installation{err: errors.New("api unavailable")}, Publications: publications{}, Limit: 1}).Scan(context.Background(), pool, time.Now())
	if failed.Valid || failed.Message == "" {
		t.Fatalf("identity failure hidden: %#v", failed)
	}
}

func TestScannerDoesNotTruncateAtExactLimit(t *testing.T) {
	host := t.TempDir()
	root := filepath.Join(host, "pool")
	if err := os.MkdirAll(filepath.Join(root, "volumes"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "volumes", "only"), 0755); err != nil {
		t.Fatal(err)
	}
	pool := volumeapi.Pool{Name: "pool-a", UID: "pool-uid", NodeName: "node-a", MountPath: "/pool"}
	result := (&Scanner{HostRoot: host, TargetRoot: "/kubelet/pods", Installation: installation{id: "installation"}, Publications: publications{}, Limit: 1}).Scan(context.Background(), pool, time.Now())
	if !result.Valid || result.Truncated || len(result.Copies) != 1 {
		t.Fatalf("exact-limit inventory=%#v", result)
	}
}

func TestScannerFailsClosedWhenPublicationObservationFails(t *testing.T) {
	host := t.TempDir()
	root := filepath.Join(host, "pool")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	identity := volume.CopyIdentity{
		InstallationID: "installation", PoolName: "pool-a", PoolUID: "pool-uid", VolumeID: "shiftpv-33333333333333333333333333333333",
		VolumeUID: "volume-uid", CopyID: "copy-id", NodeName: "node-a", Role: volume.RoleServing,
	}
	if err := ownership.PrepareServing(context.Background(), root, identity, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	scanner := &Scanner{
		HostRoot: host, TargetRoot: "/kubelet/pods", Installation: installation{id: identity.InstallationID},
		Publications: publications{err: errors.New("mount namespace unavailable")}, Limit: 256,
	}
	result := scanner.Scan(context.Background(), volumeapi.Pool{Name: identity.PoolName, UID: identity.PoolUID, NodeName: identity.NodeName, MountPath: "/pool"}, time.Now())
	if !result.Valid || len(result.Copies) != 1 || result.Copies[0].Problem == "" || result.Copies[0].Published {
		t.Fatalf("publication observation failure was not preserved: %#v", result)
	}
}
