package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/mobility/fsm"
)

func TestMoveDiagnosticsTrackTransitionsWithoutPollingDrift(t *testing.T) {
	ctx := context.Background()
	first := time.Date(2026, 9, 4, 1, 0, 0, 0, time.UTC)
	second := first.Add(time.Minute)
	now := first
	recorder := record.NewFakeRecorder(10)
	move := volumeapi.Move{Name: "move", UID: "uid", Status: volumeapi.MoveStatus{}}
	repo := &memoryRepository{moves: []volumeapi.Move{move}}
	r := &Reconciler{Repository: repo, Now: func() time.Time { return now }, Recorder: recorder}

	previous := move.Status
	move.Status.Phase = string(fsm.PhasePending)
	move.Status.Reason = "DisruptionBudgetDenied"
	move.Status.Message = mobilityMessage(fsm.PhasePending, move.Status.Reason)
	if err := r.persistMoveStatus(ctx, &move, previous); err != nil {
		t.Fatal(err)
	}
	if move.Status.LastTransitionTime != first.Format(time.RFC3339Nano) || move.Status.LastProgressTime != first.Format(time.RFC3339Nano) {
		t.Fatalf("initial diagnostics=%+v", move.Status)
	}
	assertEventContains(t, recorder, "Normal DisruptionBudgetDenied automatic retry")

	now = second
	previous = move.Status
	if err := r.persistMoveStatus(ctx, &move, previous); err != nil {
		t.Fatal(err)
	}
	assertNoEvent(t, recorder)
	if move.Status.LastProgressTime != first.Format(time.RFC3339Nano) {
		t.Fatalf("polling changed progress time: %+v", move.Status)
	}

	move.Status.Reason = "NoCompatibleDestination"
	move.Status.Message = mobilityMessage(fsm.PhasePending, move.Status.Reason)
	if err := r.persistMoveStatus(ctx, &move, previous); err != nil {
		t.Fatal(err)
	}
	assertEventContains(t, recorder, "Normal NoCompatibleDestination automatic retry")
	if move.Status.LastProgressTime != first.Format(time.RFC3339Nano) || move.Status.LastTransitionTime != first.Format(time.RFC3339Nano) {
		t.Fatalf("diagnostic-only change looked like progress: %+v", move.Status)
	}

	previous = move.Status
	move.Status.Phase = string(fsm.PhaseLocking)
	move.Status.Reason = ""
	move.Status.Message = mobilityMessage(fsm.PhaseLocking, "")
	if err := r.persistMoveStatus(ctx, &move, previous); err != nil {
		t.Fatal(err)
	}
	if move.Status.LastTransitionTime != second.Format(time.RFC3339Nano) || move.Status.LastProgressTime != second.Format(time.RFC3339Nano) {
		t.Fatalf("phase transition did not advance timestamps: %+v", move.Status)
	}
	assertEventContains(t, recorder, "Normal MobilityLocking automatic retry")
}

