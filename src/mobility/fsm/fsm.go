package fsm

import "fmt"

type Phase string

const (
	PhasePending                      Phase = "Pending"
	PhaseLocking                      Phase = "Locking"
	PhaseEvicting                     Phase = "Evicting"
	PhaseWaitingForUnpublish          Phase = "WaitingForUnpublish"
	PhaseWaitingForReplacement        Phase = "WaitingForReplacement"
	PhaseWaitingForDestination        Phase = "WaitingForDestination"
	PhaseWaitingForCapacity           Phase = "WaitingForCapacity"
	PhaseCopying                      Phase = "Copying"
	PhasePromoting                    Phase = "Promoting"
	PhaseCommitting                   Phase = "Committing"
	PhaseReleasingDestination         Phase = "ReleasingDestination"
	PhaseWaitingForDestinationPublish Phase = "WaitingForDestinationPublish"
	PhaseCleaningSource               Phase = "CleaningSource"
	PhaseCompleting                   Phase = "Completing"
	PhaseSucceeded                    Phase = "Succeeded"
	PhaseBlocked                      Phase = "Blocked"
)

type Action string

const (
	ActionWait             Action = "Wait"
	ActionLockVolume       Action = "LockVolume"
	ActionEvictConsumer    Action = "EvictConsumer"
	ActionEnsurePlacement  Action = "EnsurePlacement"
	ActionEnsureCapacity   Action = "EnsureCapacity"
	ActionDeletePlacement  Action = "DeletePlacement"
	ActionReleasePlacement Action = "ReleasePlacement"
	ActionEnsureCopy       Action = "EnsureCopy"
	ActionEnsurePromotion  Action = "EnsurePromotion"
	ActionCommitOwner      Action = "CommitOwner"
	ActionEnsureCleanup    Action = "EnsureCleanup"
	ActionConfirmCleanup   Action = "ConfirmCleanup"
	ActionMarkSucceeded    Action = "MarkSucceeded"
	ActionMarkBlocked      Action = "MarkBlocked"
)

type Observation struct {
	PreconditionsValid     bool
	PreflightDeferred      bool
	UnsafeReason           string
	SourceHealthy          bool
	SourceAuthorityInvalid bool
	VolumeLocked           bool
	ConsumerExists         bool
	EvictionRequested      bool
	PublishedOnSource      bool
	ReplacementExists      bool
	ReplacementHeld        bool
	PlacementExists        bool
	DestinationScheduled   bool
	DestinationBlocked     bool
	DestinationUnavailable bool
	CapacityApproved       bool
	CapacityBlocked        bool
	CopyComplete           bool
	CopyFailed             bool
	PromotionComplete      bool
	PromotionFailed        bool
	OwnerCommitted         bool
	PublishedOnDestination bool
	CleanupComplete        bool
	CleanupFailed          bool
	CompletionReady        bool
}

type Decision struct {
	Next   Phase
	Action Action
	Reason string
}

