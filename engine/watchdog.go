package engine

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/genuinebnt/flowforge/eventstore"
)

// startWatchdog runs a goroutine for the duration of an advance that
// renews the lease every Config.RenewEvery and, while tool executions are
// in flight, cancels them when the run is cancelled, its deadline passes
// or a kill switch engages (spec 9.8). Decisions about the run are made
// by the advancing goroutine at the next effect boundary; the watchdog
// only interrupts. The returned function stops it.
func (r *runner) startWatchdog() func() {
	sig, unsub := r.e.hub.subscribeRun(r.run)
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		defer unsub()
		t := time.NewTicker(r.e.cfg.PollInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-r.ctx.Done():
				return
			case <-sig:
				r.reqsDirty.Store(true)
				r.checkInterrupt()
			case <-t.C:
				r.renewIfDue()
				if r.inFlight() {
					r.reqsDirty.Store(true)
					r.checkInterrupt()
				}
			}
		}
	}()
	return func() {
		close(done)
		<-exited
	}
}

func (r *runner) renewIfDue() {
	mono := r.e.d.Clock.Monotonic()
	if mono-r.lastRenew < r.e.cfg.RenewEvery {
		return
	}
	lease := r.currentLease()
	nl, err := r.e.d.Store.RenewLease(r.ctx, lease, r.e.cfg.LeaseTTL)
	switch {
	case errors.Is(err, eventstore.ErrFenced):
		r.fenced.Store(true)
		r.stop()
	case err == nil:
		r.leaseMu.Lock()
		r.lease = nl
		r.leaseMu.Unlock()
		r.lastRenew = mono
	}
}

func (r *runner) inFlight() bool {
	r.execMu.Lock()
	defer r.execMu.Unlock()
	return r.execCancel != nil
}

// checkInterrupt cancels in-flight executions if the run must stop.
func (r *runner) checkInterrupt() {
	if !r.inFlight() || r.compensating.Load() {
		return
	}
	why := ""
	if reqs, err := r.e.d.Repo.PendingRunRequests(r.ctx, r.run); err == nil &&
		slices.ContainsFunc(reqs, func(q RunRequest) bool { return q.Kind == RequestCancel }) {
		why = "cancel"
	}
	if why == "" && r.deadlinePassed() {
		why = "timeout"
	}
	if why == "" {
		if ks, err := r.e.d.Scope.KillSwitch(r.ctx, r.rec.Tenant, r.rec.ProgramID); err == nil && ks.Engaged {
			why = "kill_switch"
		}
	}
	if why != "" {
		r.interrupt(why)
	}
}

func (r *runner) interrupt(why string) {
	r.execMu.Lock()
	defer r.execMu.Unlock()
	if r.execCancel != nil {
		r.interrupted.Store(why)
		r.execCancel()
	}
}

// beginExec installs the cancel function of a batch of executions.
func (r *runner) beginExec() (context.Context, func()) {
	ctx, cancel := context.WithCancel(r.ctx)
	r.execMu.Lock()
	r.execCancel = cancel
	r.execMu.Unlock()
	return ctx, func() {
		r.execMu.Lock()
		r.execCancel = nil
		r.execMu.Unlock()
		cancel()
	}
}
