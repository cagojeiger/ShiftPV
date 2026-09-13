package model_test

import (
	"fmt"
	"testing"
)

type deleteState struct {
	NodeUp bool

	ObjectExists      bool
	FinalizerPresent  bool
	DeletionRequested bool
	CapacityHeld      bool

	TargetPresent bool
	TargetMounted bool

	PoolGeneration            uint8
	Observation               poolObservation
	UnmountRequiredGeneration uint8
	UnmountAcknowledged       bool
	AbsenceRequiredGeneration uint8
	AbsenceAcknowledged       bool

	CleanupIntent     bool
	JobCreated        bool
	ExecutorBound     bool
	JobRunning        bool
	PurgeLocalReceipt bool
	PurgeAPIReceipt   bool

	Contradiction bool
}

func initialDeleteState(targetMounted bool) deleteState {
	return deleteState{
		NodeUp:           true,
		ObjectExists:     true,
		FinalizerPresent: true,
		CapacityHeld:     true,
		TargetPresent:    true,
		TargetMounted:    targetMounted,
		PoolGeneration:   1,
		Observation: poolObservation{
			Generation:      1,
			Valid:           true,
			Complete:        true,
			SourcePresent:   true,
			SourcePublished: targetMounted,
		},
	}
}

type deleteAction string

const (
	actionDeleteNodeDown        deleteAction = "node down"
	actionDeleteNodeUp          deleteAction = "node up"
	actionRequestVolumeDelete   deleteAction = "request volume delete"
	actionUnpublishDeleteTarget deleteAction = "unpublish delete target"
	actionRequestUnmountScan    deleteAction = "request unmount scan"
	actionObserveDeleteTarget   deleteAction = "observe delete target"
	actionObserveDeleteStale    deleteAction = "observe stale delete target"
	actionObserveDeleteInvalid  deleteAction = "observe invalid delete target"
	actionObserveDeletePartial  deleteAction = "observe incomplete delete target"
	actionObserveDeleteConflict deleteAction = "observe delete identity contradiction"
	actionAcceptUnmount         deleteAction = "accept unmount"
	actionRecordDeleteIntent    deleteAction = "record delete intent"
	actionCreateDeleteJob       deleteAction = "create suspended delete job"
	actionBindDeleteExecutor    deleteAction = "bind delete executor"
	actionStartDeleteJob        deleteAction = "start delete job"
	actionDeleteTargetEffect    deleteAction = "delete target effect before receipt"
	actionPurgeDeleteTarget     deleteAction = "purge delete target"
	actionReportDeletePurge     deleteAction = "report delete purge"
	actionRequestDeleteAbsence  deleteAction = "request delete absence scan"
	actionAcceptDeleteAbsence   deleteAction = "accept delete absence"
	actionRemoveVolumeFinalizer deleteAction = "remove volume finalizer"
)

var allDeleteActions = []deleteAction{
	actionDeleteNodeDown,
	actionDeleteNodeUp,
	actionRequestVolumeDelete,
	actionUnpublishDeleteTarget,
	actionRequestUnmountScan,
	actionObserveDeleteTarget,
	actionObserveDeleteStale,
	actionObserveDeleteInvalid,
	actionObserveDeletePartial,
	actionObserveDeleteConflict,
	actionAcceptUnmount,
	actionRecordDeleteIntent,
	actionCreateDeleteJob,
	actionBindDeleteExecutor,
	actionStartDeleteJob,
	actionDeleteTargetEffect,
	actionPurgeDeleteTarget,
	actionReportDeletePurge,
	actionRequestDeleteAbsence,
	actionAcceptDeleteAbsence,
	actionRemoveVolumeFinalizer,
}

