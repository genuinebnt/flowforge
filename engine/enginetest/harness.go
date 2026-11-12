package enginetest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/eventstore"
	"github.com/genuinebnt/flowforge/testkit"
)

// Start is the fake clock's start time in every harness.
var Start = time.Date(2026, 11, 12, 14, 2, 11, 0, time.UTC)

// Fixed identities used by the harness.
var (
	TenantA  = core.MustParseUUID("0190a1b2-0000-7000-8000-00000000a001")
	TenantB  = core.MustParseUUID("0190a1b2-0000-7000-8000-00000000b001")
	ProgramA = core.MustParseUUID("0190a1b2-0000-7000-8000-0000000000a1")
	ProgramB = core.MustParseUUID("0190a1b2-0000-7000-8000-0000000000b1")
)

// Analyst, Approver and Admin return callers of a tenant.
func Analyst(t core.UUID) engine.Caller {
	return engine.Caller{Tenant: t, Actor: "user:" + core.MustParseUUID("0190a1b2-0000-7000-8000-00000000c001").String(), Roles: []string{"analyst"}}
}

// Approver holds the finance_ap and lead roles.
func Approver(t core.UUID) engine.Caller {
	return engine.Caller{Tenant: t, Actor: "user:" + core.MustParseUUID("0190a1b2-0000-7000-8000-00000000c002").String(), Roles: []string{"approver", "finance_ap", "lead"}}
}

// Harness wires an engine with in-memory fakes, a fake clock and seeded
// randomness. Store, Repo and Tools can be replaced (pgstore, pgrepo)
// before Init.
type Harness struct {
	T     testing.TB
	Clock *testkit.FakeClock
	Seed  uint64
	Store eventstore.Store
	Repo  engine.Repo
	Tools interface {
		engine.ToolRegistry
		Register(engine.ToolSpec) engine.ToolSpec
	}
	Scope    *MemScope
	Exec     *FakeToolExec
	Ingester *MemIngester
	Pub      *MemPublisher
	// Publisher, when set, replaces Pub (for example the pgrepo outbox).
	Publisher engine.Publisher
	Compiler  *FakeCompiler
	Config    engine.Config
	Engine    *engine.Engine
	ctx       context.Context
}

// New returns an initialized harness with MemStore, MemRepo and MemTools,
// every test tool registered and programs A (tenant A) and B (tenant B)
// whose scope allows acme.com and example.com up to intrusive activity.
func New(t testing.TB, configure ...func(*Harness)) *Harness {
	t.Helper()
	clock := testkit.NewFakeClock(Start)
	h := &Harness{
		T: t, Clock: clock, Seed: 42,
		Scope: NewMemScope(nil), Exec: NewFakeToolExec(), Ingester: NewMemIngester(),
		Pub: NewMemPublisher(), Compiler: NewFakeCompiler(), ctx: context.Background(),
		Config: engine.Config{PollInterval: time.Millisecond, CancelGrace: 200 * time.Millisecond},
	}
	for _, c := range configure {
		c(h)
	}
	if h.Store == nil {
		h.Store = eventstore.NewMemStore(clock)
	}
	if h.Repo == nil {
		h.Repo = NewMemRepo(core.IDGen{Clock: clock, Rng: core.NewSplitMix64(7)})
	}
	if h.Tools == nil {
		h.Tools = NewMemTools()
	}
	for _, s := range AllSpecs() {
		h.Tools.Register(s)
	}
	doc := DocAllowing(core.ClassIntrusive, "acme.com", "example.com")
	if _, err := h.Scope.PutScope(TenantA, ProgramA, doc); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Scope.PutScope(TenantB, ProgramB, doc); err != nil {
		t.Fatal(err)
	}
	h.Engine = h.NewEngine(h.Store)
	return h
}

// Deps returns the harness ports with the given store.
func (h *Harness) Deps(store eventstore.Store) engine.Deps {
	var pub engine.Publisher = h.Pub
	if h.Publisher != nil {
		pub = h.Publisher
	}
	return engine.Deps{
		Store: store, Repo: h.Repo, Compiler: h.Compiler, Scope: h.Scope, Tools: h.Tools,
		Executor: h.Exec, Ingester: h.Ingester, Publisher: pub, Clock: h.Clock,
		Rng: core.NewSplitMix64(h.Seed),
	}
}

// NewEngine returns another engine instance on the harness ports (a
// separate process, for example a worker that will crash) using store.
func (h *Harness) NewEngine(store eventstore.Store) *engine.Engine {
	e, err := engine.New(h.Deps(store), h.Config)
	if err != nil {
		h.T.Fatal(err)
	}
	return e
}

// Ctx returns the harness context.
func (h *Harness) Ctx() context.Context { return h.ctx }

