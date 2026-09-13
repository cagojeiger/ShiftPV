package model_test

import (
	"fmt"
	"testing"
)

// This package is an executable design model for the proposed 0.4 protocol.
// It deliberately has no dependency on the current implementation: a passing
// model establishes that the proposed facts and fences are internally
// consistent, not that the production controller already implements them.

type owner uint8

const (
	ownerSource owner = iota
	ownerDestination
)

type outcome uint8

const (
	outcomeActive outcome = iota
	outcomeSucceeded
	outcomeAborted
	outcomeBlocked
)

type poolObservation struct {
	Generation      uint8
	Valid           bool
	Complete        bool
	SourcePresent   bool
	SourcePublished bool
}

type destinationPoolObservation struct {
	Generation uint8
	Valid      bool
	Complete   bool
	Present    bool
	Published  bool
}

type moveState struct {
	Owner   owner
	Outcome outcome

	MoveActive     bool
	Fenced         bool
	AbortRequested bool

	SourceUp      bool
	DestinationUp bool

	SourcePresent      bool
	DestinationPresent bool
	SourceMounted      bool
	DestinationMounted bool

	// BeginPublish records the API publication intent before the local mount
	// effect. NodeUnpublish clears it only after holding the local lock,
	// removing the mount, and inspecting that no mount remains.
	SourcePublicationIntent    bool
	SourcePublishInFlight      bool
	DestinationPublishInFlight bool

	PoolGeneration            uint8
	Observation               poolObservation
	AbsenceRequiredGeneration uint8
	SourceAbsenceAcknowledged bool

	DestinationReserved      bool
	DestinationPartial       bool
	CopyLocalReceipt         bool
	CopyAPIReceipt           bool
	DestinationPublishIntent bool
	DestinationPublishAck    bool

	SourceCleanupIntent     bool
	SourcePurgeLocalReceipt bool
	SourcePurgeAPIReceipt   bool

	DestinationCleanupIntent     bool
	DestinationPurgeLocalReceipt bool
	DestinationPurgeAPIReceipt   bool
	DestinationPoolGeneration    uint8
	DestinationObservation       destinationPoolObservation
	DestinationAbsenceGeneration uint8
	DestinationAbsenceAck        bool

	Contradiction bool
}

func initialMoveState(sourceMounted bool) moveState {
	return moveState{
		Owner:                     ownerSource,
		Outcome:                   outcomeActive,
		SourceUp:                  true,
		DestinationUp:             true,
		SourcePresent:             true,
		SourceMounted:             sourceMounted,
		SourcePublicationIntent:   sourceMounted,
		PoolGeneration:            1,
		DestinationPoolGeneration: 1,
		Observation: poolObservation{
			Generation:      1,
			Valid:           true,
			Complete:        true,
			SourcePresent:   true,
			SourcePublished: sourceMounted,
		},
	}
}

type moveAction string

const (
	actionSourceDown               moveAction = "source down"
	actionSourceUp                 moveAction = "source up"
	actionDestinationDown          moveAction = "destination down"
	actionDestinationUp            moveAction = "destination up"
	actionBeginSourcePublish       moveAction = "begin source publish"
	actionFinishSourcePublish      moveAction = "finish source publish"
	actionCancelSourcePublish      moveAction = "cancel source publish"
	actionUnpublishSource          moveAction = "unpublish source"
	actionStartMove                moveAction = "start move"
	actionObserveSource            moveAction = "observe source"
	actionObserveSourceStale       moveAction = "observe stale source"
	actionObserveSourceInvalid     moveAction = "observe invalid source"
	actionObserveSourceIncomplete  moveAction = "observe incomplete source"
	actionObserveContradiction     moveAction = "observe identity contradiction"
	actionReserveDestination       moveAction = "reserve destination"
	actionCopyEffect               moveAction = "write partial destination"
	actionVerifyCopy               moveAction = "verify destination copy"
	actionReportCopy               moveAction = "report copy receipt"
	actionRequestAbort             moveAction = "request abort"
	actionResumeSource             moveAction = "resume source"
	actionStartDestinationCleanup  moveAction = "start destination cleanup"
	actionDestinationDeleteEffect  moveAction = "delete destination effect before receipt"
	actionPurgeDestination         moveAction = "purge destination"
	actionReportDestinationPurge   moveAction = "report destination purge"
	actionRequestDestinationScan   moveAction = "request destination absence scan"
	actionObserveDestination       moveAction = "observe destination"
	actionAcceptDestinationAbsence moveAction = "accept destination absence"
	actionFinishAbort              moveAction = "finish abort"
	actionCommitOwner              moveAction = "commit owner"
	actionBeginDestinationPublish  moveAction = "begin destination publish"
	actionFinishDestinationPublish moveAction = "finish destination publish"
	actionCancelDestinationPublish moveAction = "cancel destination publish"
	actionAckDestinationPublish    moveAction = "ack destination publish"
	actionStartSourceCleanup       moveAction = "start source cleanup"
	actionSourceDeleteEffect       moveAction = "delete source effect before receipt"
	actionPurgeSource              moveAction = "purge source"
	actionReportSourcePurge        moveAction = "report source purge"
	actionRequestAbsenceScan       moveAction = "request source absence scan"
	actionAcceptSourceAbsence      moveAction = "accept source absence"
	actionFinishSuccess            moveAction = "finish success"
)