func applyDelete(state deleteState, action deleteAction) (deleteState, bool) {
	if !state.ObjectExists || state.Contradiction {
		return state, false
	}
	next := state
	switch action {
	case actionDeleteNodeDown:
		if !state.NodeUp {
			return state, false
		}
		next.NodeUp = false
	case actionDeleteNodeUp:
		if state.NodeUp {
			return state, false
		}
		next.NodeUp = true
	case actionRequestVolumeDelete:
		if state.DeletionRequested || !state.FinalizerPresent {
			return state, false
		}
		next.DeletionRequested = true
	case actionUnpublishDeleteTarget:
		if !state.NodeUp || !state.TargetMounted {
			return state, false
		}
		next.TargetMounted = false
	case actionRequestUnmountScan:
		if !state.DeletionRequested || state.UnmountRequiredGeneration != 0 {
			return state, false
		}
		next.PoolGeneration++
		next.UnmountRequiredGeneration = next.PoolGeneration
	case actionObserveDeleteTarget:
		if !state.NodeUp {
			return state, false
		}
		next.Observation = poolObservation{
			Generation:      state.PoolGeneration,
			Valid:           true,
			Complete:        true,
			SourcePresent:   state.TargetPresent,
			SourcePublished: state.TargetMounted,
		}
	case actionObserveDeleteStale:
		if !state.NodeUp || state.PoolGeneration == 0 {
			return state, false
		}
		next.Observation = poolObservation{
			Generation:      state.PoolGeneration - 1,
			Valid:           true,
			Complete:        true,
			SourcePresent:   state.TargetPresent,
			SourcePublished: state.TargetMounted,
		}
	case actionObserveDeleteInvalid:
		if !state.NodeUp {
			return state, false
		}
		next.Observation = poolObservation{Generation: state.PoolGeneration, Complete: true}
	case actionObserveDeletePartial:
		if !state.NodeUp {
			return state, false
		}
		next.Observation = poolObservation{
			Generation:      state.PoolGeneration,
			Valid:           true,
			SourcePresent:   state.TargetPresent,
			SourcePublished: state.TargetMounted,
		}
	case actionObserveDeleteConflict:
		if !state.DeletionRequested {
			return state, false
		}
		next.Contradiction = true
	case actionAcceptUnmount:
		observation := state.Observation
		if state.UnmountAcknowledged || state.UnmountRequiredGeneration == 0 || !observation.Valid || !observation.Complete ||
			observation.Generation != state.PoolGeneration || observation.Generation < state.UnmountRequiredGeneration ||
			!observation.SourcePresent || observation.SourcePublished {
			return state, false
		}
		next.UnmountAcknowledged = true
	case actionRecordDeleteIntent:
		if !state.DeletionRequested || !state.FinalizerPresent || !state.UnmountAcknowledged || !state.TargetPresent || state.CleanupIntent {
			return state, false
		}
		next.CleanupIntent = true
	case actionCreateDeleteJob:
		if !state.CleanupIntent || state.JobCreated {
			return state, false
		}
		next.JobCreated = true
	case actionBindDeleteExecutor:
		if !state.JobCreated || state.ExecutorBound {
			return state, false
		}
		next.ExecutorBound = true
	case actionStartDeleteJob:
		if !state.JobCreated || !state.ExecutorBound || state.JobRunning {
			return state, false
		}
		next.JobRunning = true
	case actionDeleteTargetEffect:
		if !canPurgeDeleteTarget(state) || !state.TargetPresent || state.PurgeLocalReceipt {
			return state, false
		}
		// The unlink completed, but the node process can fail before persisting
		// the local receipt. A later idempotent purge must close this window.
		next.TargetPresent = false
	case actionPurgeDeleteTarget:
		if !canPurgeDeleteTarget(state) || state.PurgeLocalReceipt {
			return state, false
		}
		next.TargetPresent = false
		next.PurgeLocalReceipt = true
	case actionReportDeletePurge:
		if !state.NodeUp || !state.PurgeLocalReceipt || state.PurgeAPIReceipt {
			return state, false
		}
		next.PurgeAPIReceipt = true
	case actionRequestDeleteAbsence:
		if !state.PurgeLocalReceipt || state.AbsenceRequiredGeneration != 0 {
			return state, false
		}
		next.PoolGeneration++
		next.AbsenceRequiredGeneration = next.PoolGeneration
	case actionAcceptDeleteAbsence:
		observation := state.Observation
		if state.AbsenceAcknowledged || state.AbsenceRequiredGeneration == 0 || !observation.Valid || !observation.Complete ||
			observation.Generation != state.PoolGeneration || observation.Generation < state.AbsenceRequiredGeneration ||
			observation.SourcePresent || observation.SourcePublished {
			return state, false
		}
		next.AbsenceAcknowledged = true
	case actionRemoveVolumeFinalizer:
		if !state.DeletionRequested || !state.FinalizerPresent || !state.PurgeAPIReceipt || !state.AbsenceAcknowledged {
			return state, false
		}
		next.FinalizerPresent = false
		next.ObjectExists = false
		next.CapacityHeld = false
	default:
		return state, false
	}
	if next == state {
		return state, false
	}
	return next, true
}

