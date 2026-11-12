package engine

import (
	"errors"
	"fmt"
)

// ErrIllegalTransition reports an event that the run state machine of
// spec 9.2 does not allow in the run's current status.
var ErrIllegalTransition = errors.New("engine: illegal run status transition")

// transitions is the table of spec 9.2: from -> allowed targets.
// "cancelling enters from any non-terminal" is spelled out.
var transitions = map[RunStatus][]RunStatus{
	StatusPending:      {StatusRunning, StatusCancelling, StatusCancelled},
	StatusRunning:      {StatusWaiting, StatusPaused, StatusCompensating, StatusSucceeded, StatusFailed, StatusCancelling},
	StatusWaiting:      {StatusRunning, StatusCancelling, StatusFailed, StatusPaused},
	StatusPaused:       {StatusRunning, StatusCancelling},
	StatusCancelling:   {StatusCompensating, StatusCancelled},
	StatusCompensating: {StatusFailed, StatusCancelled, StatusCancelling},
}

// CanTransition reports whether the run state machine allows from -> to.
// Staying in the same non-terminal status is always allowed.
func CanTransition(from, to RunStatus) bool {
	if from == to {
		return !Terminal(from)
	}
	for _, t := range transitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// nextStatus returns the status after an event of type typ is appended to
// a run in status cur, or ErrIllegalTransition. Events that do not move
// the status (decisions, logs, observations, ...) are legal in every
// non-terminal status. Effect activity moves a waiting run back to
// running; inside cancelling and compensating it leaves the status alone.
func nextStatus(cur RunStatus, typ string) (RunStatus, error) {
	if cur == "" {
		if typ == EvRunSubmitted {
			return StatusPending, nil
		}
		return "", fmt.Errorf("%w: %s before run_submitted", ErrIllegalTransition, typ)
	}
	if Terminal(cur) {
		return cur, fmt.Errorf("%w: %s after terminal status %s", ErrIllegalTransition, typ, cur)
	}
	to := cur
	switch typ {
	case EvRunSubmitted:
		return cur, fmt.Errorf("%w: duplicate run_submitted", ErrIllegalTransition)
	case EvRunStarted:
		if cur != StatusPending {
			return cur, fmt.Errorf("%w: run_started in %s", ErrIllegalTransition, cur)
		}
		to = StatusRunning
	case EvRunResumed:
		if cur != StatusPaused {
			return cur, fmt.Errorf("%w: run_resumed in %s", ErrIllegalTransition, cur)
		}
		to = StatusRunning
	case EvApprovalRequested, EvTimerStarted, EvEffectRetryScheduled, EvEffectOutcomeUnknown:
		switch cur {
		case StatusRunning, StatusWaiting:
			to = StatusWaiting
		case StatusPending, StatusPaused:
			return cur, fmt.Errorf("%w: %s in %s", ErrIllegalTransition, typ, cur)
		}
	case EvEffectScheduled, EvEffectStarted, EvEffectCompleted, EvEffectFailed,
		EvApprovalDecided, EvApprovalExpired, EvTimerFired:
		switch cur {
		case StatusWaiting:
			to = StatusRunning
		case StatusPending, StatusPaused:
			return cur, fmt.Errorf("%w: %s in %s", ErrIllegalTransition, typ, cur)
		}
	case EvRunPaused:
		to = StatusPaused
	case EvRunCancelRequested:
		to = StatusCancelling
	case EvCompensationStarted:
		to = StatusCompensating
	case EvRunSucceeded:
		to = StatusSucceeded
	case EvRunFailed:
		to = StatusFailed
	case EvRunCancelled:
		to = StatusCancelled
	}
	if !CanTransition(cur, to) {
		return cur, fmt.Errorf("%w: %s moves %s to %s", ErrIllegalTransition, typ, cur, to)
	}
	return to, nil
}

// coarseStatus maps a run status to the scheduler status kept on the
// eventstore row: cancelling and compensating runs are still worked on, so
// the row says running and ListReady picks them up after a crash
// (eventstore OPEN_QUESTIONS 8).
func coarseStatus(s RunStatus) RunStatus {
	switch s {
	case StatusCancelling, StatusCompensating:
		return StatusRunning
	}
	return s
}
