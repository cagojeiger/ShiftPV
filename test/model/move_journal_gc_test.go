package model

import "testing"

type moveJournalGCState struct {
	terminal          bool
	cleanupSettled    bool
	capacityReleased  bool
	protectionRemoved bool
	otherFinalizer    bool
	retentionElapsed  bool
	activeReference   bool
	validIdentity     bool
	journalPresent    bool
	copyDataPresent   bool
}

func applyJournalGC(state moveJournalGCState) moveJournalGCState {
	allowed := state.journalPresent && state.terminal && state.cleanupSettled && state.capacityReleased && state.protectionRemoved &&
		!state.otherFinalizer && state.retentionElapsed && !state.activeReference && state.validIdentity
	if allowed {
		state.journalPresent = false
	}
	return state
}

func TestMoveJournalGCExhaustiveSafety(t *testing.T) {
	for bits := 0; bits < 1<<10; bits++ {
		state := moveJournalGCState{
			terminal: bits&(1<<0) != 0, cleanupSettled: bits&(1<<1) != 0,
			capacityReleased: bits&(1<<2) != 0, protectionRemoved: bits&(1<<3) != 0,
			otherFinalizer: bits&(1<<4) != 0, retentionElapsed: bits&(1<<5) != 0,
			activeReference: bits&(1<<6) != 0, validIdentity: bits&(1<<7) != 0,
			journalPresent: bits&(1<<8) != 0, copyDataPresent: bits&(1<<9) != 0,
		}
		next := applyJournalGC(state)
		allGates := state.journalPresent && state.terminal && state.cleanupSettled && state.capacityReleased && state.protectionRemoved &&
			!state.otherFinalizer && state.retentionElapsed && !state.activeReference && state.validIdentity
		if (state.journalPresent && !next.journalPresent) != allGates {
			t.Fatalf("journal GC crossed a gate: state=%#v next=%#v", state, next)
		}
		if next.copyDataPresent != state.copyDataPresent {
			t.Fatalf("metadata GC changed copy data: state=%#v next=%#v", state, next)
		}
	}
}
