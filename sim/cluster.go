package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/alerts"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/eventstore"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/limiter"
	"github.com/genuinebnt/flowforge/oam"
)

// Fixed identities of the simulated deployment.
var (
	simStart  = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	tenantA   = core.MustParseUUID("0190a1b2-0000-7000-8000-00000000a001")
	programA  = core.MustParseUUID("0190a1b2-0000-7000-8000-0000000000a1")
	programB  = core.MustParseUUID("0190a1b2-0000-7000-8000-0000000000b1")
	clientC   = engine.Caller{Tenant: tenantA, Actor: "apikey:sim-client", Roles: []string{"analyst"}}
	approverC = engine.Caller{Tenant: tenantA, Actor: "user:0190a1b2-0000-7000-8000-00000000c002", Roles: []string{"finance_ap", "approver"}}
	operatorC = engine.Caller{Tenant: tenantA, Actor: "user:0190a1b2-0000-7000-8000-00000000c003", Roles: []string{"operator"}}
)

// Per-target ceiling: every registrable domain of a program (rl:h).
const (
	hostRate  = 2.0
	hostBurst = 4
)

// Engine configuration shared by every simulated worker.
var workerConfig = engine.Config{
	LeaseTTL: 30 * time.Second,
	// The watchdog's ticker runs on the wall clock; a period far beyond
	// any test keeps it from ever firing, so no goroutine acts on real
	// time. Each worker is its own Engine, so the API engine's
	// in-process notifications never reach it either: workers learn about
	// cancellations and decisions through the store, at their next
	// advance. Leases are therefore never renewed mid-advance; a slow
	// advance can lose its lease, which is exactly what fencing is for.
	PollInterval:          1 << 62,
	CancelGrace:           time.Second,
	IdleBackoff:           time.Second,
	WorkerConcurrency:     1,
	RateLimitErrorBackoff: time.Second,
	Executor:              "sim-runner",
}

// simWorker is one worker process: an Engine and its Worker, a clock
// offset, and the partition it may be in.
type simWorker struct {
	id        string
	w         *engine.Worker
	skew      time.Duration
	dead      bool
	actor     *actor
	partUntil time.Time
}

type runState struct {
	workflow string
	spec     eventstore.RunSpec
	terminal bool
}

// cluster is one scenario's simulated deployment.
type cluster struct {
	sc        Scenario
	seed      uint64
	prof      FaultProfile
	clock     *simClock
	s         *scheduler
	rng       *core.SplitMix64
	ctx       context.Context
	faultsEnd time.Time
	hist      *History

	store  *eventstore.MemStore
	fstore *faultStore
	repo   faultRepo
	scope  *scopeService
	tools  *toolRegistry
	exec   *toolExec
	lim    simLimiter
	graph  *graphstore.Mem
	notif  *alerts.Processor
	api    *engine.Engine

	op            uint64
	workers       []*simWorker
	workerN       int
	runs          map[string]*runState
	runOrder      []string
	nonterminal   int
	versions      map[core.UUID]string
	pendingPub    int
	obsSeen       map[string]string
	runWorkflow   map[string]string
	pendingClient int
	cancelled     map[core.UUID]bool
	deciding      map[core.UUID]bool
}

func (c *cluster) faulty() bool { return c.clock.Now().Before(c.faultsEnd) }

// frozenClock is the graph stores' clock: covering-run timestamps then do
// not depend on delivery times, so the live graph and the reference graph
// of inv_09 are comparable byte for byte.
type frozenClock struct{}

func (frozenClock) Now() time.Time           { return simStart }
func (frozenClock) Monotonic() time.Duration { return 0 }