func canPurgeDeleteTarget(state deleteState) bool {
	return state.NodeUp && state.DeletionRequested && state.FinalizerPresent && state.UnmountAcknowledged &&
		state.CleanupIntent && state.ExecutorBound && state.JobRunning && !state.TargetMounted
}

func reachableDeleteStates(starts ...deleteState) []deleteState {
	seen := make(map[deleteState]struct{})
	queue := append([]deleteState(nil), starts...)
	for len(queue) > 0 {
		state := queue[0]
		queue = queue[1:]
		if _, exists := seen[state]; exists {
			continue
		}
		seen[state] = struct{}{}
		for _, action := range allDeleteActions {
			if next, ok := applyDelete(state, action); ok {
				queue = append(queue, next)
			}
		}
	}
	result := make([]deleteState, 0, len(seen))
	for state := range seen {
		result = append(result, state)
	}
	return result
}

func stableDeleteConvergence(state deleteState) (deleteState, error) {
	state.NodeUp = true
	for step := 0; step < 24 && state.ObjectExists; step++ {
		advanced := false
		for _, action := range []deleteAction{
			actionRequestVolumeDelete,
			actionUnpublishDeleteTarget,
			actionRequestUnmountScan,
			actionObserveDeleteTarget,
			actionAcceptUnmount,
			actionRecordDeleteIntent,
			actionCreateDeleteJob,
			actionBindDeleteExecutor,
			actionStartDeleteJob,
			actionPurgeDeleteTarget,
			actionReportDeletePurge,
			actionRequestDeleteAbsence,
			actionAcceptDeleteAbsence,
			actionRemoveVolumeFinalizer,
		} {
			if next, ok := applyDelete(state, action); ok {
				state = next
				advanced = true
				break
			}
		}
		if !advanced {
			return state, fmt.Errorf("no stable delete transition from %#v", state)
		}
	}
	if state.ObjectExists {
		return state, fmt.Errorf("stable delete transition limit exceeded: %#v", state)
	}
	return state, nil
}

func TestVolumeDelete04ExhaustiveSafetyAndRecoveredLiveness(t *testing.T) {
	states := reachableDeleteStates(
		initialDeleteState(false),
		initialDeleteState(true),
		deleteAfterEffectState(),
		unexpectedAbsentDeleteState(),
	)
	if len(states) < 50 {
		t.Fatalf("model explored too few delete states: %d", len(states))
	}
	for _, state := range states {
		assertDeleteInvariants(t, state)
		if state.Contradiction {
			assertDeleteQuarantined(t, state)
			continue
		}
		converged, err := stableDeleteConvergence(state)
		if err != nil {
			t.Fatalf("reachable delete state did not converge after node recovery: start=%#v: %v", state, err)
		}
		if converged.ObjectExists || converged.TargetPresent || converged.FinalizerPresent {
			t.Fatalf("delete did not converge to an absent object and copy: start=%#v end=%#v", state, converged)
		}
	}
	t.Logf("checked %d reachable volume-delete states", len(states))
}

func TestVolumeDelete04RejectsStaleUnmountAndAbsenceProofs(t *testing.T) {
	state := initialDeleteState(false)
	state = requireDeleteAction(t, state, actionRequestVolumeDelete)
	state = requireDeleteAction(t, state, actionRequestUnmountScan)
	if _, ok := applyDelete(state, actionAcceptUnmount); ok {
		t.Fatal("pre-request observation satisfied the unmount fence")
	}
	state = requireDeleteAction(t, state, actionObserveDeleteTarget)
	state = requireDeleteAction(t, state, actionAcceptUnmount)
	state = driveDelete(t, state,
		actionRecordDeleteIntent,
		actionCreateDeleteJob,
		actionBindDeleteExecutor,
		actionStartDeleteJob,
		actionPurgeDeleteTarget,
		actionReportDeletePurge,
		actionRequestDeleteAbsence,
	)
	if _, ok := applyDelete(state, actionAcceptDeleteAbsence); ok {
		t.Fatal("pre-purge observation satisfied the absence fence")
	}
}

func TestVolumeDelete04ContradictionPreservesFinalizerAndData(t *testing.T) {
	state := initialDeleteState(false)
	state.DeletionRequested = true
	state.Contradiction = true
	assertDeleteQuarantined(t, state)
	if !state.TargetPresent || !state.FinalizerPresent || !state.ObjectExists {
		t.Fatalf("contradiction did not preserve authority and data: %#v", state)
	}
}