// Register adds p to the fake compiler under the source "source:<name>"
// and registers it for tenant (zero: tenant A) against program.
func (h *Harness) Register(c engine.Caller, name string, p Program, program core.UUID) engine.WorkflowVersion {
	h.T.Helper()
	src := "source:" + name
	h.Compiler.Add(src, p)
	v, err := h.Engine.RegisterWorkflow(h.ctx, c, engine.RegisterWorkflowRequest{Name: name, Source: src, ProgramID: program})
	if err != nil {
		h.T.Fatalf("register %s: %v", name, err)
	}
	return v
}

// Submit submits a run with the JSON of input.
func (h *Harness) Submit(c engine.Caller, name string, program core.UUID, input any, key string) engine.Run {
	h.T.Helper()
	r, err := h.SubmitErr(c, name, program, input, key, 0)
	if err != nil {
		h.T.Fatalf("submit %s: %v", name, err)
	}
	return r
}

// SubmitErr submits a run and returns the error.
func (h *Harness) SubmitErr(c engine.Caller, name string, program core.UUID, input any, key string, deadline time.Duration) (engine.Run, error) {
	b, err := json.Marshal(input)
	if err != nil {
		h.T.Fatal(err)
	}
	return h.Engine.SubmitRun(h.ctx, c, engine.SubmitRunRequest{
		WorkflowName: name, ProgramID: program, Input: b, IdempotencyKey: key, Deadline: deadline,
	})
}

// Drain advances ready runs with w until none is ready, without moving
// the clock. It returns the number of runs advanced.
func (h *Harness) Drain(w *engine.Worker) int {
	h.T.Helper()
	n := 0
	for i := 0; i < 10_000; i++ {
		ok, err := w.RunOnce(h.ctx)
		if err != nil {
			h.T.Fatalf("worker %s: %v", w.ID(), err)
		}
		if !ok {
			return n
		}
		n++
	}
	h.T.Fatal("Drain: runs never settle")
	return n
}

// RunToEnd advances the run with w, moving the fake clock to each wake
// time, until it is terminal, paused or waits indefinitely. It returns the
// final store status.
func (h *Harness) RunToEnd(w *engine.Worker, run core.UUID) engine.RunStatus {
	h.T.Helper()
	for i := 0; i < 10_000; i++ {
		h.Drain(w)
		info, err := h.Store.GetRun(h.ctx, run)
		if err != nil {
			h.T.Fatal(err)
		}
		if engine.Terminal(info.Status) || info.Status == engine.StatusPaused || info.NextWakeAt.Year() >= 9999 {
			return info.Status
		}
		d := info.NextWakeAt.Sub(h.Clock.Now())
		if info.LeaseOwner != "" {
			d = max(d, h.Engine.Config().LeaseTTL+time.Second)
		}
		h.Clock.Advance(max(d, time.Millisecond))
	}
	h.T.Fatal("RunToEnd: run never finishes")
	return ""
}

// Events returns a run's whole log.
func (h *Harness) Events(run core.UUID) []eventstore.Event {
	h.T.Helper()
	evs, err := h.Store.Read(h.ctx, run, 1, 0)
	if err != nil {
		h.T.Fatal(err)
	}
	return evs
}

// Types returns the event types of a log.
func Types(evs []eventstore.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

// NormalEvent is an event with run-specific values normalized.
type NormalEvent struct {
	Seq     uint64 `json:"seq"`
	Type    string `json:"type"`
	Actor   string `json:"actor"`
	Payload any    `json:"payload"`
}

// Normalize returns the log with timestamps, durations, module hashes and
// the results of now() effects replaced by placeholders, so logs of runs that differ
// only in timing compare equal (spec 25.4 "up to timestamps").
func Normalize(evs []eventstore.Event) []NormalEvent {
	nowPaths := map[string]bool{}
	out := make([]NormalEvent, len(evs))
	for i, e := range evs {
		var p map[string]any
		_ = json.Unmarshal(e.Payload, &p)
		if e.Type == engine.EvEffectScheduled && p["kind"] == "now" {
			nowPaths[fmt.Sprint(p["path"])] = true
		}
		if e.Type == engine.EvEffectCompleted && nowPaths[fmt.Sprint(p["path"])] {
			p["result"], p["value"] = "<time>", "<time>"
		}
		out[i] = NormalEvent{Seq: e.Seq, Type: e.Type, Actor: e.Actor, Payload: normalizeValue(p)}
	}
	return out
}

func normalizeValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			switch {
			case strings.HasSuffix(k, "_at"):
				x[k] = "<time>"
			case k == "duration_ms":
				x[k] = 0
			case k == "module_sha256":
				x[k] = "<module>"
			default:
				x[k] = normalizeValue(val)
			}
		}
		return x
	case []any:
		for i := range x {
			x[i] = normalizeValue(x[i])
		}
	}
	return v
}

// NormalJSON renders normalized events one per line.
func NormalJSON(evs []NormalEvent) string {
	var sb strings.Builder
	sb.WriteString("[\n")
	for i, e := range evs {
		b, _ := core.CanonicalJSON(e)
		sb.Write(b)
		if i < len(evs)-1 {
			sb.WriteByte(',')
		}
		sb.WriteByte('\n')
	}
	sb.WriteString("]\n")
	return sb.String()
}