var allMoveActions = []moveAction{
	actionSourceDown,
	actionSourceUp,
	actionDestinationDown,
	actionDestinationUp,
	actionBeginSourcePublish,
	actionFinishSourcePublish,
	actionCancelSourcePublish,
	actionUnpublishSource,
	actionStartMove,
	actionObserveSource,
	actionObserveSourceStale,
	actionObserveSourceInvalid,
	actionObserveSourceIncomplete,
	actionObserveContradiction,
	actionReserveDestination,
	actionCopyEffect,
	actionVerifyCopy,
	actionReportCopy,
	actionRequestAbort,
	actionResumeSource,
	actionStartDestinationCleanup,
	actionDestinationDeleteEffect,
	actionPurgeDestination,
	actionReportDestinationPurge,
	actionRequestDestinationScan,
	actionObserveDestination,
	actionAcceptDestinationAbsence,
	actionFinishAbort,
	actionCommitOwner,
	actionBeginDestinationPublish,
	actionFinishDestinationPublish,
	actionCancelDestinationPublish,
	actionAckDestinationPublish,
	actionStartSourceCleanup,
	actionSourceDeleteEffect,
	actionPurgeSource,
	actionReportSourcePurge,
	actionRequestAbsenceScan,
	actionAcceptSourceAbsence,
	actionFinishSuccess,
}

