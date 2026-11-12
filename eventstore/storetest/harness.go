package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/eventstore"
	"github.com/genuinebnt/flowforge/testkit"
)

// Harness adapts one Store implementation to the suite.
type Harness struct {
	// Store is the implementation under test. It must start empty: the
	// suite lists ready runs and expects to see only its own.
	Store eventstore.Store

	// Now returns the store's current time, the clock that lease expiry
	// and wake times are compared with (the FakeClock for MemStore, the
	// database now() for Postgres).
	Now func(ctx context.Context) (time.Time, error)

	// Elapse makes d of store time pass for every run: leases and wake
	// times that were d in the future become due.
	Elapse func(ctx context.Context, d time.Duration) error

	// WorkflowVersion is used as RunSpec.WorkflowVersion. Postgres needs
	// an existing engine.workflow_versions row (foreign key); zero means a
	// fixed placeholder, which suffices for stores without the constraint.
	WorkflowVersion core.UUID

	// Restart, if set, simulates a restart: it returns a new Store over
	// the same durable state. Property (4) across restarts is checked only
	// when it is set.
	Restart func(t testing.TB) eventstore.Store

	// PropCases overrides the number of random-interleaving iterations
	// (0: testkit.Cases()).
	PropCases int
}

// NewHarness returns a fresh, empty harness for one test.
type NewHarness func(t *testing.T) *Harness

var placeholderWorkflowVersion = core.MustParseUUID("00000000-0000-7000-8000-00000000f10f")

// env bundles what a test case needs.
type env struct {
	t   testing.TB
	h   *Harness
	s   eventstore.Store
	ctx context.Context
	rng core.Rng
	ids *idGen
}

func newEnv(t *testing.T, nh NewHarness) *env {
	t.Helper()
	h := nh(t)
	if h == nil || h.Store == nil || h.Elapse == nil || h.Now == nil {
		t.Fatal("storetest: harness must set Store, Now and Elapse")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	rng := testkit.Rng(t)
	return &env{t: t, h: h, s: h.Store, ctx: ctx, rng: rng, ids: &idGen{rng: rng}}
}

// idGen makes unique run ids from the test's seeded rng.
type idGen struct {
	rng core.Rng
	n   uint64
}

func (g *idGen) next() eventstore.RunID {
	g.n++
	clk := testkit.NewFakeClock(time.UnixMilli(int64(1_700_000_000_000 + g.n)))
	return core.NewUUIDv7(clk, g.rng)
}

func (e *env) spec() eventstore.RunSpec {
	id := e.ids.next()
	wv := e.h.WorkflowVersion
	if wv.IsZero() {
		wv = placeholderWorkflowVersion
	}
	return eventstore.RunSpec{
		ID:              id,
		Tenant:          core.MustParseUUID("00000000-0000-7000-8000-0000000000a1"),
		WorkflowVersion: wv,
		CreatedBy:       "user:storetest",
		IdempotencyKey:  "idem-" + id.String(),
		RequestHash:     []byte{1, 2, 3},
		Input:           json.RawMessage(`{"suite":"storetest"}`),
	}
}

// newRun creates a run (optionally adjusted by mod) and returns its id.
func (e *env) newRun(mod ...func(*eventstore.RunSpec)) eventstore.RunID {
	e.t.Helper()
	sp := e.spec()
	for _, f := range mod {
		f(&sp)
	}
	if err := e.s.CreateRun(e.ctx, sp, nil); err != nil {
		e.t.Fatalf("CreateRun: %v", err)
	}
	return sp.ID
}

func (e *env) now() time.Time {
	e.t.Helper()
	n, err := e.h.Now(e.ctx)
	if err != nil {
		e.t.Fatalf("harness Now: %v", err)
	}
	return n
}

func (e *env) elapse(d time.Duration) {
	e.t.Helper()
	if err := e.h.Elapse(e.ctx, d); err != nil {
		e.t.Fatalf("harness Elapse(%v): %v", d, err)
	}
}

func (e *env) acquire(run eventstore.RunID, w eventstore.WorkerID, ttl time.Duration) eventstore.Lease {
	e.t.Helper()
	l, err := e.s.AcquireLease(e.ctx, run, w, ttl)
	if err != nil {
		e.t.Fatalf("AcquireLease(%s, %s): %v", run, w, err)
	}
	return l
}

func (e *env) append(run eventstore.RunID, expected uint64, f eventstore.Fence, evs ...eventstore.Event) uint64 {
	e.t.Helper()
	h, err := e.s.Append(e.ctx, run, expected, f, evs)
	if err != nil {
		e.t.Fatalf("Append(%s, expected %d, fence %s): %v", run, expected, f, err)
	}
	if want := expected + uint64(len(evs)); h != want {
		e.t.Fatalf("Append returned head %d, want %d", h, want)
	}
	return h
}

func (e *env) read(run eventstore.RunID, from uint64, limit int) []eventstore.Event {
	e.t.Helper()
	evs, err := e.s.Read(e.ctx, run, from, limit)
	if err != nil {
		e.t.Fatalf("Read(%s, %d, %d): %v", run, from, limit, err)
	}
	return evs
}

func (e *env) head(run eventstore.RunID) uint64 {
	e.t.Helper()
	info, err := e.s.GetRun(e.ctx, run)
	if err != nil {
		e.t.Fatalf("GetRun(%s): %v", run, err)
	}
	return info.Head
}

func (e *env) wantErr(err, target error, what string) {
	e.t.Helper()
	if !errors.Is(err, target) {
		e.t.Fatalf("%s: got error %v, want %v", what, err, target)
	}
}

// ev builds an event with a JSON payload.
func ev(typ, actor string, payload any) eventstore.Event {
	b, err := json.Marshal(payload)
	if err != nil {
		panic(err) // test fixtures only
	}
	return eventstore.Event{Type: typ, Actor: actor, Payload: b}
}

func logEv(i int) eventstore.Event {
	return ev("log", "engine", map[string]any{"level": "info", "message": fmt.Sprintf("m%d", i)})
}

// jsonEqual compares two JSON documents by value (stores may normalize
// numbers and key order differently, e.g. Postgres jsonb).
func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return string(xa) == string(ya)
}

// checkLog verifies per-run seq contiguity 1..head and run ids.
func (e *env) checkLog(run eventstore.RunID, evs []eventstore.Event) {
	e.t.Helper()
	for i, x := range evs {
		if x.Seq != uint64(i)+1 {
			e.t.Fatalf("run %s: event %d has seq %d (gap or duplicate)", run, i, x.Seq)
		}
		if x.RunID != run {
			e.t.Fatalf("run %s: event %d has run id %s", run, i, x.RunID)
		}
		if x.At.IsZero() {
			e.t.Fatalf("run %s: event %d has zero timestamp", run, i)
		}
	}
}