func newCluster(sc Scenario) (*cluster, error) {
	prof, err := Profile(sc.Profile)
	if err != nil {
		return nil, err
	}
	clock := newSimClock(simStart)
	c := &cluster{
		sc: sc, seed: sc.Seed, prof: prof, clock: clock, s: newScheduler(clock),
		rng: core.NewSplitMix64(sc.Seed), ctx: context.Background(), faultsEnd: simStart.Add(sc.Duration),
		runs: map[string]*runState{}, versions: map[core.UUID]string{}, obsSeen: map[string]string{}, runWorkflow: map[string]string{},
		cancelled: map[core.UUID]bool{}, deciding: map[core.UUID]bool{},
	}
	c.hist = &History{Seed: sc.Seed, Profile: sc.Profile, Workload: sc.Workload, Start: simStart, FaultsEnd: c.faultsEnd,
		LivenessBound: sc.LivenessBound,
		Compensations: []CompensationPair{{Forward: toolReserve, Undo: toolRelease}},
		RateCeilings:  []RateCeiling{{Prefix: "rl:h:", Rate: hostRate, Burst: hostBurst}}}
	c.store = eventstore.NewMemStore(clock)
	c.fstore = &faultStore{c: c, inner: c.store}
	c.repo = faultRepo{newMemRepo(core.IDGen{Clock: clock, Rng: core.NewSplitMix64(sc.Seed ^ 0x5eed)})}
	c.scope = newScopeService()
	c.tools = newToolRegistry(toolSpecs())
	c.exec = newToolExec(c)
	g, err := limiter.NewGCRA(
		limiter.Rule{Pattern: "rl:h:*", Params: limiter.Params{Rate: hostRate, Burst: hostBurst}},
		limiter.Rule{Pattern: "*", Params: limiter.Params{Rate: 10_000, Burst: 10_000}},
	)
	if err != nil {
		return nil, err
	}
	c.lim = simLimiter{c: c, g: g}
	c.graph = graphstore.NewMem(graphstore.Config{Clock: frozenClock{}})
	if err := c.setupAlerts(); err != nil {
		return nil, err
	}
	for _, p := range []struct {
		id  core.UUID
		doc []string
	}{{programA, []string{"acme.com", "example.com"}}, {programB, []string{"acme.com"}}} {
		if _, err := c.scope.put(tenantA, p.id, docAllowing(p.doc...)); err != nil {
			return nil, err
		}
	}
	c.api, err = engine.New(c.deps(clock, core.NewSplitMix64(sc.Seed^0xa91)), workerConfig)
	if err != nil {
		return nil, err
	}
	for _, w := range workflowSources {
		v, err := c.api.RegisterWorkflow(c.ctx, clientC, engine.RegisterWorkflowRequest{Name: w.name, Source: w.src, ProgramID: programA})
		if err != nil {
			return nil, fmt.Errorf("sim: register %s: %w", w.name, err)
		}
		c.versions[v.ID] = v.Name
		c.hist.Modules = append(c.hist.Modules, Module{VersionID: v.ID.String(), Name: v.Name, Encoded: v.Module, Tools: c.tools.list()})
	}
	return c, nil
}

func (c *cluster) deps(clock core.Clock, rng core.Rng) engine.Deps {
	return engine.Deps{
		Store: c.fstore, Repo: c.repo, Compiler: compilerPort{}, Scope: faultScope{c: c, inner: c.scope},
		Tools: c.tools, Executor: c.exec, Ingester: faultIngester{c: c}, Limiter: c.lim,
		Publisher: faultPublisher{c: c}, Clock: clock, Rng: rng,
	}
}

// ---- workers ----

func (c *cluster) startWorker() {
	c.workerN++
	w := &simWorker{id: fmt.Sprintf("w%d", c.workerN)}
	if c.prof.MaxSkew > 0 {
		w.skew = between(c.rng, -c.prof.MaxSkew, c.prof.MaxSkew)
	}
	e, err := engine.New(c.deps(skewClock{base: c.clock, skew: w.skew}, core.NewSplitMix64(c.rng.Uint64())), workerConfig)
	if err != nil {
		return // impossible: every port is set
	}
	w.w = e.NewWorker(eventstore.WorkerID(w.id))
	c.workers = append(c.workers, w)
	c.s.after(between(c.rng, 0, 500*time.Millisecond), func() { c.tick(w) })
}

