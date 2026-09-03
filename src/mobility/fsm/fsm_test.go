package fsm

import "testing"

func TestHappyPathIsClosed(t *testing.T) {
	phase := PhasePending
	observations := []Observation{
		{PreconditionsValid: true, SourceHealthy: true},
		{SourceHealthy: true, VolumeLocked: true},
		{SourceHealthy: true, ConsumerExists: false},
		{SourceHealthy: true, PublishedOnSource: false},
		{SourceHealthy: true, ReplacementExists: true, ReplacementHeld: true},
		{SourceHealthy: true, DestinationScheduled: true},
		{SourceHealthy: true, CopyComplete: true},
		{SourceHealthy: true, PromotionComplete: true},
		{OwnerCommitted: true},
		{PublishedOnDestination: true},
		{CleanupComplete: true},
	}
	wantActions := []Action{
		ActionLockVolume,
		ActionEvictConsumer,
		ActionWait,
		ActionWait,
		ActionReleasePlacement,
		ActionEnsureCopy,
		ActionEnsurePromotion,
		ActionCommitOwner,
		ActionWait,
		ActionEnsureCleanup,
		ActionMarkSucceeded,
	}
	for index, observation := range observations {
		decision, err := Decide(phase, observation)
		if err != nil {
			t.Fatalf("step %d: %v", index, err)
		}
		if decision.Action != wantActions[index] {
			t.Fatalf("step %d action = %q, want %q", index, decision.Action, wantActions[index])
		}
		phase = decision.Next
	}
	if phase != PhaseSucceeded {
		t.Fatalf("final phase = %q, want %q", phase, PhaseSucceeded)
	}
	decision, err := Decide(phase, Observation{})
	if err != nil || decision.Next != PhaseSucceeded || decision.Action != ActionWait {
		t.Fatalf("terminal decision = %#v, %v", decision, err)
	}
}

func TestEveryPhaseHasAClosedDecision(t *testing.T) {
	for phase := range allowedTransitions {
		observation := Observation{PreconditionsValid: true, SourceHealthy: true}
		decision, err := Decide(phase, observation)
		if err != nil {
			t.Errorf("phase %q: %v", phase, err)
			continue
		}
		if err := ValidateTransition(phase, decision.Next); err != nil {
			t.Errorf("phase %q returned open transition: %v", phase, err)
		}
		if decision.Action == "" {
			t.Errorf("phase %q returned no action", phase)
		}
	}
}

func TestTransitionGraphIsClosedAndEveryNonTerminalCanFinish(t *testing.T) {
	for phase, targets := range allowedTransitions {
		if len(targets) == 0 {
			t.Errorf("phase %q has no outgoing transition", phase)
			continue
		}
		for _, target := range targets {
			if !known(target) {
				t.Errorf("phase %q points to unknown phase %q", phase, target)
			}
		}
		if terminal(phase) {
			if len(targets) != 1 || targets[0] != phase {
				t.Errorf("terminal phase %q is not an isolated self-loop: %#v", phase, targets)
			}
			continue
		}
		if !canReachTerminal(phase) {
			t.Errorf("non-terminal phase %q cannot reach a terminal phase", phase)
		}
	}
}

func canReachTerminal(start Phase) bool {
	visited := map[Phase]bool{}
	queue := []Phase{start}
	for len(queue) > 0 {
		phase := queue[0]
		queue = queue[1:]
		if terminal(phase) {
			return true
		}
		if visited[phase] {
			continue
		}
		visited[phase] = true
		for _, target := range allowedTransitions[phase] {
			if !visited[target] {
				queue = append(queue, target)
			}
		}
	}
	return false
}

func TestSafetyFailuresBlockBeforeCommit(t *testing.T) {
	for _, phase := range []Phase{
		PhasePending, PhaseLocking, PhaseEvicting, PhaseWaitingForUnpublish,
		PhaseWaitingForReplacement, PhaseWaitingForDestination, PhaseCopying, PhasePromoting,
		PhaseCommitting,
	} {
		decision, err := Decide(phase, Observation{UnsafeReason: "SourceUnavailable"})
		if err != nil {
			t.Fatal(err)
		}
		if decision.Next != PhaseBlocked || decision.Action != ActionMarkBlocked {
			t.Errorf("phase %q decision = %#v", phase, decision)
		}
	}
}

