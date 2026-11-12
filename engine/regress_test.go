package engine_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/engine/enginetest"
	"github.com/genuinebnt/flowforge/eventstore"
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