func applyMove(state moveState, action moveAction) (moveState, bool) {
	if state.Outcome != outcomeActive || state.Contradiction {
		return state, false
	}
	next := state
	switch action {
	case actionSourceDown:
		if !state.SourceUp {
			return state, false
		}
		next.SourceUp = false
	case actionSourceUp:
		if state.SourceUp {
			return state, false
		}
		next.SourceUp = true
	case actionDestinationDown:
		if !state.DestinationUp {
			return state, false
		}
		next.DestinationUp = false
	case actionDestinationUp:
		if state.DestinationUp {
			return state, false
		}
		next.DestinationUp = true
	case actionBeginSourcePublish:
		if state.Owner != ownerSource || state.Fenced || !state.SourceUp || !state.SourcePresent || state.SourceMounted ||
			state.SourcePublicationIntent || state.SourcePublishInFlight {
			return state, false
		}
		next.SourcePublicationIntent = true
		next.SourcePublishInFlight = true
	case actionFinishSourcePublish:
		if !state.SourcePublicationIntent || !state.SourcePublishInFlight || !state.SourceUp || state.Owner != ownerSource {
			return state, false
		}
		next.SourcePublishInFlight = false
		next.SourceMounted = true
	case actionCancelSourcePublish:
		if !state.SourcePublicationIntent || !state.SourcePublishInFlight || !state.SourceUp || state.SourceMounted {
			return state, false
		}
		next.SourcePublishInFlight = false
		next.SourcePublicationIntent = false
	case actionUnpublishSource:
		if !state.SourceUp || !state.SourcePublicationIntent || !state.SourceMounted || state.SourcePublishInFlight {
			return state, false
		}
		next.SourceMounted = false
		next.SourcePublicationIntent = false
	case actionStartMove:
		if state.MoveActive || state.Owner != ownerSource || state.Fenced || !state.SourcePresent {
			return state, false
		}
		next.MoveActive = true
		next.Fenced = true
	case actionObserveSource:
		if !state.SourceUp {
			return state, false
		}
		next.Observation = poolObservation{
			Generation:      state.PoolGeneration,
			Valid:           true,
			Complete:        true,
			SourcePresent:   state.SourcePresent,
			SourcePublished: state.SourcePublicationIntent || state.SourceMounted,
		}
	case actionObserveSourceStale:
		if !state.SourceUp || state.PoolGeneration == 0 {
			return state, false
		}
		next.Observation = poolObservation{
			Generation:      state.PoolGeneration - 1,
			Valid:           true,
			Complete:        true,
			SourcePresent:   state.SourcePresent,
			SourcePublished: state.SourcePublicationIntent || state.SourceMounted,
		}
	case actionObserveSourceInvalid:
		if !state.SourceUp {
			return state, false
		}
		next.Observation = poolObservation{Generation: state.PoolGeneration, Complete: true}
	case actionObserveSourceIncomplete:
		if !state.SourceUp {
			return state, false
		}
		next.Observation = poolObservation{
			Generation:      state.PoolGeneration,
			Valid:           true,
			SourcePresent:   state.SourcePresent,
			SourcePublished: state.SourcePublicationIntent || state.SourceMounted,
		}
	case actionObserveContradiction:
		if !state.MoveActive {
			return state, false
		}
		next.Contradiction = true
		next.Outcome = outcomeBlocked
	case actionReserveDestination:
		if !state.MoveActive || state.AbortRequested || state.Owner != ownerSource || !sourceAvailableForMoveEffect(state) ||
			!state.DestinationUp || state.DestinationReserved {
			return state, false
		}
		next.DestinationReserved = true
	case actionCopyEffect:
		if !state.MoveActive || state.AbortRequested || state.Owner != ownerSource || !state.DestinationReserved ||
			!sourceAvailableForMoveEffect(state) || !state.DestinationUp || state.DestinationPresent {
			return state, false
		}
		next.DestinationPresent = true
		next.DestinationPartial = true
	case actionVerifyCopy:
		if !state.MoveActive || state.AbortRequested || state.Owner != ownerSource ||
			!sourceAvailableForMoveEffect(state) || !state.DestinationUp ||
			!state.DestinationPresent || !state.DestinationPartial || state.CopyLocalReceipt {
			return state, false
		}
		next.DestinationPartial = false
		next.CopyLocalReceipt = true
	case actionReportCopy:
		if !state.CopyLocalReceipt || state.CopyAPIReceipt || state.DestinationPartial || !state.DestinationUp || !state.DestinationPresent {
			return state, false
		}
		next.CopyAPIReceipt = true
	case actionRequestAbort:
		if !state.MoveActive || state.AbortRequested || state.Owner != ownerSource {
			return state, false
		}
		next.AbortRequested = true
	case actionResumeSource:
		if !state.MoveActive || !state.AbortRequested || state.Owner != ownerSource || !state.SourceUp || !state.SourcePresent || !state.Fenced {
			return state, false
		}
		next.Fenced = false
	case actionStartDestinationCleanup:
		if !state.MoveActive || !state.AbortRequested || state.Owner != ownerSource || !state.SourceUp || !state.SourcePresent ||
			!state.DestinationPresent || state.DestinationCleanupIntent {
			return state, false
		}
		next.DestinationCleanupIntent = true
	case actionDestinationDeleteEffect:
		if !canPurgeDestination(state) || !state.DestinationPresent || state.DestinationPurgeLocalReceipt {
			return state, false
		}
		next.DestinationPresent = false
		next.DestinationPartial = false
		next.DestinationMounted = false
	case actionPurgeDestination:
		if !canPurgeDestination(state) || state.DestinationPurgeLocalReceipt {
			return state, false
		}
		next.DestinationPresent = false
		next.DestinationPartial = false
		next.DestinationMounted = false
		next.DestinationPurgeLocalReceipt = true
	case actionReportDestinationPurge:
		if !state.DestinationPurgeLocalReceipt || state.DestinationPurgeAPIReceipt || !state.DestinationUp {
			return state, false
		}
		next.DestinationPurgeAPIReceipt = true
	case actionRequestDestinationScan:
		if !state.DestinationPurgeLocalReceipt || state.DestinationAbsenceGeneration != 0 {
			return state, false
		}
		next.DestinationPoolGeneration++
		next.DestinationAbsenceGeneration = next.DestinationPoolGeneration
	case actionObserveDestination:
		if !state.DestinationUp {
			return state, false
		}
		next.DestinationObservation = destinationPoolObservation{
			Generation: state.DestinationPoolGeneration,
			Valid:      true,
			Complete:   true,
			Present:    state.DestinationPresent,
			Published:  state.DestinationMounted,
		}
	case actionAcceptDestinationAbsence:
		if state.DestinationAbsenceAck || !canAcceptDestinationAbsence(state) {
			return state, false
		}
		next.DestinationAbsenceAck = true
	case actionFinishAbort:
		if !state.MoveActive || !state.AbortRequested || state.Owner != ownerSource || state.Fenced || !state.SourcePresent ||
			state.DestinationPresent || (state.DestinationCleanupIntent &&
			(!state.DestinationPurgeAPIReceipt || !state.DestinationAbsenceAck)) {
			return state, false
		}
		next.MoveActive = false
		next.DestinationReserved = false
		next.Outcome = outcomeAborted
	case actionCommitOwner:
		if !state.MoveActive || state.AbortRequested || state.Owner != ownerSource || !state.CopyAPIReceipt ||
			!state.DestinationUp || !state.DestinationPresent || !sourceAvailableForMoveEffect(state) {
			return state, false
		}
		next.Owner = ownerDestination
		next.Fenced = false
		next.DestinationReserved = false
	case actionBeginDestinationPublish:
		if state.Owner != ownerDestination || state.Fenced || !state.DestinationUp || !state.DestinationPresent ||
			state.DestinationMounted || state.DestinationPublishInFlight {
			return state, false
		}
		next.DestinationPublishIntent = true
		next.DestinationPublishInFlight = true
	case actionFinishDestinationPublish:
		if !state.DestinationPublishIntent || !state.DestinationPublishInFlight || !state.DestinationUp ||
			state.Owner != ownerDestination || !state.DestinationPresent {
			return state, false
		}
		next.DestinationPublishInFlight = false
		next.DestinationMounted = true
	case actionCancelDestinationPublish:
		if !state.DestinationPublishInFlight {
			return state, false
		}
		next.DestinationPublishInFlight = false
	case actionAckDestinationPublish:
		observation := state.DestinationObservation
		if state.DestinationPublishAck || !state.DestinationPublishIntent || state.Owner != ownerDestination ||
			!state.DestinationUp || !state.DestinationPresent || !state.DestinationMounted ||
			!observation.Valid || !observation.Complete || observation.Generation != state.DestinationPoolGeneration ||
			!observation.Present || !observation.Published {
			return state, false
		}
		next.DestinationPublishAck = true
	case actionStartSourceCleanup:
		if !state.MoveActive || state.Owner != ownerDestination || !state.DestinationPublishAck || !state.DestinationUp ||
			!state.DestinationPresent || !state.SourcePresent || state.SourceCleanupIntent {
			return state, false
		}
		next.SourceCleanupIntent = true
	case actionSourceDeleteEffect:
		if !canPurgeSource(state) || !state.SourcePresent || state.SourcePurgeLocalReceipt {
			return state, false
		}
		next.SourcePresent = false
	case actionPurgeSource:
		if !canPurgeSource(state) || state.SourcePurgeLocalReceipt {
			return state, false
		}
		next.SourcePresent = false
		next.SourcePurgeLocalReceipt = true
	case actionReportSourcePurge:
		if !state.SourcePurgeLocalReceipt || state.SourcePurgeAPIReceipt || !state.SourceUp {
			return state, false
		}
		next.SourcePurgeAPIReceipt = true
	case actionRequestAbsenceScan:
		if !state.SourcePurgeLocalReceipt || state.AbsenceRequiredGeneration != 0 {
			return state, false
		}
		next.PoolGeneration++
		next.AbsenceRequiredGeneration = next.PoolGeneration
	case actionAcceptSourceAbsence:
		if state.SourceAbsenceAcknowledged || !canAcceptSourceAbsence(state) {
			return state, false
		}
		next.SourceAbsenceAcknowledged = true
	case actionFinishSuccess:
		if !state.MoveActive || state.Owner != ownerDestination || !state.DestinationPresent || !state.DestinationPublishAck ||
			state.SourcePresent || !state.SourcePurgeAPIReceipt || !state.SourceAbsenceAcknowledged {
			return state, false
		}
		next.MoveActive = false
		next.Outcome = outcomeSucceeded
	default:
		return state, false
	}
	if next == state {
		return state, false
	}
	return next, true
}