func Decide(current Phase, observation Observation) (Decision, error) {
	if !known(current) {
		return Decision{}, fmt.Errorf("unknown mobility phase %q", current)
	}
	if terminal(current) {
		return Decision{Next: current, Action: ActionWait}, nil
	}
	if current == PhaseCompleting {
		if observation.CompletionReady {
			return transition(current, PhaseSucceeded, ActionMarkSucceeded, "")
		}
		return transition(current, current, ActionWait, "CompletionAuthorityMismatch")
	}
	if observation.SourceAuthorityInvalid {
		return blockedWith(observation, "SourceAuthorityInvalid"), nil
	}
	if beforeCommit(current) && !observation.OwnerCommitted && !observation.SourceHealthy {
		return blockedWith(observation, "SourceUnavailable"), nil
	}
	// A related but looser pre-eviction window is derived from Move status by
	// preEviction in src/mobility/controller/preflight.go; they are not the same
	// predicate, so do not unify them blindly.
	if observation.PreflightDeferred && (current == PhasePending || current == PhaseLocking ||
		(current == PhaseEvicting && !observation.EvictionRequested)) {
		return transition(current, current, ActionWait, observation.UnsafeReason)
	}

	switch current {
	case PhasePending:
		if !observation.PreconditionsValid {
			return transition(current, current, ActionWait, reasonOr(observation.UnsafeReason, "PreconditionFailed"))
		}
		return transition(current, PhaseLocking, ActionLockVolume, "")
	case PhaseLocking:
		if observation.VolumeLocked {
			return transition(current, PhaseEvicting, ActionEvictConsumer, "")
		}
		return transition(current, PhaseLocking, ActionLockVolume, "")
	case PhaseEvicting:
		if !observation.ConsumerExists {
			return transition(current, PhaseWaitingForUnpublish, ActionWait, "")
		}
		if observation.EvictionRequested {
			return transition(current, PhaseEvicting, ActionWait, "")
		}
		return transition(current, PhaseEvicting, ActionEvictConsumer, "")
	case PhaseWaitingForUnpublish:
		if observation.PublishedOnSource {
			return transition(current, current, ActionWait, "")
		}
		return transition(current, PhaseWaitingForReplacement, ActionWait, "")
	case PhaseWaitingForReplacement:
		if !observation.ReplacementExists {
			return transition(current, current, ActionWait, "")
		}
		if !observation.ReplacementHeld {
			return blockedWith(observation, "PlacementHoldLost"), nil
		}
		return transition(current, PhaseWaitingForDestination, ActionEnsurePlacement, "")
	case PhaseWaitingForDestination:
		// Intentionally not destinationGuard: no placement exists yet, so an
		// unavailable destination is not a wait condition here and a blocked one
		// reports DestinationUnavailable rather than InvalidDestination.
		if observation.DestinationBlocked {
			return blockedWith(observation, "DestinationUnavailable"), nil
		}
		if !observation.ReplacementExists {
			return transition(current, current, ActionWait, "")
		}
		if !observation.ReplacementHeld {
			return blockedWith(observation, "PlacementHoldLost"), nil
		}
		if !observation.DestinationScheduled {
			return transition(current, current, ActionEnsurePlacement, "")
		}
		if observation.DestinationUnavailable {
			return transition(current, PhaseWaitingForCapacity, ActionWait, "DestinationUnavailable")
		}
		return transition(current, PhaseWaitingForCapacity, ActionEnsureCapacity, "")
	case PhaseWaitingForCapacity:
		if decision, guarded, err := destinationGuard(current, observation); guarded {
			return decision, err
		}
		if observation.CapacityBlocked {
			return blockedWith(observation, "DestinationCapacityInsufficient"), nil
		}
		if observation.CapacityApproved {
			return transition(current, PhaseCopying, ActionEnsureCopy, "")
		}
		return transition(current, current, ActionEnsureCapacity, "")
	case PhaseCopying:
		if decision, guarded, err := destinationGuard(current, observation); guarded {
			return decision, err
		}
		if observation.CopyFailed {
			return blockedWith(observation, "CopyFailed"), nil
		}
		if decision, guarded, err := placementGuard(current, observation); guarded {
			return decision, err
		}
		if observation.CopyComplete {
			return transition(current, PhasePromoting, ActionEnsurePromotion, "")
		}
		return transition(current, current, ActionEnsureCopy, "")
	case PhasePromoting:
		if decision, guarded, err := destinationGuard(current, observation); guarded {
			return decision, err
		}
		if observation.PromotionFailed {
			return blockedWith(observation, "PromotionFailed"), nil
		}
		if decision, guarded, err := placementGuard(current, observation); guarded {
			return decision, err
		}
		if observation.PromotionComplete {
			return transition(current, PhaseCommitting, ActionCommitOwner, "")
		}
		return transition(current, current, ActionEnsurePromotion, "")
	case PhaseCommitting:
		if observation.OwnerCommitted {
			// Authority already moved, so a blocked destination can no longer
			// invalidate it; only an unavailable one delays releasing the hold.
			if observation.DestinationUnavailable {
				return transition(current, current, ActionWait, "DestinationUnavailable")
			}
			return transition(current, PhaseReleasingDestination, ActionDeletePlacement, "")
		}
		if decision, guarded, err := destinationGuard(current, observation); guarded {
			return decision, err
		}
		if decision, guarded, err := placementGuard(current, observation); guarded {
			return decision, err
		}
		return transition(current, current, ActionCommitOwner, "")
	case PhaseReleasingDestination:
		if observation.DestinationUnavailable {
			return transition(current, current, ActionWait, "DestinationUnavailable")
		}
		if observation.PlacementExists {
			return transition(current, current, ActionDeletePlacement, "")
		}
		if observation.ReplacementExists && observation.ReplacementHeld {
			return transition(current, PhaseWaitingForDestinationPublish, ActionReleasePlacement, "")
		}
		return transition(current, PhaseWaitingForDestinationPublish, ActionWait, "")
	case PhaseWaitingForDestinationPublish:
		if observation.CleanupFailed {
			return blockedWith(observation, "CleanupFailed"), nil
		}
		if observation.DestinationUnavailable {
			return transition(current, current, ActionWait, "DestinationUnavailable")
		}
		if !observation.PublishedOnDestination {
			return transition(current, current, ActionWait, "")
		}
		return transition(current, PhaseCleaningSource, ActionEnsureCleanup, "")
	case PhaseCleaningSource:
		if observation.DestinationUnavailable {
			return transition(current, current, ActionWait, "DestinationUnavailable")
		}
		if observation.CleanupFailed {
			return blockedWith(observation, "CleanupFailed"), nil
		}
		if observation.CleanupComplete {
			return transition(current, PhaseCompleting, ActionConfirmCleanup, "")
		}
		return transition(current, current, ActionEnsureCleanup, "")
	default:
		return Decision{}, fmt.Errorf("mobility phase %q has no decision rule", current)
	}
}