// tick lets an idle worker claim and advance one run (Worker.RunOnce).
func (c *cluster) tick(w *simWorker) {
	if w.dead || w.actor != nil {
		return
	}
	a := c.s.newActor(w.id+"/run", w)
	w.actor = a
	claimed := false
	c.s.start(c.ctx, a, func(ctx context.Context) {
		claimed, _ = w.w.RunOnce(ctx)
	}, func() {
		w.actor = nil
		if w.dead {
			return
		}
		next := between(c.rng, 500*time.Millisecond, 1500*time.Millisecond)
		if claimed {
			next = between(c.rng, time.Millisecond, 20*time.Millisecond)
		}
		c.s.after(next, func() { c.tick(w) })
	})
}

// crash kills a worker's process (spec 23.1 crash-stop with restart): its
// actor fails every further call, its leases stay until they expire, and
// a fresh worker (a new process with a new id) starts after a delay. It
// runs on the worker's own actor goroutine or on the scheduler.
func (c *cluster) crash(w *simWorker) {
	if w == nil || w.dead {
		return
	}
	w.dead = true
	c.hist.Stats.Crashes++
	if a := w.actor; a != nil {
		if a.parked != nil {
			c.s.kill(a)
		} else {
			a.crashed.Store(true)
			a.cancel()
		}
	}
	c.s.after(between(c.rng, time.Second, c.prof.RestartAfter), c.startWorker)
}

func (c *cluster) liveWorkers() []*simWorker {
	var out []*simWorker
	for _, w := range c.workers {
		if !w.dead {
			out = append(out, w)
		}
	}
	return out
}

// scheduleFaults plans the time-driven faults: crashes at random instants
// and partitions.
func (c *cluster) scheduleFaults() {
	if c.prof.CrashEvery > 0 {
		var next func()
		next = func() {
			if !c.faulty() {
				return
			}
			if ws := c.liveWorkers(); len(ws) > 0 {
				c.crash(ws[core.IntN(c.rng, len(ws))])
			}
			c.s.after(c.expDelay(c.prof.CrashEvery), next)
		}
		c.s.after(c.expDelay(c.prof.CrashEvery), next)
	}
	if c.prof.PartitionEvery > 0 {
		var next func()
		next = func() {
			if !c.faulty() {
				return
			}
			if ws := c.liveWorkers(); len(ws) > 0 {
				w := ws[core.IntN(c.rng, len(ws))]
				w.partUntil = c.clock.Now().Add(between(c.rng, 10*time.Second, c.prof.PartitionFor))
				c.hist.Stats.Partitions++
			}
			c.s.after(c.expDelay(c.prof.PartitionEvery), next)
		}
		c.s.after(c.expDelay(c.prof.PartitionEvery), next)
	}
}

// expDelay draws an exponentially distributed delay with the given mean
// (capped at 4x).
func (c *cluster) expDelay(mean time.Duration) time.Duration {
	u := core.Float64(c.rng)
	d := time.Duration(-float64(mean) * math.Log(1-u))
	return min(max(d, time.Second), 4*mean)
}

// ---- API actors ----

// apiCall runs body as an API actor (a gateway request): its store and
// scope calls go through the fault adapter like a worker's.
func (c *cluster) apiCall(name string, body func(ctx context.Context)) {
	a := c.s.newActor(name, nil)
	c.s.start(c.ctx, a, body, nil)
}

// ---- runs ----

func (c *cluster) addRun(spec eventstore.RunSpec) {
	id := spec.ID.String()
	if _, ok := c.runs[id]; ok {
		return
	}
	c.runs[id] = &runState{workflow: c.versions[spec.WorkflowVersion], spec: spec}
	c.runWorkflow[id] = c.versions[spec.WorkflowVersion]
	c.runOrder = append(c.runOrder, id)
	c.nonterminal++
}

func (c *cluster) markTerminal(id string) {
	if r := c.runs[id]; r != nil && !r.terminal {
		r.terminal = true
		c.nonterminal--
	}
}

