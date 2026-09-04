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
	PhaseCopying                      Phase = "Copying"
	PhasePromoting                    Phase = "Promoting"
	PhaseCommitting                   Phase = "Committing"
	PhaseWaitingForDestinationPublish Phase = "WaitingForDestinationPublish"
	PhaseCleaningSource               Phase = "CleaningSource"
	PhaseSucceeded                    Phase = "Succeeded"
	PhaseBlocked                      Phase = "Blocked"
)

type Action string

const (
	ActionWait             Action = "Wait"
	ActionLockVolume       Action = "LockVolume"
	ActionEvictConsumer    Action = "EvictConsumer"
	ActionReleasePlacement Action = "ReleasePlacement"
	ActionEnsureCopy       Action = "EnsureCopy"
	ActionEnsurePromotion  Action = "EnsurePromotion"
	ActionCommitOwner      Action = "CommitOwner"
	ActionEnsureCleanup    Action = "EnsureCleanup"
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
	DestinationScheduled   bool
	DestinationBlocked     bool
	DestinationUnavailable bool
	CopyComplete           bool
	CopyFailed             bool
	PromotionComplete      bool
	PromotionFailed        bool
	OwnerCommitted         bool
	PublishedOnDestination bool
	CleanupComplete        bool
	CleanupFailed          bool
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
	if observation.SourceAuthorityInvalid {
		return blocked(current, reasonOr(observation.UnsafeReason, "SourceAuthorityInvalid")), nil
	}
	if beforeCommit(current) && !observation.OwnerCommitted && !observation.SourceHealthy {
		return blocked(current, reasonOr(observation.UnsafeReason, "SourceUnavailable")), nil
	}
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
			return transition(current, PhaseWaitingForDestination, ActionWait, "")
		}
		return transition(current, PhaseWaitingForDestination, ActionReleasePlacement, "")
	case PhaseWaitingForDestination:
		if observation.DestinationBlocked {
			return blocked(current, reasonOr(observation.UnsafeReason, "DestinationUnavailable")), nil
		}
		if !observation.DestinationScheduled {
			return transition(current, current, ActionWait, "")
		}
		return transition(current, PhaseCopying, ActionEnsureCopy, "")
	case PhaseCopying:
		if observation.DestinationUnavailable {
			return transition(current, current, ActionWait, "DestinationUnavailable")
		}
		if observation.CopyFailed {
			return blocked(current, reasonOr(observation.UnsafeReason, "CopyFailed")), nil
		}
		if observation.CopyComplete {
			return transition(current, PhasePromoting, ActionEnsurePromotion, "")
		}
		return transition(current, current, ActionEnsureCopy, "")
	case PhasePromoting:
		if observation.DestinationUnavailable {
			return transition(current, current, ActionWait, "DestinationUnavailable")
		}
		if observation.PromotionFailed {
			return blocked(current, reasonOr(observation.UnsafeReason, "PromotionFailed")), nil
		}
		if observation.PromotionComplete {
			return transition(current, PhaseCommitting, ActionCommitOwner, "")
		}
		return transition(current, current, ActionEnsurePromotion, "")
	case PhaseCommitting:
		if observation.OwnerCommitted {
			return transition(current, PhaseWaitingForDestinationPublish, ActionWait, "")
		}
		if observation.DestinationUnavailable {
			return transition(current, current, ActionWait, "DestinationUnavailable")
		}
		return transition(current, current, ActionCommitOwner, "")
	case PhaseWaitingForDestinationPublish:
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
			return blocked(current, reasonOr(observation.UnsafeReason, "CleanupFailed")), nil
		}
		if observation.CleanupComplete {
			return transition(current, PhaseSucceeded, ActionMarkSucceeded, "")
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
	PhaseWaitingForDestination:        {PhaseWaitingForDestination, PhaseCopying, PhaseBlocked},
	PhaseCopying:                      {PhaseCopying, PhasePromoting, PhaseBlocked},
	PhasePromoting:                    {PhasePromoting, PhaseCommitting, PhaseBlocked},
	PhaseCommitting:                   {PhaseCommitting, PhaseWaitingForDestinationPublish, PhaseBlocked},
	PhaseWaitingForDestinationPublish: {PhaseWaitingForDestinationPublish, PhaseCleaningSource, PhaseBlocked},
	PhaseCleaningSource:               {PhaseCleaningSource, PhaseSucceeded, PhaseBlocked},
	PhaseSucceeded:                    {PhaseSucceeded},
	PhaseBlocked:                      {PhaseBlocked},
}

func transition(from, to Phase, action Action, reason string) (Decision, error) {
	if err := ValidateTransition(from, to); err != nil {
		return Decision{}, err
	}
	return Decision{Next: to, Action: action, Reason: reason}, nil
}

func blocked(from Phase, reason string) Decision {
	return Decision{Next: PhaseBlocked, Action: ActionMarkBlocked, Reason: reason}
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
		PhaseWaitingForReplacement, PhaseWaitingForDestination, PhaseCopying, PhasePromoting,
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
