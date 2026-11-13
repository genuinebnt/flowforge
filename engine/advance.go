package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/eventstore"
	"github.com/genuinebnt/flowforge/vm"
)

var errCorruptLog = errors.New("engine: run log cannot be replayed")

// farFuture is the wake time of a run that waits for an operator.
var farFuture = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)

// runner advances one run under one lease. All appends go through it
// with the lease's fence; the folded log is updated after every append so
// the live path and replay share one state.
type runner struct {
	e      *Engine
	parent context.Context
	ctx    context.Context
	stop   context.CancelFunc
	run    core.UUID
	info   eventstore.RunInfo

	leaseMu sync.Mutex
	lease   eventstore.Lease

	rec   RunRecord
	ver   WorkflowVersion
	lm    *loadedModule
	mod   *bytecode.Module
	attrs WorkflowAttributes
	vmcfg vm.Config
	log   *runLog
	root  *task

	reqs         []RunRequest
	reqsDirty    atomic.Bool
	applied      map[int64]bool
	killReason   string
	touched      map[string]bool
	pubFailed    bool
	lastRenew    time.Duration
	fenced       atomic.Bool
	compensating atomic.Bool  // compensations run to the end: no interrupts
	interrupted  atomic.Value // string: why in-flight executions were cancelled

	execMu     sync.Mutex
	execCancel context.CancelFunc
}

func newRunner(ctx context.Context, e *Engine, lease eventstore.Lease, info eventstore.RunInfo) *runner {
	cctx, stop := context.WithCancel(ctx)
	return &runner{
		e: e, parent: ctx, ctx: cctx, stop: stop, run: lease.Run, info: info, lease: lease,
		applied: map[int64]bool{}, touched: map[string]bool{}, lastRenew: e.d.Clock.Monotonic(),
	}
}

func (r *runner) now() time.Time { return r.e.now() }

func (r *runner) currentLease() eventstore.Lease {
	r.leaseMu.Lock()
	defer r.leaseMu.Unlock()
	return r.lease
}

// advance replays the run and drives it until it waits or finishes. It
// returns how to release the lease.
func (r *runner) advance() (eventstore.Next, error) {
	defer r.stop()
	evs, err := r.e.d.Store.Read(r.ctx, r.run, 1, 0)
	if err != nil {
		return eventstore.Next{}, err
	}
	if r.log, err = foldLog(evs); err != nil {
		return eventstore.Next{}, fmt.Errorf("%w: %v", errCorruptLog, err)
	}
	if err := r.loadRecord(); err != nil {
		return eventstore.Next{}, err
	}
	r.publishBacklog(evs)
	if r.log.terminal {
		return r.finish(eventstore.Next{Status: r.log.status})
	}
	if r.reqs, err = r.e.d.Repo.PendingRunRequests(r.ctx, r.run); err != nil {
		return eventstore.Next{}, err
	}

	stopWatch := r.startWatchdog()
	next, err := r.main()
	stopWatch()
	if err != nil {
		if r.fenced.Load() {
			return eventstore.Next{}, ErrLeaseLost
		}
		return eventstore.Next{}, err
	}
	return r.finish(next)
}

// finish writes the projection, marks consumed requests and returns next.
func (r *runner) finish(next eventstore.Next) (eventstore.Next, error) {
	if next.Status == "" {
		next.Status = coarseStatus(r.log.status)
	}
	if Terminal(next.Status) && r.rec.PublishedSeq < r.log.head {
		// The outbox has not taken every event (a publish failed, or the
		// previous owner died before publishing). A terminal row is never
		// scheduled again, so releasing it as terminal would lose those
		// events for good (spec 23.7: duplicates, never losses). Keep it
		// schedulable; the next advance republishes the backlog and then
		// releases it as terminal (regress_outbox_terminal_loss).
		next = eventstore.Next{Status: StatusRunning, WakeAt: r.now().Add(r.e.cfg.IdleBackoff)}
	}
	r.flushProjection()
	if len(r.applied) > 0 {
		ids := make([]int64, 0, len(r.applied))
		for id := range r.applied {
			ids = append(ids, id)
		}
		if err := r.e.d.Repo.MarkRunRequestsApplied(r.ctx, r.run, ids); err != nil {
			return next, err
		}
	}
	if !next.WakeAt.IsZero() && !r.rec.DeadlineAt.IsZero() && !Terminal(next.Status) &&
		next.Status != StatusPaused && r.rec.DeadlineAt.Before(next.WakeAt) {
		next.WakeAt = r.rec.DeadlineAt
	}
	return next, nil
}

