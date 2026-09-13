package capacity

import (
	"math"
	"strings"
	"testing"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
)

func TestReservedBytesCountsVolumeOwnerCapacity(t *testing.T) {
	volumes := map[string]volumeapi.State{
		"v": {OwnerNode: "a", CapacityBytes: 64},
	}
	got, err := ReservedBytes(volumes, nil, "a")
	if err != nil || got != 64 {
		t.Fatalf("reserved=%d err=%v", got, err)
	}
}

func TestReservedBytesCountsActivePrecommitDestinationHold(t *testing.T) {
	volumes := map[string]volumeapi.State{
		"v": {OwnerNode: "a", ActiveMove: "m", CapacityBytes: 64},
	}
	moves := []volumeapi.Move{{
		Name: "m", Spec: volumeapi.MoveSpec{VolumeID: "v", SourceNode: "a"},
		Status: volumeapi.MoveStatus{DestinationNode: "b", CapacityApproved: true},
	}}
	got, err := ReservedBytes(volumes, moves, "b")
	if err != nil || got != 64 {
		t.Fatalf("reserved=%d err=%v", got, err)
	}
}

func TestReservedBytesCountsPostcommitRetainedSourceHold(t *testing.T) {
	volumes := map[string]volumeapi.State{
		"v": {OwnerNode: "b", ActiveMove: "m", CapacityBytes: 64},
	}
	moves := []volumeapi.Move{{
		Name: "m", Spec: volumeapi.MoveSpec{VolumeID: "v", SourceNode: "a"},
		Status: volumeapi.MoveStatus{DestinationNode: "b", CapacityApproved: true},
	}}
	got, err := ReservedBytes(volumes, moves, "a")
	if err != nil || got != 64 {
		t.Fatalf("reserved=%d err=%v", got, err)
	}
}

func TestReservedBytesSkipsCompletedMoves(t *testing.T) {
	volumes := map[string]volumeapi.State{
		"v": {OwnerNode: "b", CapacityBytes: 64},
	}
	moves := []volumeapi.Move{{
		Name: "m", Spec: volumeapi.MoveSpec{VolumeID: "v", SourceNode: "a"},
		Status: volumeapi.MoveStatus{Phase: "Succeeded", CleanupPhase: "Completed", DestinationNode: "b", CapacityApproved: true},
	}}
	got, err := ReservedBytes(volumes, moves, "a")
	if err != nil || got != 0 {
		t.Fatalf("reserved=%d err=%v", got, err)
	}
}

func TestReservedBytesRetainsSucceededMoveWithoutCleanupProof(t *testing.T) {
	volumes := map[string]volumeapi.State{
		"v": {OwnerNode: "b", ActiveMove: "m", CapacityBytes: 64},
	}
	moves := []volumeapi.Move{{
		Name: "m", Spec: volumeapi.MoveSpec{VolumeID: "v", SourceNode: "a"},
		Status: volumeapi.MoveStatus{Phase: "Succeeded", DestinationNode: "b", CapacityApproved: true},
	}}
	got, err := ReservedBytes(volumes, moves, "a")
	if err != nil || got != 64 {
		t.Fatalf("reserved=%d err=%v", got, err)
	}
}

func TestReservedBytesRejectsContradictoryCapacityState(t *testing.T) {
	for name, test := range map[string]struct {
		volumes map[string]volumeapi.State
		moves   []volumeapi.Move
		want    string
	}{
		"missing owner": {
			volumes: map[string]volumeapi.State{"v": {CapacityBytes: 64}},
			want:    "no current owner",
		},
		"missing capacity": {
			volumes: map[string]volumeapi.State{"v": {OwnerNode: "a"}},
			want:    "invalid capacity",
		},
		"approved move without volume": {
			moves: []volumeapi.Move{{
				Name: "m", Spec: volumeapi.MoveSpec{VolumeID: "v"},
				Status: volumeapi.MoveStatus{DestinationNode: "b", CapacityApproved: true},
			}},
			want: "no volume state",
		},
		"approved inactive move": {
			volumes: map[string]volumeapi.State{"v": {OwnerNode: "a", CapacityBytes: 64}},
			moves: []volumeapi.Move{{
				Name: "m", Spec: volumeapi.MoveSpec{VolumeID: "v"},
				Status: volumeapi.MoveStatus{DestinationNode: "b", CapacityApproved: true},
			}},
			want: "not the active Move",
		},
		"overflow": {
			volumes: map[string]volumeapi.State{
				"v": {OwnerNode: "a", CapacityBytes: math.MaxInt64},
				"w": {OwnerNode: "a", CapacityBytes: 1},
			},
			want: "overflows",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ReservedBytes(test.volumes, test.moves, "a")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v, want %q", err, test.want)
			}
		})
	}
}
