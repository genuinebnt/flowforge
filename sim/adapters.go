package sim

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/eventstore"
	"github.com/genuinebnt/flowforge/limiter"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/scope"
)

// enter is the fault adapter's front half, run on the caller's actor
// goroutine before a call reaches a service: the actor yields for the
// call's latency, then the profile decides whether the call is dropped,
// fails, crashes the worker's process, or (store calls of a partitioned
// worker) cannot reach the store. Calls made from the scheduler itself
// (setup, consumers) carry no actor and are never faulted.
func (c *cluster) enter(ctx context.Context, store bool) (*actor, callPlan, error) {
	a := actorOf(ctx)
	if a == nil {
		return nil, callPlan{}, nil
	}
	if a.crashed.Load() {
		return a, callPlan{}, errCrashed
	}
	if err := c.s.yield(a, c.prof.latency(c.rng, c.faulty())); err != nil {
		return a, callPlan{}, err
	}
	if !c.faulty() {
		return a, callPlan{}, nil
	}
	if store && a.w != nil && c.clock.Now().Before(a.w.partUntil) {
		if err := c.s.yield(a, c.prof.Timeout); err != nil {
			return a, callPlan{}, err
		}
		return a, callPlan{}, errPartitioned
	}
	p := c.prof.plan(c.rng, a.w != nil)
	switch {
	case p.crashBefore:
		c.crash(a.w)
		return a, p, errCrashed
	case p.fail:
		return a, p, errUnavailable
	case p.dropRequest:
		if err := c.s.yield(a, c.prof.Timeout); err != nil {
			return a, p, err
		}
		return a, p, errTimeout
	}
	return a, p, nil
}

// exit is the back half: the call took effect, but the process may die
// before it sees the reply, or the reply may be lost.
func (c *cluster) exit(a *actor, p callPlan) error {
	if a == nil {
		return nil
	}
	if p.crashAfter {
		c.crash(a.w)
		return errCrashed
	}
	if p.dropResponse {
		if err := c.s.yield(a, c.prof.Timeout); err != nil {
			return err
		}
		return errTimeout
	}
	return nil
}

func ownerOf(a *actor) string {
	switch {
	case a == nil:
		return "sim"
	case a.w != nil:
		return a.w.id
	}
	return "api:" + a.name
}

func actorID(a *actor) uint64 {
	if a == nil {
		return 0
	}
	return a.id
}

// faultStore is the event store as the cluster sees it: every call goes
// through the fault adapter, and the store's linearization (lease grants
// and commits, in the order the store executed them) is recorded for
// inv_01..inv_03.
type faultStore struct {
	c     *cluster
	inner *eventstore.MemStore
}

var _ eventstore.Store = (*faultStore)(nil)

func (f *faultStore) op() uint64 {
	f.c.op++
	f.c.hist.Stats.StoreCalls++
	return f.c.op
}

func (f *faultStore) CreateRun(ctx context.Context, spec eventstore.RunSpec, initial []eventstore.Event) error {
	a, p, err := f.c.enter(ctx, true)
	if err != nil {
		return err
	}
	for i := range 1 + b2i(p.duplicate) {
		op := f.op()
		e := f.inner.CreateRun(ctx, spec, initial)
		if i == 0 {
			err = e
		}
		if e == nil {
			f.c.hist.Commits = append(f.c.hist.Commits, Commit{Op: op, At: f.c.clock.Now(), Run: spec.ID.String(),
				Owner: ownerOf(a), Actor: actorID(a), FromSeq: 1, ToSeq: uint64(len(initial))})
			f.c.addRun(spec)
		}
	}
	if e := f.c.exit(a, p); e != nil {
		return e
	}
	return err
}

func (f *faultStore) GetRun(ctx context.Context, run eventstore.RunID) (eventstore.RunInfo, error) {
	a, p, err := f.c.enter(ctx, true)
	if err != nil {
		return eventstore.RunInfo{}, err
	}
	f.op()
	info, err := f.inner.GetRun(ctx, run)
	if e := f.c.exit(a, p); e != nil {
		return eventstore.RunInfo{}, e
	}
	return info, err
}