func (r *runner) loadRecord() error {
	rec, err := r.e.d.Repo.GetRun(r.ctx, core.Nil, r.run)
	switch {
	case errors.Is(err, ErrNotFound):
		rec = recordFromSubmitted(r.run, r.info.Tenant, r.log.submitted, r.log.submittedAt)
		if err := r.e.d.Repo.InsertRun(r.ctx, rec); err != nil && !errors.Is(err, ErrConflict) {
			return err
		}
	case err != nil:
		return err
	}
	r.rec = rec
	return nil
}

// flushProjection repairs the run record and the effects projection from
// the log (best effort: the log stays authoritative).
func (r *runner) flushProjection() {
	ctx := context.WithoutCancel(r.ctx)
	r.log.project(&r.rec)
	_ = r.e.d.Repo.UpdateRun(ctx, r.rec)
	for path := range r.touched {
		if e := r.log.effect(path); e != nil {
			_ = r.e.d.Repo.UpsertEffect(ctx, r.effectRecord(e))
		}
	}
	clear(r.touched)
}

func (r *runner) effectRecord(e *effectState) EffectRecord {
	rec := EffectRecord{
		RunID: r.run, Path: e.path, Seq: e.scheduledSeq, Kind: e.kind.String(),
		State: EffectStateScheduled, Attempt: e.attempt, IdempotencyKey: IdempotencyKey(r.run, e.path),
		ScheduledAt: e.scheduledAt, StartedAt: e.startedAt,
	}
	rec.ArgsHash, _ = hexDecode(e.argsHash)
	switch {
	case e.done && e.failed:
		rec.State, rec.CompletedAt = EffectStateFailed, e.doneAt
		rec.Error, _ = json.Marshal(e.failErr)
	case e.done:
		rec.State, rec.CompletedAt, rec.Result = EffectStateCompleted, e.doneAt, e.result
	case e.unknown:
		rec.State = EffectStateUnknown
	case e.inFlight:
		rec.State = EffectStateStarted
	case e.retryPending:
		rec.NextRetryAt = e.retryAt
		rec.Error, _ = json.Marshal(e.failErr)
	}
	return rec
}

// main is the body of advance once the log, record and requests are
// loaded.
func (r *runner) main() (eventstore.Next, error) {
	var err error
	r.ver, err = r.e.d.Repo.GetWorkflowVersionByID(r.ctx, core.Nil, r.rec.WorkflowVersionID)
	if err != nil {
		return eventstore.Next{}, err
	}
	if r.lm, err = r.e.loadModule(r.ctx, r.ver.Module); err != nil {
		return r.failEarly(bytecode.FailEffectFailed, "workflow module cannot be loaded: "+err.Error())
	}
	r.mod = r.lm.prog.Module()
	r.attrs = workflowAttributes(r.mod, r.ver.Attributes)
	r.vmcfg = vm.Config{
		FuelLimit: r.e.cfg.FuelLimit,
		Limits:    vm.Limits{Effects: r.attrs.MaxEffects},
		Scope:     r.scopeSnapshot(),
	}
	hash := hexHash(r.lm.prog.Hash())
	if r.log.started && r.log.moduleSHA != hash {
		return r.failEarly(bytecode.FailNondeterminismDetected, "the run started on a different module")
	}
	if !r.log.started {
		if r.cancelRequested() {
			return r.cancelFlow(nil)
		}
		if err := r.append(ev(EvRunStarted, ActorEngine, RunStartedPayload{ModuleSHA256: hash, FuelLimit: r.e.cfg.FuelLimit})); err != nil {
			return eventstore.Next{}, err
		}
		if o := r.e.d.Observer; o != nil {
			o.RunStarted(r.rec.Tenant, r.log.startedAt.Sub(r.log.submittedAt))
		}
	}
	root, err := r.newRoot()
	if err != nil {
		return r.failEarly(bytecode.FailEffectFailed, err.Error())
	}
	r.root = root
	st, err := r.runTasks([]*task{root}, 1, false, phaseBody)
	if err != nil {
		return eventstore.Next{}, err
	}
	return r.afterBody(st)
}

