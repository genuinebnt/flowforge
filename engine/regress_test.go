package engine_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/engine/enginetest"
	"github.com/genuinebnt/flowforge/eventstore"
	"github.com/genuinebnt/flowforge/limiter"
	"github.com/genuinebnt/flowforge/scope"
)

// Regression tests for bugs found by the deterministic simulator (sim,
// conformance/sim/regress_seeds.json).

// outagePolicyScope fails the first Policy calls as an unreachable scope
// service would.
type outagePolicyScope struct {
	*enginetest.MemScope
	mu   sync.Mutex
	fail int
}

func (s *outagePolicyScope) Policy(ctx context.Context, tenant, program core.UUID, version int32) (*scope.Policy, error) {
	s.mu.Lock()
	fail := s.fail > 0
	if fail {
		s.fail--
	}
	s.mu.Unlock()
	if fail {
		return nil, errors.New("scope service unavailable")
	}
	return s.MemScope.Policy(ctx, tenant, program, version)
}

// TestRegress_ScopeOutagePause: a scope service outage while a run
// re-validated against a new scope version recorded scope_changed{pause}
// and run_paused, and the run stayed paused forever although the new
// version allowed it (sim chaos seed 3). Now the run waits at the boundary
// and re-validates once the service answers.
func TestRegress_ScopeOutagePause(t *testing.T) {
	h, r := multiHarness(t)
	w := h.Engine.NewWorker("w")
	h.Drain(w) // waits on sleep(1s)
	v2, _ := h.Scope.PutScope(enginetest.TenantA, enginetest.ProgramA, enginetest.DocAllowing(core.ClassIntrusive, "acme.com"))
	if err := h.Engine.HandleScopeChanged(h.Ctx(), enginetest.TenantA, enginetest.ProgramA, v2); err != nil {
		t.Fatal(err)
	}
	d := h.Deps(h.Store)
	// Two failures: advance start (scope.allows snapshot) and revalidation.
	d.Scope = &outagePolicyScope{MemScope: h.Scope, fail: 2}
	e, err := engine.New(d, h.Config)
	if err != nil {
		t.Fatal(err)
	}
	if st := h.RunToEnd(e.NewWorker("w2"), r.ID); st != engine.StatusSucceeded {
		t.Fatalf("status %s, want succeeded; events %v", st, enginetest.Types(h.Events(r.ID)))
	}
	for _, typ := range enginetest.Types(h.Events(r.ID)) {
		if typ == engine.EvRunPaused {
			t.Fatal("an outage paused the run")
		}
	}
}

// failingPublisher fails every publish of a batch holding a terminal run
// event until fail reaches zero.
type failingPublisher struct {
	*enginetest.MemPublisher
	mu   sync.Mutex
	fail int
}

func (p *failingPublisher) Publish(ctx context.Context, envs []engine.Envelope) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range envs {
		if e.Type == "run."+engine.EvRunSucceeded && p.fail > 0 {
			p.fail--
			return errors.New("relay unavailable")
		}
	}
	return p.MemPublisher.Publish(ctx, envs)
}

// TestRegress_OutboxTerminalLoss: when publishing the batch with the
// terminal event failed, the worker still released the run as terminal;
// terminal runs are never scheduled again, so those events never reached
// run.events and their alerts were lost (sim lossy seed 1, inv_08). Now
// the run stays schedulable until its outbox watermark reaches the head.
func TestRegress_OutboxTerminalLoss(t *testing.T) {
	pub := &failingPublisher{MemPublisher: enginetest.NewMemPublisher(), fail: 1}
	h, r := multiHarness(t, func(h *enginetest.Harness) { h.Publisher = pub })
	if st := h.RunToEnd(h.Engine.NewWorker("w"), r.ID); st != engine.StatusSucceeded {
		t.Fatalf("status %s", st)
	}
	found := false
	for _, e := range pub.Envelopes(engine.TopicRunEvents) {
		found = found || e.Type == "run."+engine.EvRunSucceeded
	}
	if !found {
		t.Fatal("run_succeeded never reached the outbox")
	}
	if n := len(pub.Envelopes(engine.TopicRunEvents)); n != len(h.Events(r.ID)) {
		t.Fatalf("%d envelopes for %d events", n, len(h.Events(r.ID)))
	}
}