// settled reports whether every run's store row is terminal: its last
// worker released it (and so handed its events to the outbox).
func (c *cluster) settled() bool {
	for _, id := range c.runOrder {
		run, _ := core.ParseUUID(id)
		info, err := c.store.GetRun(c.ctx, run)
		if err != nil || !engine.Terminal(info.Status) {
			return false
		}
	}
	return true
}

// ---- Pub/Sub ----

// deliver hands a message to the simulated Pub/Sub: it is delivered at
// least once, after a random delay (which reorders messages), and while
// faults are on possibly several times.
func (c *cluster) deliver(fn func()) {
	copies := 1
	if c.faulty() && chance(c.rng, c.prof.PubDupP) {
		copies += 1 + core.IntN(c.rng, max(c.prof.PubMaxCopies-1, 1))
	}
	for i := range copies {
		d := time.Duration(0)
		if c.prof.PubDelay > 0 {
			d = between(c.rng, 0, c.prof.PubDelay)
		}
		c.pendingPub++
		c.s.after(d, func() {
			c.pendingPub--
			c.hist.Stats.PubDeliveries++
			if i > 0 {
				c.hist.Stats.PubDuplicates++
			}
			fn()
		})
	}
}

// acceptObservation records an envelope accepted by the graph ingester
// and publishes it to the graph consumer.
func (c *cluster) acceptObservation(o Observation) {
	if prev, ok := c.obsSeen[o.ID]; ok && prev == string(o.Raw) {
		// The same envelope again (a re-executed call): still delivered,
		// the graph must ignore it.
	} else {
		c.obsSeen[o.ID] = string(o.Raw)
		c.hist.Observations = append(c.hist.Observations, o)
	}
	c.deliver(func() { c.applyObservation(c.graph, o) })
}

// applyObservation ingests one envelope into a graph store.
func (c *cluster) applyObservation(g *graphstore.Mem, o Observation) {
	obs, err := oam.DecodeObservation(o.Raw, oam.Lenient)
	if err != nil {
		return
	}
	_, _ = g.IngestObservation(c.ctx, graphstore.IngestRequest{TenantID: o.Tenant, ProgramID: o.Program, RunID: o.Run,
		EffectPath: o.Path, ToolName: o.Tool, ToolVersion: o.ToolVersion, Observation: obs})
}

// Alert rules of the notifier: one alert per alert_emitted event (its
// group_by makes every event its own dedupe key) and the default
// run-failure rule, throttled per (workflow, code).
const (
	ruleEmitted   = "alr_sim_emitted"
	ruleRunFailed = "alr_ops_run_failed"
	srcEmitted    = `alert sim_emitted
  when event.type == "run.alert_emitted"
  -> slack()
  severity = High  group_by run.id, run.path`
)

func (c *cluster) setupAlerts() error {
	rs := alerts.NewRuleSet(alerts.RenderOptions{})
	for _, r := range []struct{ id, src string }{{ruleEmitted, srcEmitted}, {ruleRunFailed, alerts.DefaultRuleRunFailed}} {
		rule, diags := alerts.Compile(r.src)
		if rule == nil {
			return fmt.Errorf("sim: alert rule %s: %v", r.id, diags)
		}
		if err := rs.Add(r.id, rule); err != nil {
			return err
		}
	}
	c.hist.AlertRules = []AlertRule{
		{ID: ruleEmitted, EventType: engine.EvAlertEmitted},
		{ID: ruleRunFailed, EventType: engine.EvRunFailed, Throttle: 15 * time.Minute},
	}
	p, err := alerts.NewProcessor(rs, alerts.ProcessorConfig{Clock: c.clock, Store: alerts.NewMemProcessedStore()})
	if err != nil {
		return err
	}
	c.notif = p
	return nil
}

