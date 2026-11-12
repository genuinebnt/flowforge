package engine

import (
	"errors"
	"time"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/vm"
)

// doSleep performs sleep(d): a timer row and timer_started, a wait until
// fire_at, then timer_fired and the Unit result (spec 9.5).
func (r *runner) doSleep(s vm.Suspended, e *effectState) (*stepResult, error) {
	if len(s.Args) != 1 {
		return nil, r.failEffect(e, ActorEngine, EffectError{Code: "invalid_arguments", Message: "sleep takes a duration"}, bytecode.FailTypeMismatch)
	}
	id := derivedID("timer", r.run.String(), e.path)
	if e.timerID == "" {
		fire := msTime(r.now().Add(max(time.Duration(s.Args[0].AsInt()), 0)))
		t, err := r.e.d.Repo.UpsertTimer(r.ctx, Timer{ID: id, RunID: r.run, Path: e.path, FireAt: fire})
		if err != nil {
			return nil, err
		}
		fire = msTime(t.FireAt)
		if err := r.append(ev(EvTimerStarted, ActorEngine, TimerPayload{Path: e.path, TimerID: id.String(), FireAt: fmtTime(fire)})); err != nil {
			return nil, err
		}
	}
	if r.now().Before(e.fireAt) {
		return waitUntil(e.fireAt, "timer"), nil
	}
	if err := r.e.d.Repo.FireTimer(r.ctx, id, r.now()); err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	return nil, r.completeEffect(e, ActorEngine, vm.Unit(),
		ev(EvTimerFired, ActorEngine, TimerPayload{Path: e.path, TimerID: e.timerID, FireAt: fmtTime(e.fireAt)}))
}

// doNow performs now(): the wall clock, recorded so replay returns the
// original value (spec 7.6, 8.6).
func (r *runner) doNow(e *effectState) error {
	return r.completeEffect(e, ActorEngine, vm.Time(msTime(r.now()).UnixNano()))
}