func (f *faultStore) Append(ctx context.Context, run eventstore.RunID, expectedSeq uint64, fence eventstore.Fence, events []eventstore.Event) (uint64, error) {
	a, p, err := f.c.enter(ctx, true)
	if err != nil {
		return 0, err
	}
	var head uint64
	for i := range 1 + b2i(p.duplicate) {
		op := f.op()
		h, e := f.inner.Append(ctx, run, expectedSeq, fence, events)
		if i == 0 {
			head, err = h, e
		}
		switch {
		case e == nil && len(events) > 0:
			f.c.hist.Commits = append(f.c.hist.Commits, Commit{Op: op, At: f.c.clock.Now(), Run: run.String(),
				Owner: ownerOf(a), Actor: actorID(a), Fence: fence, FromSeq: expectedSeq + 1, ToSeq: h})
			for _, ev := range events {
				if ev.Type == engine.EvRunSucceeded || ev.Type == engine.EvRunFailed || ev.Type == engine.EvRunCancelled {
					f.c.markTerminal(run.String())
				}
			}
		case errors.Is(e, eventstore.ErrFenced):
			f.c.hist.Stats.FencedAppends++
		}
	}
	if e := f.c.exit(a, p); e != nil {
		return 0, e
	}
	return head, err
}

func (f *faultStore) Read(ctx context.Context, run eventstore.RunID, fromSeq uint64, limit int) ([]eventstore.Event, error) {
	a, p, err := f.c.enter(ctx, true)
	if err != nil {
		return nil, err
	}
	f.op()
	evs, err := f.inner.Read(ctx, run, fromSeq, limit)
	if e := f.c.exit(a, p); e != nil {
		return nil, e
	}
	return evs, err
}

// Subscribe is not used by workers; it is passed through.
func (f *faultStore) Subscribe(ctx context.Context, run eventstore.RunID, fromSeq uint64) iter.Seq2[eventstore.Event, error] {
	return f.inner.Subscribe(ctx, run, fromSeq)
}

func (f *faultStore) SaveSnapshot(ctx context.Context, run eventstore.RunID, snap eventstore.Snapshot) error {
	a, p, err := f.c.enter(ctx, true)
	if err != nil {
		return err
	}
	f.op()
	err = f.inner.SaveSnapshot(ctx, run, snap)
	if e := f.c.exit(a, p); e != nil {
		return e
	}
	return err
}

func (f *faultStore) LoadSnapshot(ctx context.Context, run eventstore.RunID) (eventstore.Snapshot, bool, error) {
	a, p, err := f.c.enter(ctx, true)
	if err != nil {
		return eventstore.Snapshot{}, false, err
	}
	f.op()
	snap, ok, err := f.inner.LoadSnapshot(ctx, run)
	if e := f.c.exit(a, p); e != nil {
		return eventstore.Snapshot{}, false, e
	}
	return snap, ok, err
}

// DeleteSnapshots passes through without entering the fault schedule:
// the call is best effort in the engine, and keeping it out of the
// operation count leaves existing seeds' histories unchanged.
func (f *faultStore) DeleteSnapshots(ctx context.Context, run eventstore.RunID) error {
	return f.inner.DeleteSnapshots(ctx, run)
}

func (f *faultStore) grant(op uint64, a *actor, l eventstore.Lease) {
	f.c.hist.Leases = append(f.c.hist.Leases, LeaseGrant{Op: op, At: f.c.clock.Now(), Run: l.Run.String(),
		Owner: string(l.Owner), Actor: actorID(a), Fence: l.Fence})
}

func (f *faultStore) AcquireLease(ctx context.Context, run eventstore.RunID, owner eventstore.WorkerID, ttl time.Duration) (eventstore.Lease, error) {
	a, p, err := f.c.enter(ctx, true)
	if err != nil {
		return eventstore.Lease{}, err
	}
	var lease eventstore.Lease
	for i := range 1 + b2i(p.duplicate) {
		op := f.op()
		l, e := f.inner.AcquireLease(ctx, run, owner, ttl)
		if e == nil {
			f.grant(op, a, l)
		}
		if i == 0 || e == nil {
			lease, err = l, e // a duplicate re-acquisition stales the first lease
		}
	}
	if e := f.c.exit(a, p); e != nil {
		return eventstore.Lease{}, e
	}
	return lease, err
}

func (f *faultStore) RenewLease(ctx context.Context, lease eventstore.Lease, ttl time.Duration) (eventstore.Lease, error) {
	a, p, err := f.c.enter(ctx, true)
	if err != nil {
		return eventstore.Lease{}, err
	}
	f.op()
	l, err := f.inner.RenewLease(ctx, lease, ttl)
	if e := f.c.exit(a, p); e != nil {
		return eventstore.Lease{}, e
	}
	return l, err
}

func (f *faultStore) ReleaseLease(ctx context.Context, lease eventstore.Lease, next eventstore.Next) error {
	a, p, err := f.c.enter(ctx, true)
	if err != nil {
		return err
	}
	f.op()
	err = f.inner.ReleaseLease(ctx, lease, next)
	if e := f.c.exit(a, p); e != nil {
		return e
	}
	return err
}