// onRunEvent is the notifier's consumer of run.events (spec 15.1): the
// envelope becomes an alerts.Event; the processor's inbox drops
// redeliveries by event id.
func (c *cluster) onRunEvent(env engine.Envelope) {
	var m struct {
		RunID   string          `json:"run_id"`
		Seq     uint64          `json:"seq"`
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(env.Payload, &m) != nil {
		return
	}
	var p struct {
		Path    string `json:"path"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Error   struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(m.Payload, &p)
	if p.Code == "" {
		p.Code, p.Message = p.Error.Code, p.Error.Message
	}
	ev := &alerts.Event{
		ID: env.EventID.String(), Type: env.Type, TenantID: env.Tenant.String(), ProgramID: env.Program.String(),
		OccurredAt: env.OccurredAt,
		Run: &alerts.Run{ID: m.RunID, Seq: int64(m.Seq), Type: m.Type, WorkflowName: c.runWorkflow[m.RunID],
			Code: p.Code, Message: p.Message, Path: p.Path},
	}
	out, err := c.notif.Process(c.ctx, ev)
	if err != nil || out.Duplicate {
		return
	}
	for _, a := range out.Alerts {
		c.hist.Alerts = append(c.hist.Alerts, AlertRecord{At: c.clock.Now(), Rule: a.RuleID, DedupeKey: a.DedupeKey,
			TriggerEventID: a.TriggerEventID, Run: m.RunID, Seq: m.Seq, Suppressed: a.Status == alerts.StatusSuppressed})
		if a.Status != alerts.StatusSuppressed {
			c.hist.Stats.Alerts++
		}
	}
}

// ---- end of scenario ----

func (c *cluster) finish() {
	h := c.hist
	h.End = c.clock.Now()
	h.Stats.Steps = c.s.steps
	h.Scopes = slices.Clone(c.scope.history)
	for _, id := range c.runOrder {
		r := c.runs[id]
		run, _ := core.ParseUUID(id)
		evs, _ := c.store.Read(c.ctx, run, 1, 0)
		info, _ := c.store.GetRun(c.ctx, run)
		rec := RunRecord{ID: id, Workflow: r.workflow, WorkflowVersion: r.spec.WorkflowVersion.String(),
			Tenant: r.spec.Tenant.String(), Program: r.spec.Program.String(), IdempotencyKey: r.spec.IdempotencyKey,
			Input: r.spec.Input, Status: string(info.Status), Log: evs}
		h.Runs = append(h.Runs, rec)
		h.Stats.Runs++
		h.Stats.Events += len(evs)
		if len(evs) > 0 {
			switch evs[len(evs)-1].Type {
			case engine.EvRunSucceeded:
				h.Stats.Succeeded++
			case engine.EvRunFailed:
				h.Stats.Failed++
			case engine.EvRunCancelled:
				h.Stats.Cancelled++
			}
		}
	}
	for _, c2 := range h.ToolCalls {
		if c2.Delivered {
			h.Stats.ToolDeliveries++
		}
	}
	h.Stats.Observations = len(h.Observations)
	h.Graphs = graphStates(c.ctx, c.graph, h.Observations)
	h.Stats.ToolReruns = countReruns(h.ToolCalls)
}

// graphStates exports every program the observations touched.
func graphStates(ctx context.Context, g *graphstore.Mem, obs []Observation) []GraphState {
	type tp struct{ t, p string }
	var progs []tp
	for _, o := range obs {
		k := tp{o.Tenant, o.Program}
		if !slices.Contains(progs, k) {
			progs = append(progs, k)
		}
	}
	slices.SortFunc(progs, func(a, b tp) int { return strings.Compare(a.t+a.p, b.t+b.p) })
	var out []GraphState
	for _, p := range progs {
		ex, err := g.Export(ctx, graphstore.Caller{TenantID: p.t}, p.p)
		s := ""
		if err != nil {
			s = "error: " + err.Error()
		} else if b, err := core.CanonicalJSON(ex); err == nil {
			s = string(b)
		}
		out = append(out, GraphState{Tenant: p.t, Program: p.p, Export: s})
	}
	return out
}

func countReruns(calls []ToolCall) int {
	seen := map[string]bool{}
	n := 0
	for _, c := range calls {
		if !c.Delivered {
			continue
		}
		if seen[c.Key] {
			n++
		}
		seen[c.Key] = true
	}
	return n
}
