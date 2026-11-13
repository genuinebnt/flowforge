package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/eventstore"
)

// Worker advances runs (spec 9.4): it claims a ready run with a lease,
// replays its log, drives the VM to the live frontier, performs effects
// with every append fenced by the lease, and releases the run when it
// waits or finishes. Several workers, in one process or many, may share
// a store; the lease and fence guarantee a single writer per run.
type Worker struct {
	e     *Engine
	id    eventstore.WorkerID
	shard eventstore.Shard
	wakes <-chan struct{}

	mu      sync.Mutex
	tenants map[core.UUID]int
}

// WorkerOption configures a Worker.
type WorkerOption func(*Worker)

// WithShard restricts the worker to one shard of the runs.
func WithShard(s eventstore.Shard) WorkerOption { return func(w *Worker) { w.shard = s } }

// WithWakeups adds an external wake source (for example the engine_wake
// LISTEN channel) that interrupts the idle backoff of Run.
func WithWakeups(ch <-chan struct{}) WorkerOption { return func(w *Worker) { w.wakes = ch } }

// NewWorker returns a worker with the given id (unique per process
// instance).
func (e *Engine) NewWorker(id eventstore.WorkerID, opts ...WorkerOption) *Worker {
	w := &Worker{e: e, id: id, tenants: map[core.UUID]int{}}
	for _, o := range opts {
		o(w)
	}
	return w
}

// ID returns the worker id.
func (w *Worker) ID() eventstore.WorkerID { return w.id }

// RunOnce claims one ready run, advances it until it waits or finishes,
// and releases it. It reports whether a run was claimed. An error means
// the run could not be advanced (for example ErrLeaseLost when another
// worker took the lease over); the run stays in the store for a later
// attempt.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	leases, err := w.e.d.Store.ClaimReady(ctx, w.shard, w.id, w.e.cfg.LeaseTTL, 1)
	if err != nil {
		return false, err
	}
	if len(leases) == 0 {
		return false, nil
	}
	return true, w.process(ctx, leases[0])
}

// Run claims and advances runs until ctx is done, up to
// Config.WorkerConcurrency at a time and Config.TenantConcurrency per
// tenant. When nothing is ready it waits for a wake notification or an
// exponential backoff capped at Config.IdleBackoff.
//
// Cancelling ctx starts a graceful drain (Config.DrainTimeout): no new run
// is claimed; the runs in progress keep their own context, so a tool call
// in flight completes and its result is appended under the lease; each run
// then stops at its next effect boundary and is released as running with
// next_wake_at = now, so another worker resumes it at once. Run returns
// when every run is released, or cancels the executions still in flight
// when the drain timeout expires and returns once they are released.
func (w *Worker) Run(ctx context.Context) error {
	wake, unsub := w.e.hub.subscribeWorker()
	defer unsub()
	// PORT: context.WithoutCancel keeps the values of ctx (trace ids) but
	// not its cancellation; execCancel is the drain deadline's cancel.
	execCtx, execCancel := context.WithCancel(context.WithoutCancel(ctx))
	defer execCancel()
	var draining atomic.Bool
	sem := make(chan struct{}, w.e.cfg.WorkerConcurrency)
	var wg sync.WaitGroup
	defer func() {
		draining.Store(true)
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		if w.e.cfg.DrainTimeout < 0 {
			execCancel()
		} else {
			t := time.NewTimer(w.e.cfg.DrainTimeout)
			select {
			case <-done:
			case <-t.C:
				execCancel()
			}
			t.Stop()
		}
		<-done
	}()
	minBackoff := max(w.e.cfg.IdleBackoff/16, time.Millisecond)
	backoff := minBackoff
	for {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return nil
		}
		leases, err := w.e.d.Store.ClaimReady(ctx, w.shard, w.id, w.e.cfg.LeaseTTL, 1)
		if err != nil || len(leases) == 0 {
			<-sem
			if ctx.Err() != nil {
				if len(leases) > 0 { // claimed as ctx ended: hand it straight back
					_ = w.e.d.Store.ReleaseLease(execCtx, leases[0], eventstore.Next{Status: StatusRunning, WakeAt: w.e.now()})
				}
				return nil
			}
			t := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil
			case <-wake:
			case <-w.wakes:
			case <-t.C:
			}
			t.Stop()
			backoff = min(2*backoff, w.e.cfg.IdleBackoff)
			continue
		}
		backoff = minBackoff
		wg.Add(1)
		go func(l eventstore.Lease) {
			defer wg.Done()
			defer func() { <-sem }()
			_ = w.processDraining(execCtx, l, draining.Load)
		}(leases[0])
	}
}

func (w *Worker) acquireTenant(t core.UUID) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.tenants[t] >= w.e.cfg.TenantConcurrency {
		return false
	}
	w.tenants[t]++
	return true
}

func (w *Worker) releaseTenant(t core.UUID) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.tenants[t]--; w.tenants[t] <= 0 {
		delete(w.tenants, t)
	}
}

// process advances one leased run and releases it.
func (w *Worker) process(ctx context.Context, lease eventstore.Lease) error {
	return w.processDraining(ctx, lease, nil)
}

// processDraining is process with a drain signal: once draining reports
// true the run stops at its next effect boundary (Worker.Run).
func (w *Worker) processDraining(ctx context.Context, lease eventstore.Lease, draining func() bool) error {
	st := w.e.d.Store
	info, err := st.GetRun(ctx, lease.Run)
	if err != nil {
		return err
	}
	if !w.acquireTenant(info.Tenant) {
		// Over the tenant's cap: hand the run back for a little later.
		return st.ReleaseLease(ctx, lease, eventstore.Next{WakeAt: w.e.now().Add(w.e.cfg.IdleBackoff)})
	}
	defer w.releaseTenant(info.Tenant)

	r := newRunner(ctx, w.e, lease, info)
	r.draining = draining
	next, err := r.advance()
	if errors.Is(err, ErrLeaseLost) || r.fenced.Load() {
		return ErrLeaseLost // another worker owns the run now; never write again
	}
	if err != nil {
		if errors.Is(err, errCorruptLog) {
			// An operator must look at the run; keep it out of the ready set.
			_ = st.ReleaseLease(context.WithoutCancel(ctx), lease, eventstore.Next{Status: StatusPaused})
			return err
		}
		// Best effort: hand the run back with a delay. If the store is
		// unreachable the lease simply expires.
		_ = st.ReleaseLease(context.WithoutCancel(ctx), lease, eventstore.Next{WakeAt: w.e.now().Add(w.e.cfg.IdleBackoff)})
		return err
	}
	if err := st.ReleaseLease(context.WithoutCancel(ctx), r.currentLease(), next); err != nil {
		if errors.Is(err, eventstore.ErrFenced) {
			return ErrLeaseLost
		}
		return err
	}
	return nil
}