// failEarly fails a run that cannot execute at all.
func (r *runner) failEarly(code bytecode.FailCode, msg string) (eventstore.Next, error) {
	var evs []pending
	if !r.log.started {
		evs = append(evs, ev(EvRunStarted, ActorEngine, RunStartedPayload{ModuleSHA256: hexHash(r.ver.ModuleSHA256), FuelLimit: r.e.cfg.FuelLimit}))
	}
	evs = append(evs, ev(EvRunFailed, ActorEngine, RunFailedPayload{Code: code.String(), Message: msg}))
	if r.log.status == StatusCancelling || r.log.status == StatusCompensating {
		evs = evs[:len(evs)-1]
		evs = append(evs, ev(EvRunCancelled, ActorEngine, CancelPayload{Actor: ActorEngine, Reason: msg}))
	}
	if err := r.append(evs...); err != nil {
		return eventstore.Next{}, err
	}
	return eventstore.Next{Status: r.log.status}, nil
}

// newRoot restores the latest snapshot or starts a fresh VM.
func (r *runner) newRoot() (*task, error) {
	if snap, ok, err := r.e.d.Store.LoadSnapshot(r.ctx, r.run); err == nil && ok {
		h := r.lm.prog.Hash()
		if string(snap.ModuleSHA256) == string(h[:]) {
			if v, err := vm.Restore(r.lm.prog, snap.State, r.vmcfg); err == nil {
				return &task{vm: v, out: v.Outcome()}, nil
			}
		}
	}
	inputs, err := vm.DecodeInputs(r.lm.prog, r.rec.Input)
	if err != nil {
		return nil, fmt.Errorf("inputs: %w", err)
	}
	v, err := vm.New(r.lm.prog, r.vmcfg)
	if err != nil {
		return nil, err
	}
	if err := v.Start(inputs); err != nil {
		return nil, err
	}
	return &task{vm: v, out: v.Outcome()}, nil
}

// afterBody acts on how the workflow body stopped.
func (r *runner) afterBody(st stepResult) (eventstore.Next, error) {
	switch st.kind {
	case stepWait:
		return r.park(st), nil
	case stepHalt:
		switch st.halt {
		case haltCancel:
			return r.cancelFlow(r.root)
		case haltTimeout:
			return r.timeoutFlow()
		case haltPause:
			return eventstore.Next{Status: StatusPaused}, nil
		case haltNondeterminism:
			return r.failNondeterministic(st.msg)
		}
		return eventstore.Next{}, fmt.Errorf("engine: unexpected halt %d", st.halt)
	}
	if r.log.cancel != nil {
		return r.cancelFlow(r.root)
	}
	switch o := r.root.out.(type) {
	case vm.Completed:
		out, err := vm.EncodeOutput(r.lm.prog, o.Value)
		if err != nil {
			return r.failureFlow(bytecode.FailTypeMismatch.String(), "output: "+err.Error(), nil)
		}
		if err := r.append(ev(EvRunSucceeded, ActorEngine, RunSucceededPayload{Output: out})); err != nil {
			return eventstore.Next{}, err
		}
		return eventstore.Next{Status: StatusSucceeded}, nil
	case vm.Failed:
		var extra []pending
		if o.Code == bytecode.FailOutOfFuel && r.log.comp == nil {
			extra = append(extra, ev(EvBudgetExceeded, ActorEngine, BudgetExceededPayload{
				Resource: "fuel", Limit: r.e.cfg.FuelLimit, Used: r.root.vm.FuelUsed(),
			}))
		}
		if o.Code == bytecode.FailNondeterminismDetected {
			return r.failNondeterministic(o.Message)
		}
		return r.failureFlow(o.Code.String(), o.Message, extra)
	}
	return eventstore.Next{}, fmt.Errorf("engine: workflow body stopped with %T", r.root.out)
}