func TestMoveDiagnosticsRecordAndClearTransientObservationFailure(t *testing.T) {
	ctx := context.Background()
	move := volumeapi.Move{Name: "move", UID: "uid", Spec: volumeapi.MoveSpec{VolumeID: preflightVolume, SourceNode: "source"}, Status: volumeapi.MoveStatus{Phase: string(fsm.PhasePending)}}
	repo := &memoryRepository{
		volumes: map[string]volumeapi.State{preflightVolume: {Phase: volumeapi.PhaseReady, OwnerNode: "source", PublishedNodes: []string{"source"}}},
		pools:   []volumeapi.Pool{{Name: "source", NodeName: "source", MountPath: "/pool"}, {Name: "destination", NodeName: "destination", MountPath: "/pool"}},
		moves:   []volumeapi.Move{move},
	}
	client := fake.NewSimpleClientset(mobilityObjects(preflightVolume)...)
	client.PrependReactor("get", "replicasets", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("API timeout")
	})
	r := &Reconciler{Client: client, Repository: repo, Namespace: "system", HelperImage: "helper", Now: func() time.Time {
		return time.Date(2026, 9, 4, 2, 0, 0, 0, time.UTC)
	}}
	if err := r.ReconcileAll(ctx); err == nil {
		t.Fatal("observation failure was hidden")
	}
	failed := repo.moves[0].Status
	if failed.Phase != string(fsm.PhasePending) || failed.Reason != "ObservationFailed" || !strings.Contains(failed.Message, "API timeout") || failed.LastProgressTime == "" {
		t.Fatalf("failure diagnostic=%+v", failed)
	}

	r.Client = fake.NewSimpleClientset(mobilityObjects(preflightVolume)...)
	r.Now = func() time.Time { return time.Date(2026, 9, 4, 2, 1, 0, 0, time.UTC) }
	if err := r.ReconcileAll(ctx); err != nil {
		t.Fatal(err)
	}
	recovered := repo.moves[0].Status
	if recovered.Phase != string(fsm.PhaseLocking) || recovered.Reason != "" || strings.Contains(recovered.Message, "API timeout") {
		t.Fatalf("stale transient diagnostic=%+v", recovered)
	}
}

func TestMoveDiagnosticsRecordAndClearTransientActionFailure(t *testing.T) {
	ctx := context.Background()
	move := volumeapi.Move{Name: "move", UID: "uid", Spec: volumeapi.MoveSpec{VolumeID: preflightVolume, SourceNode: "source"}, Status: volumeapi.MoveStatus{
		Phase: string(fsm.PhaseEvicting), ConsumerName: "consumer", ConsumerUID: "consumer-uid",
	}}
	repo := &memoryRepository{
		volumes: map[string]volumeapi.State{preflightVolume: {Phase: volumeapi.PhaseMoving, OwnerNode: "source", ActiveMove: move.Name, PublishedNodes: []string{"source"}}},
		pools:   []volumeapi.Pool{{Name: "source", NodeName: "source", MountPath: "/pool"}, {Name: "destination", NodeName: "destination", MountPath: "/pool"}},
		moves:   []volumeapi.Move{move},
	}
	client := fake.NewSimpleClientset(mobilityObjects(preflightVolume)...)
	client.PrependReactor("create", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		return true, nil, fmt.Errorf("API timeout")
	})
	r := &Reconciler{Client: client, Repository: repo, Namespace: "system", HelperImage: "helper"}
	if err := r.ReconcileAll(ctx); err == nil {
		t.Fatal("action failure was hidden")
	}
	failed := repo.moves[0].Status
	if failed.Phase != string(fsm.PhaseEvicting) || failed.Reason != "ActionFailed" || !strings.Contains(failed.Message, "API timeout") || failed.EvictionRequested {
		t.Fatalf("action failure diagnostic=%+v", failed)
	}

	r.Client = fake.NewSimpleClientset(mobilityObjects(preflightVolume)...)
	if err := r.ReconcileAll(ctx); err != nil {
		t.Fatal(err)
	}
	recovered := repo.moves[0].Status
	if recovered.Phase != string(fsm.PhaseEvicting) || recovered.Reason != "" || !recovered.EvictionRequested || strings.Contains(recovered.Message, "API timeout") {
		t.Fatalf("stale action diagnostic=%+v", recovered)
	}
}

func assertEventContains(t *testing.T, recorder *record.FakeRecorder, want string) {
	t.Helper()
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, want) {
			t.Fatalf("event=%q want substring=%q", event, want)
		}
	default:
		t.Fatalf("missing event containing %q", want)
	}
}

func assertNoEvent(t *testing.T, recorder *record.FakeRecorder) {
	t.Helper()
	select {
	case event := <-recorder.Events:
		t.Fatalf("unexpected event=%q", event)
	default:
	}
}