func TestCommittedOwnerSurvivesCommittingCrashWindow(t *testing.T) {
	decision, err := Decide(PhaseCommitting, Observation{
		OwnerCommitted: true,
		SourceHealthy:  false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Next != PhaseWaitingForDestinationPublish || decision.Action != ActionWait {
		t.Fatalf("committed crash-window decision = %#v", decision)
	}
}

func TestBindingLossBlocksAfterOwnerCommit(t *testing.T) {
	decision, err := Decide(PhaseWaitingForDestinationPublish, Observation{
		OwnerCommitted:         true,
		SourceAuthorityInvalid: true,
		UnsafeReason:           "VolumeBindingMissing",
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Next != PhaseBlocked || decision.Reason != "VolumeBindingMissing" {
		t.Fatalf("post-commit binding-loss decision = %#v", decision)
	}
}

func TestActionFailuresAndDestinationFailureBlock(t *testing.T) {
	tests := []struct {
		phase       Phase
		observation Observation
		reason      string
	}{
		{PhasePending, Observation{SourceHealthy: true, UnsafeReason: "UnsupportedConsumer"}, "UnsupportedConsumer"},
		{PhaseWaitingForDestination, Observation{SourceHealthy: true, DestinationBlocked: true}, "DestinationUnavailable"},
		{PhaseCopying, Observation{SourceHealthy: true, CopyFailed: true}, "CopyFailed"},
		{PhasePromoting, Observation{SourceHealthy: true, PromotionFailed: true}, "PromotionFailed"},
		{PhaseCleaningSource, Observation{CleanupFailed: true}, "CleanupFailed"},
		{PhaseLocking, Observation{SourceHealthy: true, SourceAuthorityInvalid: true, UnsafeReason: "OwnerMismatch"}, "OwnerMismatch"},
	}
	for _, test := range tests {
		decision, err := Decide(test.phase, test.observation)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Next != PhaseBlocked || decision.Reason != test.reason {
			t.Errorf("phase %q decision = %#v", test.phase, decision)
		}
	}
}

func TestWaitPathsStayInTheCurrentPhase(t *testing.T) {
	tests := []struct {
		phase       Phase
		observation Observation
		action      Action
	}{
		{PhaseLocking, Observation{SourceHealthy: true}, ActionLockVolume},
		{PhaseEvicting, Observation{SourceHealthy: true, ConsumerExists: true, EvictionRequested: true}, ActionWait},
		{PhaseWaitingForUnpublish, Observation{SourceHealthy: true, PublishedOnSource: true}, ActionWait},
		{PhaseWaitingForReplacement, Observation{SourceHealthy: true}, ActionWait},
		{PhaseWaitingForDestination, Observation{SourceHealthy: true}, ActionWait},
		{PhaseCopying, Observation{SourceHealthy: true}, ActionEnsureCopy},
		{PhasePromoting, Observation{SourceHealthy: true}, ActionEnsurePromotion},
		{PhaseCommitting, Observation{SourceHealthy: true}, ActionCommitOwner},
		{PhaseWaitingForDestinationPublish, Observation{}, ActionWait},
		{PhaseCleaningSource, Observation{}, ActionEnsureCleanup},
	}
	for _, test := range tests {
		decision, err := Decide(test.phase, test.observation)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Next != test.phase || decision.Action != test.action {
			t.Errorf("phase %q decision = %#v", test.phase, decision)
		}
	}
}

func TestTransitionValidation(t *testing.T) {
	if err := ValidateTransition(PhasePending, PhaseCopying); err == nil {
		t.Fatal("illegal transition was accepted")
	}
	if err := ValidateTransition(PhaseSucceeded, PhasePending); err == nil {
		t.Fatal("terminal transition was accepted")
	}
	if _, err := Decide(Phase("Mystery"), Observation{}); err == nil {
		t.Fatal("unknown phase was accepted")
	}
}