func canPurgeDestination(state moveState) bool {
	return state.DestinationCleanupIntent && state.DestinationUp && state.Owner == ownerSource && state.AbortRequested && !state.DestinationMounted
}

func canPurgeSource(state moveState) bool {
	return state.SourceCleanupIntent && state.SourceUp && !state.SourceMounted && state.Owner == ownerDestination && state.DestinationPublishAck
}

func sourceAvailableForMoveEffect(state moveState) bool {
	return state.SourceUp && state.SourcePresent && !state.SourcePublicationIntent && !state.SourcePublishInFlight && !state.SourceMounted
}

func canAcceptSourceAbsence(state moveState) bool {
	observation := state.Observation
	return state.SourcePurgeLocalReceipt && state.AbsenceRequiredGeneration != 0 && observation.Valid && observation.Complete &&
		observation.Generation == state.PoolGeneration && observation.Generation >= state.AbsenceRequiredGeneration &&
		!observation.SourcePresent && !observation.SourcePublished
}

func canAcceptDestinationAbsence(state moveState) bool {
	observation := state.DestinationObservation
	return state.DestinationPurgeLocalReceipt && state.DestinationAbsenceGeneration != 0 &&
		observation.Valid && observation.Complete && observation.Generation == state.DestinationPoolGeneration &&
		observation.Generation >= state.DestinationAbsenceGeneration &&
		!observation.Present && !observation.Published
}

func moveSuccessors(state moveState) []moveState {
	result := make([]moveState, 0, len(allMoveActions))
	for _, action := range allMoveActions {
		if next, ok := applyMove(state, action); ok {
			result = append(result, next)
		}
	}
	return result
}

func reachableMoveStates(starts ...moveState) []moveState {
	seen := make(map[moveState]struct{})
	queue := append([]moveState(nil), starts...)
	for len(queue) > 0 {
		state := queue[0]
		queue = queue[1:]
		if _, exists := seen[state]; exists {
			continue
		}
		seen[state] = struct{}{}
		queue = append(queue, moveSuccessors(state)...)
	}
	result := make([]moveState, 0, len(seen))
	for state := range seen {
		result = append(result, state)
	}
	return result
}

func stableConvergence(state moveState) (moveState, error) {
	state.SourceUp = true
	state.DestinationUp = true
	for step := 0; step < 48 && state.Outcome == outcomeActive; step++ {
		actions := stableActions(state)
		advanced := false
		for _, action := range actions {
			if next, ok := applyMove(state, action); ok {
				state = next
				advanced = true
				break
			}
		}
		if !advanced {
			return state, fmt.Errorf("no stable transition from %#v", state)
		}
	}
	if state.Outcome == outcomeActive {
		return state, fmt.Errorf("stable transition limit exceeded: %#v", state)
	}
	return state, nil
}