// park releases a waiting run until its wake time.
func (r *runner) park(st stepResult) eventstore.Next {
	wake := st.until
	if wake.IsZero() {
		wake = farFuture
	}
	return eventstore.Next{Status: StatusWaiting, WakeAt: wake}
}

// failNondeterministic fails a run whose replay diverged from its log.
// Registered compensations are not run (their replay cannot be trusted);
// compensation_outcome failed marks the saga for operator action
// (inv_07).
func (r *runner) failNondeterministic(msg string) (eventstore.Next, error) {
	var evs []pending
	if r.root != nil && r.log.comp == nil {
		if n := len(r.root.vm.Compensations()); n > 0 {
			evs = append(evs,
				ev(EvCompensationStarted, ActorEngine, CompensationStartedPayload{Count: n, Cause: CauseFailure,
					Code: bytecode.FailNondeterminismDetected.String(), Message: msg, PathPrefix: uint32(r.root.vm.NextSeq())}),
				ev(EvCompensationFinished, ActorEngine, CompensationFinishedPayload{Outcome: CompensationFailed}))
		}
	} else if r.log.comp != nil && !r.log.comp.finished {
		evs = append(evs, ev(EvCompensationFinished, ActorEngine, CompensationFinishedPayload{Outcome: CompensationFailed}))
	}
	if r.log.status == StatusCancelling {
		evs = append(evs, ev(EvRunCancelled, ActorEngine, CancelPayload{Actor: ActorEngine, Reason: "nondeterminism_detected: " + msg}))
	} else {
		evs = append(evs, ev(EvRunFailed, ActorEngine, RunFailedPayload{Code: bytecode.FailNondeterminismDetected.String(), Message: msg}))
	}
	if err := r.append(evs...); err != nil {
		return eventstore.Next{}, err
	}
	return eventstore.Next{Status: r.log.status}, nil
}

// ---- requests ----

func (r *runner) requests() []RunRequest {
	if r.reqsDirty.Swap(false) {
		if reqs, err := r.e.d.Repo.PendingRunRequests(r.ctx, r.run); err == nil {
			r.reqs = reqs
		}
	}
	return r.reqs
}

func (r *runner) cancelRequest() *RunRequest {
	for i, q := range r.requests() {
		if q.Kind == RequestCancel && !r.applied[q.ID] {
			return &r.reqs[i]
		}
	}
	return nil
}

func (r *runner) cancelRequested() bool {
	return r.log.cancel != nil || r.cancelRequest() != nil || r.killReason != ""
}

func (r *runner) markApplied(kind string) {
	for _, q := range r.reqs {
		if q.Kind == kind {
			r.applied[q.ID] = true
		}
	}
}

func (r *runner) resolution(path string) *RunRequest {
	for i, q := range r.requests() {
		if q.Kind == RequestResolve && q.Path == path && !r.applied[q.ID] {
			return &r.reqs[i]
		}
	}
	return nil
}

// pendingScopeChange returns the newest requested scope version above the
// run's pinned one, or 0.
func (r *runner) pendingScopeChange() int32 {
	var v int32
	for _, q := range r.requests() {
		if q.Kind == RequestScopeChanged && !r.applied[q.ID] && q.NewVersion > r.log.scopeVersion {
			v = max(v, q.NewVersion)
		}
	}
	return v
}

func (r *runner) deadlinePassed() bool {
	return !r.rec.DeadlineAt.IsZero() && !r.now().Before(r.rec.DeadlineAt)
}