// TestRegress_CompCancelledOnResume: a worker died while a compensation
// of a cancelled run was in flight. The next owner re-entered the cancel
// flow, which "closed" every open effect, including the in-flight
// compensation effect, as cancelled; the compensation was recorded as
// failed (operator action) although compensations run to the end, and a
// late execution by the dead worker could then land after the next undo
// (sim chaos seed 21, inv_07). Now the next owner continues the
// compensation effect (re-executing an idempotent undo with its key).
func TestRegress_CompCancelledOnResume(t *testing.T) {
	h, r, started := blockingSaga(t, 0)
	c := enginetest.Analyst(enginetest.TenantA)
	cs := enginetest.NewCrashStore(h.Store, -1)
	cs.BeforeAppend = func(evs []eventstore.Event) {
		for _, e := range evs {
			var p engine.EffectStartedPayload
			if e.Type == engine.EvEffectStarted && jsonUnmarshal(e.Payload, &p) == nil && p.Path == "3/0/0" {
				cs.Crash() // this append lands; the process dies right after
			}
		}
	}
	doomed := h.NewEngine(cs).NewWorker("doomed")
	done := make(chan struct{})
	go func() { _, _ = doomed.RunOnce(h.Ctx()); close(done) }()
	<-started
	if _, err := h.Engine.CancelRun(h.Ctx(), c, r.ID, "stop"); err != nil {
		t.Fatal(err)
	}
	<-done
	if !cs.Crashed() {
		t.Fatal("the worker did not reach the compensation")
	}
	h.Clock.Advance(h.Engine.Config().LeaseTTL + time.Second)
	h.RunToEnd(h.Engine.NewWorker("w"), r.ID)
	got, _ := h.Engine.GetRun(h.Ctx(), c, r.ID)
	if got.Status != engine.StatusCancelled || got.CompensationOutcome != engine.CompensationCompleted {
		t.Fatalf("run %s compensation %s; events %v", got.Status, got.CompensationOutcome, enginetest.Types(h.Events(r.ID)))
	}
	var first []string
	for _, id := range undoOrder(h) {
		if !slices.Contains(first, id) {
			first = append(first, id)
		}
	}
	if !slices.Equal(first, []string{"id-1", "id-0"}) {
		t.Fatalf("undo order %v", undoOrder(h))
	}
}

// downStore is a distributed limiter store that is unreachable.
type downStore struct{}

func (downStore) AcquireAll(context.Context, []limiter.Request, time.Duration) (limiter.Result, error) {
	return limiter.Result{}, limiter.ErrUnavailable
}

func (downStore) Acquire(context.Context, string, int, time.Duration) (limiter.Result, error) {
	return limiter.Result{}, limiter.ErrUnavailable
}

// failoverPort is limiter.Failover as an engine.WorkLimiter, recording the
// work kind of every call.
type failoverPort struct {
	f     limiter.Failover
	mu    sync.Mutex
	works []limiter.Work
}

func (p *failoverPort) AcquireAll(reqs []limiter.Request, now time.Duration) (limiter.Result, error) {
	return p.AcquireAllWork(reqs, limiter.WorkActive, now)
}

func (p *failoverPort) AcquireAllWork(reqs []limiter.Request, w limiter.Work, now time.Duration) (limiter.Result, error) {
	p.mu.Lock()
	p.works = append(p.works, w)
	p.mu.Unlock()
	return p.f.AcquireAll(context.Background(), reqs, w, now)
}