func stableActions(state moveState) []moveAction {
	if !state.MoveActive {
		return []moveAction{actionStartMove}
	}
	if state.SourcePublishInFlight {
		return []moveAction{actionFinishSourcePublish, actionCancelSourcePublish}
	}
	if state.DestinationPublishInFlight {
		return []moveAction{actionFinishDestinationPublish, actionCancelDestinationPublish}
	}
	if state.AbortRequested && state.Owner == ownerSource {
		return []moveAction{
			actionResumeSource,
			actionStartDestinationCleanup,
			actionPurgeDestination,
			actionReportDestinationPurge,
			actionRequestDestinationScan,
			actionObserveDestination,
			actionAcceptDestinationAbsence,
			actionFinishAbort,
		}
	}
	if state.Owner == ownerSource {
		return []moveAction{
			actionUnpublishSource,
			actionReserveDestination,
			actionCopyEffect,
			actionVerifyCopy,
			actionReportCopy,
			actionCommitOwner,
		}
	}
	return []moveAction{
		actionBeginDestinationPublish,
		actionFinishDestinationPublish,
		actionObserveDestination,
		actionAckDestinationPublish,
		actionStartSourceCleanup,
		actionPurgeSource,
		actionReportSourcePurge,
		actionRequestAbsenceScan,
		actionObserveSource,
		actionAcceptSourceAbsence,
		actionFinishSuccess,
	}
}

func TestMove04ExhaustiveSafetyAndRecoveredLiveness(t *testing.T) {
	states := reachableMoveStates(initialMoveState(false), initialMoveState(true))
	if len(states) < 100 {
		t.Fatalf("model explored too few states: %d", len(states))
	}
	for _, state := range states {
		assertMoveInvariants(t, state)
		if state.Outcome == outcomeBlocked || state.Contradiction {
			assertMoveQuarantined(t, state)
			continue
		}
		converged, err := stableConvergence(state)
		if err != nil {
			t.Fatalf("reachable state did not converge after both nodes recovered: start=%#v: %v", state, err)
		}
		if state.AbortRequested && state.Owner == ownerSource {
			if converged.Outcome != outcomeAborted || converged.Owner != ownerSource || !converged.SourcePresent || converged.DestinationPresent {
				t.Fatalf("pre-commit abort did not recover source: start=%#v end=%#v", state, converged)
			}
			continue
		}
		if converged.Outcome != outcomeSucceeded || converged.Owner != ownerDestination || converged.SourcePresent || !converged.DestinationPresent || !converged.DestinationPublishAck {
			t.Fatalf("forward move did not converge to destination: start=%#v end=%#v", state, converged)
		}
	}
	t.Logf("checked %d reachable move states", len(states))
}

func TestMove04SourcePublicationIntentBlocksCopyUntilUnpublishReconciles(t *testing.T) {
	state := initialMoveState(false)
	state = requireMoveAction(t, state, actionBeginSourcePublish)
	if !state.SourcePublicationIntent || !state.SourcePublishInFlight {
		t.Fatalf("begin source publish did not record intent before mount: %#v", state)
	}
	state = requireMoveAction(t, state, actionStartMove)
	if _, ok := applyMove(state, actionReserveDestination); ok {
		t.Fatal("source publish intent allowed destination reservation")
	}
	if _, ok := applyMove(state, actionBeginSourcePublish); ok {
		t.Fatal("Move fence allowed a new source publication")
	}
	state = requireMoveAction(t, state, actionFinishSourcePublish)
	if _, ok := applyMove(state, actionReserveDestination); ok {
		t.Fatal("mounted source allowed destination reservation")
	}
	state = requireMoveAction(t, state, actionUnpublishSource)
	if state.SourcePublicationIntent || state.SourceMounted || state.SourcePublishInFlight {
		t.Fatalf("NodeUnpublish did not clear source publication after mount inspection: %#v", state)
	}
	if _, ok := applyMove(state, actionReserveDestination); !ok {
		t.Fatal("unpublished source did not allow destination reservation")
	}
}

func TestMove04DestinationPublishIntentIsNotActualPublicationProof(t *testing.T) {
	state := driveMove(t, initialMoveState(false),
		actionStartMove,
		actionReserveDestination,
		actionCopyEffect,
		actionVerifyCopy,
		actionReportCopy,
		actionCommitOwner,
		actionBeginDestinationPublish,
	)
	state = requireMoveAction(t, state, actionObserveDestination)
	if _, ok := applyMove(state, actionAckDestinationPublish); ok {
		t.Fatal("begin-publish intent was accepted before the scanner observed the actual destination mount")
	}
	if _, ok := applyMove(state, actionStartSourceCleanup); ok {
		t.Fatal("source cleanup started from begin-publish intent alone")
	}

	// Model a node process crash after begin-publish: the durable intent stays,
	// while no bind mount exists. A retry may bind, but cleanup still waits for
	// a later exact scanner observation.
	state.DestinationPublishInFlight = false
	state = requireMoveAction(t, state, actionBeginDestinationPublish)
	state = requireMoveAction(t, state, actionFinishDestinationPublish)
	if _, ok := applyMove(state, actionAckDestinationPublish); ok {
		t.Fatal("a pre-bind scanner observation was reused after the bind completed")
	}
	state = requireMoveAction(t, state, actionObserveDestination)
	state = requireMoveAction(t, state, actionAckDestinationPublish)
	if _, ok := applyMove(state, actionStartSourceCleanup); !ok {
		t.Fatal("exact actual destination publication proof did not authorize source cleanup")
	}
}

