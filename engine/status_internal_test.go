package engine

import (
	"errors"
	"testing"
)

var allStatuses = []RunStatus{StatusPending, StatusRunning, StatusWaiting, StatusPaused, StatusCancelling,
	StatusCompensating, StatusSucceeded, StatusFailed, StatusCancelled}

// TestEngStatusTable pins the run state machine of spec 9.2.
func TestEngStatusTable(t *testing.T) {
	allowed := map[[2]RunStatus]bool{}
	for _, p := range [][2]RunStatus{
		{StatusPending, StatusRunning}, {StatusPending, StatusCancelled}, {StatusPending, StatusCancelling},
		{StatusRunning, StatusWaiting}, {StatusRunning, StatusPaused}, {StatusRunning, StatusCompensating},
		{StatusRunning, StatusSucceeded}, {StatusRunning, StatusFailed}, {StatusRunning, StatusCancelling},
		{StatusWaiting, StatusRunning}, {StatusWaiting, StatusCancelling}, {StatusWaiting, StatusFailed}, {StatusWaiting, StatusPaused},
		{StatusPaused, StatusRunning}, {StatusPaused, StatusCancelling},
		{StatusCancelling, StatusCompensating}, {StatusCancelling, StatusCancelled},
		{StatusCompensating, StatusFailed}, {StatusCompensating, StatusCancelled}, {StatusCompensating, StatusCancelling},
	} {
		allowed[p] = true
	}
	for _, from := range allStatuses {
		for _, to := range allStatuses {
			want := allowed[[2]RunStatus{from, to}] || (from == to && !Terminal(from))
			if got := CanTransition(from, to); got != want {
				t.Errorf("%s -> %s: %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestEngStatusEvents(t *testing.T) {
	for _, tc := range []struct {
		from RunStatus
		typ  string
		to   RunStatus
		ok   bool
	}{
		{"", EvRunSubmitted, StatusPending, true},
		{"", EvRunStarted, "", false},
		{StatusPending, EvRunStarted, StatusRunning, true},
		{StatusPending, EvEffectScheduled, "", false},
		{StatusPending, EvRunSucceeded, "", false},
		{StatusRunning, EvApprovalRequested, StatusWaiting, true},
		{StatusWaiting, EvApprovalDecided, StatusRunning, true},
		{StatusWaiting, EvRunFailed, StatusFailed, true},
		{StatusWaiting, EvCompensationStarted, "", false},
		{StatusWaiting, EvRunSucceeded, "", false},
		{StatusPaused, EvEffectStarted, "", false},
		{StatusPaused, EvRunResumed, StatusRunning, true},
		{StatusRunning, EvRunResumed, "", false},
		{StatusCancelling, EvEffectFailed, StatusCancelling, true},
		{StatusCancelling, EvRunSucceeded, "", false},
		{StatusCancelling, EvRunFailed, "", false},
		{StatusCompensating, EvEffectRetryScheduled, StatusCompensating, true},
		{StatusCompensating, EvRunCancelled, StatusCancelled, true},
		{StatusSucceeded, EvLog, "", false},
		{StatusFailed, EvRunCancelRequested, "", false},
		{StatusRunning, EvRunSubmitted, "", false},
	} {
		got, err := nextStatus(tc.from, tc.typ)
		if tc.ok != (err == nil) || (tc.ok && got != tc.to) {
			t.Errorf("%s + %s = %s, %v; want %s ok=%v", tc.from, tc.typ, got, err, tc.to, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("%s + %s: %v is not ErrIllegalTransition", tc.from, tc.typ, err)
		}
	}
}