// TestRegress_G_LIM_02_FailoverDuringRedisOutage: with the distributed
// limiter down (spec 23.8 "Redis unavailable"), the passive subfinder call
// of eng_001 proceeds on the local budget while the active httpx calls do
// not start: the run waits (fail closed) instead of failing or bypassing
// the per-target ceiling.
func TestRegress_G_LIM_02_FailoverDuringRedisOutage(t *testing.T) {
	local, err := limiter.NewAtomicGCRA(limiter.Rule{Pattern: "*", Params: limiter.Params{Rate: 10, Burst: 10}})
	if err != nil {
		t.Fatal(err)
	}
	port := &failoverPort{f: limiter.Failover{Primary: downStore{}, Local: local}}
	h := enginetest.New(t, func(h *enginetest.Harness) { h.Limiter = port })
	h.Exec.Handle("subfinder", enginetest.Output([]string{"api.acme.com"}))
	h.Exec.Handle("httpx", func(context.Context, engine.ToolCall) (engine.ToolResult, error) {
		t.Error("active tool ran while the limiter store was down")
		return engine.ToolResult{}, errors.New("unreachable")
	})
	c := enginetest.Analyst(enginetest.TenantA)
	h.Register(c, "recon", enginetest.Recon, enginetest.ProgramA)
	r := h.Submit(c, "recon", enginetest.ProgramA, map[string]any{"roots": []string{"acme.com"}}, "k")
	w := h.Engine.NewWorker("w")
	for range 5 { // several wake-ups: the active call keeps waiting
		h.Drain(w)
		h.Clock.Advance(time.Minute)
	}
	info, err := h.Store.GetRun(h.Ctx(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if engine.Terminal(info.Status) {
		t.Fatalf("run ended %s during the outage, want it waiting", info.Status)
	}
	var subfinderDone bool
	for _, e := range h.Events(r.ID) {
		if e.Type == engine.EvEffectCompleted && strings.Contains(string(e.Payload), "api.acme.com") {
			subfinderDone = true
		}
	}
	if !subfinderDone {
		t.Errorf("passive subfinder did not complete on the local budget: %v", enginetest.Types(h.Events(r.ID)))
	}
	if !slices.Contains(port.works, limiter.WorkPassive) || !slices.Contains(port.works, limiter.WorkActive) {
		t.Errorf("work kinds passed to the limiter: %v", port.works)
	}
}

// slowPost is a non-idempotent tool that blocks until released (or until
// its context is cancelled) and counts its calls.
type slowPost struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func newSlowPost() *slowPost {
	return &slowPost{started: make(chan struct{}, 16), release: make(chan struct{})}
}

func (p *slowPost) handle(ctx context.Context, call engine.ToolCall) (engine.ToolResult, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	p.started <- struct{}{}
	select {
	case <-p.release:
		return engine.ToolResult{Output: []byte(`"id-0"`)}, nil
	case <-ctx.Done():
		return engine.ToolResult{}, ctx.Err()
	}
}

// TestRegress_G_ENG_05_GracefulDrain: SIGTERM (cancelling Worker.Run's
// context) while a non-idempotent tool call is in flight lets the call
// finish and records its result normally (no effect_outcome_unknown for
// the next owner, the tool is called once); the run is released at the
// next effect boundary with next_wake_at = now and finishes on another
// worker. When the call outlives Config.DrainTimeout it is cancelled as in
// a crash and Run still returns.
func TestRegress_G_ENG_05_GracefulDrain(t *testing.T) {
	t.Run("in-flight effect finishes within the drain", func(t *testing.T) {
		post := newSlowPost()
		h := enginetest.New(t, func(h *enginetest.Harness) { h.Config.DrainTimeout = 10 * time.Second })
		h.Exec.Handle("post", post.handle)
		h.Exec.Handle("undo", enginetest.Output(nil))
		c := enginetest.Analyst(enginetest.TenantA)
		h.Register(c, "saga", enginetest.Saga(enginetest.SagaOptions{Steps: 2}), core.Nil)
		r := h.Submit(c, "saga", core.Nil, map[string]any{"amount": 5}, "k")

		ctx, sigterm := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- h.Engine.NewWorker("w1").Run(ctx) }()
		<-post.started // the first post is in flight
		sigterm()
		select {
		case err := <-done:
			t.Fatalf("Run returned (%v) while an effect was in flight", err)
		case <-time.After(50 * time.Millisecond):
		}
		close(post.release)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("Run did not return after the in-flight effect finished")
		}

		info, err := h.Store.GetRun(h.Ctx(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if info.LeaseOwner != "" || info.Status != engine.StatusRunning || info.NextWakeAt.After(h.Clock.Now()) {
			t.Fatalf("after the drain: owner %q status %s wake %v (now %v), want released, running, due now",
				info.LeaseOwner, info.Status, info.NextWakeAt, h.Clock.Now())
		}
		types := enginetest.Types(h.Events(r.ID))
		if slices.Contains(types, engine.EvEffectOutcomeUnknown) || !slices.Contains(types, engine.EvEffectCompleted) {
			t.Fatalf("log after the drain: %v", types)
		}
		if n := strings.Count(strings.Join(types, " "), engine.EvEffectStarted); n != 1 {
			t.Fatalf("%d effects started during the drain, want only the in-flight one: %v", n, types)
		}
		if st := h.RunToEnd(h.Engine.NewWorker("w2"), r.ID); st != engine.StatusSucceeded {
			t.Fatalf("status %s on the next worker", st)
		}
		types = enginetest.Types(h.Events(r.ID))
		if slices.Contains(types, engine.EvEffectOutcomeUnknown) {
			t.Fatalf("outcome unknown after a graceful drain: %v", types)
		}
		if post.calls != 2 {
			t.Fatalf("post called %d times, want 2 (one per step, none repeated)", post.calls)
		}
	})

	t.Run("drain timeout cancels like a crash", func(t *testing.T) {
		post := newSlowPost()
		h := enginetest.New(t, func(h *enginetest.Harness) { h.Config.DrainTimeout = 30 * time.Millisecond })
		h.Exec.Handle("post", post.handle)
		c := enginetest.Analyst(enginetest.TenantA)
		h.Register(c, "saga", enginetest.Saga(enginetest.SagaOptions{Steps: 1}), core.Nil)
		h.Submit(c, "saga", core.Nil, map[string]any{"amount": 5}, "k")
		ctx, sigterm := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- h.Engine.NewWorker("w1").Run(ctx) }()
		<-post.started
		sigterm()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("Run ignored the drain timeout")
		}
	})
}