func TestMove04SourceAbsenceFenceRejectsStaleInvalidAndTruncatedEvidence(t *testing.T) {
	state := driveMove(t, initialMoveState(false),
		actionStartMove,
		actionReserveDestination,
		actionCopyEffect,
		actionVerifyCopy,
		actionReportCopy,
		actionCommitOwner,
		actionBeginDestinationPublish,
		actionFinishDestinationPublish,
		actionObserveDestination,
		actionAckDestinationPublish,
		actionStartSourceCleanup,
		actionPurgeSource,
		actionReportSourcePurge,
		actionRequestAbsenceScan,
	)

	stale := state
	stale.Observation = poolObservation{Generation: state.AbsenceRequiredGeneration - 1, Valid: true, Complete: true}
	if canAcceptSourceAbsence(stale) {
		t.Fatal("stale Pool observation satisfied source absence fence")
	}
	invalid := state
	invalid.Observation = poolObservation{Generation: state.AbsenceRequiredGeneration, Complete: true}
	if canAcceptSourceAbsence(invalid) {
		t.Fatal("invalid Pool observation satisfied source absence fence")
	}
	truncated := state
	truncated.Observation = poolObservation{Generation: state.AbsenceRequiredGeneration, Valid: true}
	if canAcceptSourceAbsence(truncated) {
		t.Fatal("incomplete Pool observation satisfied source absence fence")
	}
	published := state
	published.Observation = poolObservation{Generation: state.AbsenceRequiredGeneration, Valid: true, Complete: true, SourcePublished: true}
	if canAcceptSourceAbsence(published) {
		t.Fatal("published source observation satisfied source absence fence")
	}
	state = requireMoveAction(t, state, actionObserveSource)
	if !canAcceptSourceAbsence(state) {
		t.Fatal("fresh complete source absence scan was rejected")
	}
	state.PoolGeneration++
	if canAcceptSourceAbsence(state) {
		t.Fatal("a superseded source absence scan was accepted after Pool generation advanced")
	}
}

func TestMove04CapacityTracksEveryPhysicalCopyUntilPurge(t *testing.T) {
	state := initialMoveState(false)
	assertCapacityHolds(t, state, 1, 0)
	state = driveMove(t, state,
		actionStartMove,
		actionReserveDestination,
		actionCopyEffect,
		actionVerifyCopy,
		actionReportCopy,
	)
	assertCapacityHolds(t, state, 1, 1)
	state = requireMoveAction(t, state, actionCommitOwner)
	// The destination reservation becomes the owner reservation, while the
	// still-present source becomes the Move hold. Commit does not free space.
	assertCapacityHolds(t, state, 1, 1)
	state = driveMove(t, state,
		actionBeginDestinationPublish,
		actionFinishDestinationPublish,
		actionObserveDestination,
		actionAckDestinationPublish,
		actionStartSourceCleanup,
		actionPurgeSource,
	)
	// Local path removal is not enough to release the source hold. Neither of
	// the two durable facts can release it alone either.
	assertCapacityHolds(t, state, 1, 1)
	apiReceiptOnly := requireMoveAction(t, state, actionReportSourcePurge)
	assertCapacityHolds(t, apiReceiptOnly, 1, 1)
	absenceOnly := driveMove(t, state,
		actionRequestAbsenceScan,
		actionObserveSource,
		actionAcceptSourceAbsence,
	)
	assertCapacityHolds(t, absenceOnly, 1, 1)
	state = driveMove(t, apiReceiptOnly,
		actionRequestAbsenceScan,
		actionObserveSource,
		actionAcceptSourceAbsence,
	)
	assertCapacityHolds(t, state, 0, 1)

	abort := initialMoveState(false)
	abort = driveMove(t, abort,
		actionStartMove,
		actionReserveDestination,
		actionCopyEffect,
		actionRequestAbort,
		actionResumeSource,
	)
	assertCapacityHolds(t, abort, 1, 1)
	abort = driveMove(t, abort,
		actionStartDestinationCleanup,
		actionPurgeDestination,
	)
	// Abort retains its destination hold until the durable cleanup result is
	// settled and the transaction releases it.
	assertCapacityHolds(t, abort, 1, 1)
	destinationReceiptOnly := requireMoveAction(t, abort, actionReportDestinationPurge)
	assertCapacityHolds(t, destinationReceiptOnly, 1, 1)
	if _, ok := applyMove(destinationReceiptOnly, actionFinishAbort); ok {
		t.Fatal("destination purge receipt alone released the abort hold")
	}
	destinationAbsenceOnly := driveMove(t, abort,
		actionRequestDestinationScan,
		actionObserveDestination,
		actionAcceptDestinationAbsence,
	)
	assertCapacityHolds(t, destinationAbsenceOnly, 1, 1)
	if _, ok := applyMove(destinationAbsenceOnly, actionFinishAbort); ok {
		t.Fatal("destination absence alone released the abort hold")
	}
	abort = driveMove(t, destinationReceiptOnly,
		actionRequestDestinationScan,
		actionObserveDestination,
		actionAcceptDestinationAbsence,
		actionFinishAbort,
	)
	assertCapacityHolds(t, abort, 1, 0)
}