func TestVolumeDelete04RecoversWhenDeleteEffectPrecedesReceipt(t *testing.T) {
	state := deleteAfterEffectState()
	if state.TargetPresent || state.PurgeLocalReceipt || !state.CleanupIntent {
		t.Fatalf("invalid crash-window fixture: %#v", state)
	}
	state = requireDeleteAction(t, state, actionPurgeDeleteTarget)
	if !state.PurgeLocalReceipt {
		t.Fatalf("idempotent purge did not persist a receipt for an already absent target: %#v", state)
	}
	converged, err := stableDeleteConvergence(state)
	if err != nil || converged.ObjectExists || converged.CapacityHeld {
		t.Fatalf("authorized already-absent target did not converge: end=%#v err=%v", converged, err)
	}
}

func TestVolumeDelete04UnexpectedAbsenceIsNotCleanupProof(t *testing.T) {
	state := unexpectedAbsentDeleteState()
	assertDeleteQuarantined(t, state)
	if state.PurgeLocalReceipt || state.PurgeAPIReceipt || state.AbsenceAcknowledged {
		t.Fatalf("unexpected absence was converted into a cleanup receipt: %#v", state)
	}
}

func assertDeleteInvariants(t *testing.T, state deleteState) {
	t.Helper()
	if state.TargetMounted && !state.TargetPresent {
		t.Fatalf("absent delete target remains mounted: %#v", state)
	}
	if state.CleanupIntent && (!state.DeletionRequested || !state.UnmountAcknowledged || state.ObjectExists && !state.FinalizerPresent) {
		t.Fatalf("cleanup intent lacks deletion authority: %#v", state)
	}
	if state.PurgeLocalReceipt && (!state.DeletionRequested || !state.UnmountAcknowledged || !state.CleanupIntent ||
		!state.ExecutorBound || !state.JobRunning || state.TargetPresent) {
		t.Fatalf("target purge lacks deletion authority: %#v", state)
	}
	if state.PurgeAPIReceipt && !state.PurgeLocalReceipt {
		t.Fatalf("API purge receipt lacks a local receipt: %#v", state)
	}
	if state.ObjectExists && state.PurgeLocalReceipt && !state.FinalizerPresent {
		t.Fatalf("live deleting Volume lost its finalizer after purge: %#v", state)
	}
	if !state.ObjectExists && (state.FinalizerPresent || state.TargetPresent || !state.PurgeAPIReceipt || !state.AbsenceAcknowledged) {
		t.Fatalf("Volume disappeared before cleanup settlement: %#v", state)
	}
	// The Volume object itself owns the logical capacity reservation. Even
	// after the bytes are purged, the hold remains until finalizer removal makes
	// the object NotFound.
	if state.CapacityHeld != state.ObjectExists {
		t.Fatalf("capacity hold and Volume lifetime diverged: %#v", state)
	}
}

func deleteAfterEffectState() deleteState {
	state := initialDeleteState(false)
	state.DeletionRequested = true
	state.PoolGeneration = 2
	state.Observation = poolObservation{
		Generation:    2,
		Valid:         true,
		Complete:      true,
		SourcePresent: true,
	}
	state.UnmountRequiredGeneration = 2
	state.UnmountAcknowledged = true
	state.CleanupIntent = true
	state.JobCreated = true
	state.ExecutorBound = true
	state.JobRunning = true
	state.TargetPresent = false
	return state
}

func unexpectedAbsentDeleteState() deleteState {
	state := initialDeleteState(false)
	state.DeletionRequested = true
	state.TargetPresent = false
	state.Observation.SourcePresent = false
	state.Contradiction = true
	return state
}

func assertDeleteQuarantined(t *testing.T, state deleteState) {
	t.Helper()
	if !state.Contradiction || !state.ObjectExists || !state.FinalizerPresent || !state.CapacityHeld {
		t.Fatalf("contradictory deletion was not retained for review: %#v", state)
	}
	for _, action := range allDeleteActions {
		if _, ok := applyDelete(state, action); ok {
			t.Fatalf("contradictory deletion allowed transition %q: %#v", action, state)
		}
	}
}

func requireDeleteAction(t *testing.T, state deleteState, action deleteAction) deleteState {
	t.Helper()
	next, ok := applyDelete(state, action)
	if !ok {
		t.Fatalf("delete action %q was not accepted from %#v", action, state)
	}
	return next
}

func driveDelete(t *testing.T, state deleteState, actions ...deleteAction) deleteState {
	t.Helper()
	for _, action := range actions {
		state = requireDeleteAction(t, state, action)
	}
	return state
}