func ValidateTransition(from, to Phase) error {
	if !known(from) || !known(to) {
		return fmt.Errorf("transition contains unknown phase: %q -> %q", from, to)
	}
	if terminal(from) && from != to {
		return fmt.Errorf("terminal phase %q cannot transition to %q", from, to)
	}
	for _, allowed := range allowedTransitions[from] {
		if allowed == to {
			return nil
		}
	}
	return fmt.Errorf("illegal mobility transition %q -> %q", from, to)
}

var allowedTransitions = map[Phase][]Phase{
	PhasePending:                      {PhasePending, PhaseLocking, PhaseBlocked},
	PhaseLocking:                      {PhaseLocking, PhaseEvicting, PhaseBlocked},
	PhaseEvicting:                     {PhaseEvicting, PhaseWaitingForUnpublish, PhaseBlocked},
	PhaseWaitingForUnpublish:          {PhaseWaitingForUnpublish, PhaseWaitingForReplacement, PhaseBlocked},
	PhaseWaitingForReplacement:        {PhaseWaitingForReplacement, PhaseWaitingForDestination, PhaseBlocked},
	PhaseWaitingForDestination:        {PhaseWaitingForDestination, PhaseWaitingForCapacity, PhaseBlocked},
	PhaseWaitingForCapacity:           {PhaseWaitingForCapacity, PhaseCopying, PhaseBlocked},
	PhaseCopying:                      {PhaseCopying, PhasePromoting, PhaseBlocked},
	PhasePromoting:                    {PhasePromoting, PhaseCommitting, PhaseBlocked},
	PhaseCommitting:                   {PhaseCommitting, PhaseReleasingDestination, PhaseBlocked},
	PhaseReleasingDestination:         {PhaseReleasingDestination, PhaseWaitingForDestinationPublish, PhaseBlocked},
	PhaseWaitingForDestinationPublish: {PhaseWaitingForDestinationPublish, PhaseCleaningSource, PhaseBlocked},
	PhaseCleaningSource:               {PhaseCleaningSource, PhaseCompleting, PhaseBlocked},
	PhaseCompleting:                   {PhaseCompleting, PhaseSucceeded},
	PhaseSucceeded:                    {PhaseSucceeded},
	PhaseBlocked:                      {PhaseBlocked},
}

func transition(from, to Phase, action Action, reason string) (Decision, error) {
	if err := ValidateTransition(from, to); err != nil {
		return Decision{}, err
	}
	return Decision{Next: to, Action: action, Reason: reason}, nil
}

// destinationGuard is the prelude shared by every phase that still depends on a
// live destination while source authority is unchanged: an unavailable
// destination only pauses the transaction, a blocked one fails it closed. It
// reports whether it owns the decision so callers keep their phase-specific
// checks in their original order around it.
func destinationGuard(current Phase, observation Observation) (Decision, bool, error) {
	if observation.DestinationUnavailable {
		decision, err := transition(current, current, ActionWait, "DestinationUnavailable")
		return decision, true, err
	}
	if observation.DestinationBlocked {
		return blockedWith(observation, "InvalidDestination"), true, nil
	}
	return Decision{}, false, nil
}

// placementGuard re-drives the reservation Pod whenever a phase that needs a
// scheduled destination has lost it.
func placementGuard(current Phase, observation Observation) (Decision, bool, error) {
	if !observation.PlacementExists || !observation.DestinationScheduled {
		decision, err := transition(current, current, ActionEnsurePlacement, "")
		return decision, true, err
	}
	return Decision{}, false, nil
}

func blocked(reason string) Decision {
	return Decision{Next: PhaseBlocked, Action: ActionMarkBlocked, Reason: reason}
}

// blockedWith prefers the observation's own diagnosis over the phase fallback.
func blockedWith(observation Observation, fallback string) Decision {
	return blocked(reasonOr(observation.UnsafeReason, fallback))
}

func known(phase Phase) bool {
	_, exists := allowedTransitions[phase]
	return exists
}

func terminal(phase Phase) bool {
	return phase == PhaseSucceeded || phase == PhaseBlocked
}

func beforeCommit(phase Phase) bool {
	switch phase {
	case PhasePending, PhaseLocking, PhaseEvicting, PhaseWaitingForUnpublish,
		PhaseWaitingForReplacement, PhaseWaitingForDestination, PhaseWaitingForCapacity, PhaseCopying, PhasePromoting,
		PhaseCommitting:
		return true
	default:
		return false
	}
}

func reasonOr(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