func TestMove04PartialCopyCannotBeReportedOrCommitted(t *testing.T) {
	state := initialMoveState(false)
	state = driveMove(t, state,
		actionStartMove,
		actionReserveDestination,
		actionCopyEffect,
	)
	for _, action := range []moveAction{actionReportCopy, actionCommitOwner} {
		if _, ok := applyMove(state, action); ok {
			t.Fatalf("partial copy allowed %q", action)
		}
	}
	state = driveMove(t, state, actionVerifyCopy, actionReportCopy, actionCommitOwner)
	if state.Owner != ownerDestination || state.DestinationPartial {
		t.Fatalf("verified copy did not reach the commit point: %#v", state)
	}
}

func TestMove04CleanupRecoversWhenDeleteEffectPrecedesReceipt(t *testing.T) {
	abort := initialMoveState(false)
	abort = driveMove(t, abort,
		actionStartMove,
		actionReserveDestination,
		actionCopyEffect,
		actionRequestAbort,
		actionResumeSource,
		actionStartDestinationCleanup,
		actionDestinationDeleteEffect,
	)
	assertCapacityHolds(t, abort, 1, 1)
	abort = requireMoveAction(t, abort, actionPurgeDestination)
	convergedAbort, err := stableConvergence(abort)
	if err != nil || convergedAbort.Outcome != outcomeAborted {
		t.Fatalf("destination cleanup crash window did not converge: end=%#v err=%v", convergedAbort, err)
	}

	forward := initialMoveState(false)
	forward = driveMove(t, forward,
		actionStartMove,
		actionReserveDestination,
		actionCopyEffect,
		actionVerifyCopy,
		actionReportCopy,
		actionCommitOwner,
		actionBeginDestinationPublish,
		actionFinishDestinationPublish,
		actionObserveDestination,
		actionAckDestinationPublish,
		actionStartSourceCleanup,
		actionSourceDeleteEffect,
	)
	assertCapacityHolds(t, forward, 1, 1)
	forward = requireMoveAction(t, forward, actionPurgeSource)
	convergedForward, err := stableConvergence(forward)
	if err != nil || convergedForward.Outcome != outcomeSucceeded {
		t.Fatalf("source cleanup crash window did not converge: end=%#v err=%v", convergedForward, err)
	}
}

func TestMove04ContradictionHasNoDestructiveTransition(t *testing.T) {
	state := initialMoveState(false)
	state.MoveActive = true
	state.Fenced = true
	state.Contradiction = true
	state.Outcome = outcomeBlocked
	assertMoveQuarantined(t, state)
}

func TestMove04VolumeDeleteSettlesOnTheCorrectSideOfCommit(t *testing.T) {
	for _, state := range reachableMoveStates(initialMoveState(false), initialMoveState(true)) {
		if state.Outcome != outcomeActive || !state.MoveActive {
			continue
		}
		startedOwner := state.Owner
		if startedOwner == ownerSource {
			state.AbortRequested = true
		}
		converged, err := stableConvergence(state)
		if err != nil {
			t.Fatalf("move did not settle before Volume deletion: start=%#v: %v", state, err)
		}
		switch startedOwner {
		case ownerSource:
			if converged.Outcome != outcomeAborted || converged.Owner != ownerSource {
				t.Fatalf("pre-commit Volume deletion crossed the commit point: start=%#v end=%#v", state, converged)
			}
		case ownerDestination:
			if converged.Outcome != outcomeSucceeded || converged.Owner != ownerDestination {
				t.Fatalf("post-commit Volume deletion rolled back authority: start=%#v end=%#v", state, converged)
			}
		}
	}
}