func (f *faultStore) ListReady(ctx context.Context, shard eventstore.Shard, limit int) ([]eventstore.RunID, error) {
	a, p, err := f.c.enter(ctx, true)
	if err != nil {
		return nil, err
	}
	f.op()
	ids, err := f.inner.ListReady(ctx, shard, limit)
	if e := f.c.exit(a, p); e != nil {
		return nil, e
	}
	return ids, err
}

func (f *faultStore) ClaimReady(ctx context.Context, shard eventstore.Shard, owner eventstore.WorkerID, ttl time.Duration, limit int) ([]eventstore.Lease, error) {
	a, p, err := f.c.enter(ctx, true)
	if err != nil {
		return nil, err
	}
	var leases []eventstore.Lease
	for i := range 1 + b2i(p.duplicate) {
		op := f.op()
		ls, e := f.inner.ClaimReady(ctx, shard, owner, ttl, limit)
		for _, l := range ls {
			f.grant(op, a, l)
		}
		if i == 0 {
			leases, err = ls, e // the duplicate's leases leak until they expire
		}
	}
	if e := f.c.exit(a, p); e != nil {
		return nil, e
	}
	return leases, err
}

func (f *faultStore) Wake(ctx context.Context, run eventstore.RunID) error {
	a, p, err := f.c.enter(ctx, true)
	if err != nil {
		return err
	}
	f.op()
	err = f.inner.Wake(ctx, run)
	if e := f.c.exit(a, p); e != nil {
		return e
	}
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// faultRepo is the engine's relational state. It shares the store's
// database in production, so it is not faulted separately, but a dead
// process cannot write to it.
type faultRepo struct{ *memRepo }

func dead(ctx context.Context) error {
	if a := actorOf(ctx); a != nil && a.crashed.Load() {
		return errCrashed
	}
	return nil
}

func (r faultRepo) CreateWorkflowVersion(ctx context.Context, v engine.WorkflowVersion) (engine.WorkflowVersion, error) {
	if err := dead(ctx); err != nil {
		return engine.WorkflowVersion{}, err
	}
	return r.memRepo.CreateWorkflowVersion(ctx, v)
}

func (r faultRepo) SetWorkflowVersionStatus(ctx context.Context, tenant core.UUID, name string, n int32, status string) (engine.WorkflowVersion, error) {
	if err := dead(ctx); err != nil {
		return engine.WorkflowVersion{}, err
	}
	return r.memRepo.SetWorkflowVersionStatus(ctx, tenant, name, n, status)
}

func (r faultRepo) InsertRun(ctx context.Context, rec engine.RunRecord) error {
	if err := dead(ctx); err != nil {
		return err
	}
	return r.memRepo.InsertRun(ctx, rec)
}

func (r faultRepo) UpdateRun(ctx context.Context, rec engine.RunRecord) error {
	if err := dead(ctx); err != nil {
		return err
	}
	return r.memRepo.UpdateRun(ctx, rec)
}

func (r faultRepo) AddRunRequest(ctx context.Context, q engine.RunRequest) (engine.RunRequest, error) {
	if err := dead(ctx); err != nil {
		return engine.RunRequest{}, err
	}
	return r.memRepo.AddRunRequest(ctx, q)
}

func (r faultRepo) MarkRunRequestsApplied(ctx context.Context, run core.UUID, ids []int64) error {
	if err := dead(ctx); err != nil {
		return err
	}
	return r.memRepo.MarkRunRequestsApplied(ctx, run, ids)
}

func (r faultRepo) UpsertApproval(ctx context.Context, a engine.Approval) (engine.Approval, error) {
	if err := dead(ctx); err != nil {
		return engine.Approval{}, err
	}
	return r.memRepo.UpsertApproval(ctx, a)
}

func (r faultRepo) DecideApproval(ctx context.Context, tenant, id core.UUID, status, actor, comment string, at time.Time) (engine.Approval, bool, error) {
	if err := dead(ctx); err != nil {
		return engine.Approval{}, false, err
	}
	return r.memRepo.DecideApproval(ctx, tenant, id, status, actor, comment, at)
}

func (r faultRepo) UpsertTimer(ctx context.Context, t engine.Timer) (engine.Timer, error) {
	if err := dead(ctx); err != nil {
		return engine.Timer{}, err
	}
	return r.memRepo.UpsertTimer(ctx, t)
}

func (r faultRepo) FireTimer(ctx context.Context, id core.UUID, at time.Time) error {
	if err := dead(ctx); err != nil {
		return err
	}
	return r.memRepo.FireTimer(ctx, id, at)
}

func (r faultRepo) UpsertEffect(ctx context.Context, e engine.EffectRecord) error {
	if err := dead(ctx); err != nil {
		return err
	}
	return r.memRepo.UpsertEffect(ctx, e)
}

// faultScope is the scope service behind the fault adapter. The engine
// treats every error as a deny (spec 23.2 "fail closed").
type faultScope struct {
	c     *cluster
	inner *scopeService
}

func (f faultScope) CurrentVersion(ctx context.Context, tenant, program core.UUID) (int32, error) {
	a, p, err := f.c.enter(ctx, false)
	if err != nil {
		return 0, err
	}
	v, err := f.inner.CurrentVersion(ctx, tenant, program)
	if e := f.c.exit(a, p); e != nil {
		return 0, e
	}
	return v, err
}

func (f faultScope) Policy(ctx context.Context, tenant, program core.UUID, version int32) (*scope.Policy, error) {
	a, p, err := f.c.enter(ctx, false)
	if err != nil {
		return nil, err
	}
	pol, err := f.inner.Policy(ctx, tenant, program, version)
	if e := f.c.exit(a, p); e != nil {
		return nil, e
	}
	return pol, err
}

func (f faultScope) Check(ctx context.Context, req engine.ScopeCheck) (scope.Decision, error) {
	a, p, err := f.c.enter(ctx, false)
	if err != nil {
		return scope.Decision{}, err
	}
	d, err := f.inner.Check(ctx, req)
	if e := f.c.exit(a, p); e != nil {
		return scope.Decision{}, e
	}
	return d, err
}

func (f faultScope) KillSwitch(ctx context.Context, tenant, program core.UUID) (engine.KillSwitchState, error) {
	a, p, err := f.c.enter(ctx, false)
	if err != nil {
		return engine.KillSwitchState{}, err
	}
	ks, err := f.inner.KillSwitch(ctx, tenant, program)
	if e := f.c.exit(a, p); e != nil {
		return engine.KillSwitchState{}, e
	}
	return ks, err
}

// faultIngester is GraphService.IngestObservations behind the fault
// adapter: it validates the envelopes, records them (inv_09) and hands
// them to Pub/Sub, whose consumer applies them to the graph at least
// once, in any order.
type faultIngester struct{ c *cluster }

func (f faultIngester) Ingest(ctx context.Context, req engine.IngestRequest) ([]engine.IngestResult, error) {
	a, p, err := f.c.enter(ctx, false)
	if err != nil {
		return nil, err
	}
	out := make([]engine.IngestResult, 0, len(req.Observations))
	var accepted []Observation
	for _, raw := range req.Observations {
		o, err := oam.DecodeObservation(raw, oam.Lenient)
		if err != nil {
			return nil, err
		}
		rec := Observation{ID: o.ObservationID, Tenant: req.Tenant.String(), Program: req.Program.String(),
			Run: req.RunID.String(), Path: req.Path, Tool: req.Tool, ToolVersion: req.ToolVersion, Raw: append(json.RawMessage(nil), raw...)}
		accepted = append(accepted, rec)
		out = append(out, engine.IngestResult{ObservationID: o.ObservationID, EntityCount: len(o.Entities), EdgeCount: len(o.Edges)})
	}
	for _, rec := range accepted {
		f.c.acceptObservation(rec)
	}
	if e := f.c.exit(a, p); e != nil {
		return nil, e
	}
	return out, nil
}

// faultPublisher is the run.events outbox relay behind the fault adapter.
type faultPublisher struct{ c *cluster }

func (f faultPublisher) Publish(ctx context.Context, envs []engine.Envelope) error {
	a, p, err := f.c.enter(ctx, false)
	if err != nil {
		return err
	}
	for _, env := range envs {
		if env.Topic == engine.TopicRunEvents {
			env := env
			f.c.deliver(func() { f.c.onRunEvent(env) })
		}
	}
	return f.c.exit(a, p)
}

// simLimiter is the shared limiter (the Redis limiter in production). It
// uses the store-side clock, as Redis's TIME would, so worker clock skew
// cannot widen the budget, and records the admitted rl:h keys (inv_10).
type simLimiter struct {
	c *cluster
	g *limiter.GCRA
}

func (l simLimiter) AcquireAll(reqs []limiter.Request, _ time.Duration) (limiter.Result, error) {
	res, err := l.g.AcquireAll(reqs, l.c.clock.Monotonic())
	if err == nil && res.Allowed {
		for _, r := range reqs {
			if strings.HasPrefix(r.Key, "rl:h:") {
				l.c.hist.Admissions = append(l.c.hist.Admissions, Admission{At: l.c.clock.Now(), Actor: actorID(l.c.s.running), Key: r.Key})
			}
		}
	}
	return res, err
}