func assertMoveInvariants(t *testing.T, state moveState) {
	t.Helper()
	if state.Owner == ownerSource && !state.SourcePresent {
		t.Fatalf("source authority has no source copy: %#v", state)
	}
	if state.Owner == ownerDestination && !state.DestinationPresent {
		t.Fatalf("destination authority has no destination copy: %#v", state)
	}
	if state.SourceMounted && state.DestinationMounted {
		t.Fatalf("source and destination are both mounted: %#v", state)
	}
	if state.DestinationMounted && state.Owner != ownerDestination {
		t.Fatalf("non-owner destination is mounted: %#v", state)
	}
	forwardCopyMayExist := !state.AbortRequested && (state.DestinationReserved || state.DestinationPresent || state.CopyLocalReceipt || state.CopyAPIReceipt)
	if (state.Owner == ownerDestination || forwardCopyMayExist) &&
		(state.SourcePublicationIntent || state.SourceMounted || state.SourcePublishInFlight) {
		t.Fatalf("forward authority coexists with a source publication: %#v", state)
	}
	if state.Owner == ownerDestination && (!state.CopyAPIReceipt) {
		t.Fatalf("owner committed without durable prerequisites: %#v", state)
	}
	if state.DestinationPartial && (state.CopyLocalReceipt || state.CopyAPIReceipt || state.Owner == ownerDestination || state.DestinationPublishAck) {
		t.Fatalf("partial destination was treated as a verified copy: %#v", state)
	}
	if state.CopyAPIReceipt && (!state.CopyLocalReceipt || state.DestinationPartial) {
		t.Fatalf("API copy receipt lacks a verified local copy: %#v", state)
	}
	if state.DestinationPublishAck && (state.Owner != ownerDestination || !state.DestinationPresent || !state.DestinationMounted) {
		t.Fatalf("destination publish ack has no exact mounted owner: %#v", state)
	}
	if state.SourcePurgeLocalReceipt && (state.Owner != ownerDestination || !state.DestinationPublishAck || state.SourcePresent) {
		t.Fatalf("source purge happened without post-commit authority: %#v", state)
	}
	if state.SourcePurgeLocalReceipt && !state.SourceCleanupIntent || state.SourcePurgeAPIReceipt && !state.SourcePurgeLocalReceipt {
		t.Fatalf("source purge receipts are not causally linked: %#v", state)
	}
	if state.DestinationPurgeLocalReceipt && (state.Owner != ownerSource || !state.AbortRequested || state.DestinationPresent) {
		t.Fatalf("destination purge happened outside pre-commit abort: %#v", state)
	}
	if state.DestinationPurgeLocalReceipt && !state.DestinationCleanupIntent || state.DestinationPurgeAPIReceipt && !state.DestinationPurgeLocalReceipt {
		t.Fatalf("destination purge receipts are not causally linked: %#v", state)
	}
	if state.DestinationAbsenceAck && (!state.DestinationPurgeLocalReceipt || state.DestinationAbsenceGeneration == 0 ||
		!state.DestinationObservation.Valid || !state.DestinationObservation.Complete ||
		state.DestinationObservation.Generation != state.DestinationPoolGeneration ||
		state.DestinationObservation.Generation < state.DestinationAbsenceGeneration ||
		state.DestinationObservation.Present || state.DestinationObservation.Published) {
		t.Fatalf("destination absence acknowledgement lacks exact fresh evidence: %#v", state)
	}
	if state.Outcome == outcomeSucceeded && (state.MoveActive || state.Owner != ownerDestination || state.SourcePresent || !state.SourceAbsenceAcknowledged) {
		t.Fatalf("invalid succeeded state: %#v", state)
	}
	if state.Outcome == outcomeAborted && (state.MoveActive || state.Owner != ownerSource || !state.SourcePresent || state.DestinationPresent || state.Fenced) {
		t.Fatalf("invalid aborted state: %#v", state)
	}
	if state.Outcome == outcomeAborted && state.DestinationCleanupIntent && (!state.DestinationPurgeAPIReceipt || !state.DestinationAbsenceAck) {
		t.Fatalf("abort released destination cleanup before receipt and fresh absence: %#v", state)
	}
	sourceHolds, destinationHolds := capacityHolds(state)
	if sourceHolds > 1 || destinationHolds > 1 || state.SourcePresent && sourceHolds == 0 || state.DestinationPresent && destinationHolds == 0 {
		t.Fatalf("capacity does not conservatively account for every physical copy: source=%d destination=%d state=%#v", sourceHolds, destinationHolds, state)
	}
}

func capacityHolds(state moveState) (source, destination int) {
	// ShiftPVVolume owns exactly the current owner's reservation.
	if state.Owner == ownerSource {
		source++
	}
	if state.Owner == ownerDestination {
		destination++
	}
	// Before commit the Move reserves its selected destination. The hold stays
	// through abort cleanup and is released only when the Move settles.
	if state.MoveActive && state.Owner == ownerSource && state.DestinationReserved {
		destination++
	}
	// After commit the same Move accounts for the retained source until both the
	// durable purge receipt and a fresh post-purge absence proof exist. Neither
	// local path absence nor either fact on its own can release capacity.
	if state.MoveActive && state.Owner == ownerDestination && !(state.SourcePurgeAPIReceipt && state.SourceAbsenceAcknowledged) {
		source++
	}
	return source, destination
}

func assertMoveQuarantined(t *testing.T, state moveState) {
	t.Helper()
	if state.Outcome != outcomeBlocked || !state.Contradiction {
		t.Fatalf("contradictory Move did not enter Blocked: %#v", state)
	}
	if state.Owner == ownerSource && !state.SourcePresent || state.Owner == ownerDestination && !state.DestinationPresent {
		t.Fatalf("Blocked lost the authoritative copy: %#v", state)
	}
	for _, action := range allMoveActions {
		if _, ok := applyMove(state, action); ok {
			t.Fatalf("Blocked allowed transition %q: %#v", action, state)
		}
	}
}

func assertCapacityHolds(t *testing.T, state moveState, sourceWant, destinationWant int) {
	t.Helper()
	sourceGot, destinationGot := capacityHolds(state)
	if sourceGot != sourceWant || destinationGot != destinationWant {
		t.Fatalf("capacity holds source=%d destination=%d, want source=%d destination=%d: %#v", sourceGot, destinationGot, sourceWant, destinationWant, state)
	}
}

func requireMoveAction(t *testing.T, state moveState, action moveAction) moveState {
	t.Helper()
	next, ok := applyMove(state, action)
	if !ok {
		t.Fatalf("action %q was not accepted from %#v", action, state)
	}
	return next
}

func driveMove(t *testing.T, state moveState, actions ...moveAction) moveState {
	t.Helper()
	for _, action := range actions {
		state = requireMoveAction(t, state, action)
	}
	return state
}
