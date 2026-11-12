package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/oam"
)

// Options configures a store built by a Factory.
type Options struct {
	Scope          oam.ScopeFunc
	StaleAfterRuns int
	Clock          core.Clock
}

// Config returns o as a graphstore.Config.
func (o Options) Config() graphstore.Config {
	return graphstore.Config{Scope: o.Scope, StaleAfterRuns: o.StaleAfterRuns, Clock: o.Clock}
}

// Factory returns a new, empty store. Cleanup is the factory's business
// (t.Cleanup).
type Factory func(t testing.TB, o Options) graphstore.Store

// Clock is a settable core.Clock for the suite.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock returns a clock reading t.
func NewClock(t time.Time) *Clock { return &Clock{t: t} }

// Now implements core.Clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Monotonic implements core.Clock.
func (c *Clock) Monotonic() time.Duration { return 0 }

// Set moves the clock.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// T0 is the base time of the suite's fixtures.
var T0 = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

// NamedID maps a fixture name to a canonical id with prefix p: a
// well-formed id is kept, any other name is replaced by an id derived from
// it. Conformance data uses names like "p" and "r1".
func NamedID(p core.Prefix, name string) string {
	if id, err := graphstore.ParseID(p, "", name); err == nil {
		return id.Text
	}
	return graphstore.IDFromUUID(p, graphstore.DeriveUUID("ff-storetest", string(p), name)).Text
}

// TenantID maps a fixture tenant name to a canonical ten_ id.
func TenantID(name string) string { return NamedID(core.PrefixTenant, "t:"+name) }

// ProgramID maps a fixture program name to a canonical prg_ id.
func ProgramID(name string) string { return NamedID(core.PrefixProgram, "p:"+name) }

// RunID maps a fixture run name to a canonical run_ id ("" stays "").
func RunID(name string) string {
	if name == "" {
		return ""
	}
	return NamedID(core.PrefixRun, name)
}

// env bundles a store with a caller and context for one test.
type env struct {
	t      testing.TB
	s      graphstore.Store
	clock  *Clock
	ctx    context.Context
	tenant string
	prog   string
	c      graphstore.Caller
}

func newEnv(t testing.TB, f Factory, o Options) *env {
	t.Helper()
	clock := NewClock(T0)
	if o.Clock == nil {
		o.Clock = clock
	}
	tenant := TenantID("acme")
	return &env{t: t, s: f(t, o), clock: clock, ctx: context.Background(), tenant: tenant, prog: ProgramID("main"),
		c: graphstore.Caller{TenantID: tenant}}
}

func (e *env) ingest(o oam.Observation) graphstore.IngestResult {
	e.t.Helper()
	res, err := e.s.IngestObservation(e.ctx, graphstore.IngestRequest{TenantID: e.tenant, ProgramID: e.prog, Observation: o})
	if err != nil {
		e.t.Fatalf("ingest %s: %v", o.ObservationID, err)
	}
	return res
}

func (e *env) export() graphstore.Export {
	e.t.Helper()
	ex, err := e.s.Export(e.ctx, e.c, e.prog)
	if err != nil {
		e.t.Fatalf("export: %v", err)
	}
	return ex
}

func (e *env) entity(id string) graphstore.Entity {
	e.t.Helper()
	ent, err := e.s.GetEntity(e.ctx, e.c, graphstore.GetEntityRequest{EntityID: id, IncludeTags: true})
	if err != nil {
		e.t.Fatalf("get entity %s: %v", id, err)
	}
	return ent
}

func (e *env) changes(after int64) []graphstore.ChangeEvent {
	e.t.Helper()
	var all []graphstore.ChangeEvent
	for {
		evs, err := e.s.ReadChanges(e.ctx, e.c, graphstore.ChangesRequest{ProgramID: e.prog, AfterSeq: after, Limit: graphstore.MaxPageSize})
		if err != nil {
			e.t.Fatalf("read changes: %v", err)
		}
		if len(evs) == 0 {
			return all
		}
		all = append(all, evs...)
		after = evs[len(evs)-1].Seq
	}
}

// Canon returns the canonical JSON of v.
func Canon(t testing.TB, v any) string {
	t.Helper()
	b, err := core.CanonicalJSON(v)
	if err != nil {
		t.Fatalf("canonical JSON: %v", err)
	}
	return string(b)
}

func wantKind(t testing.TB, what string, err error, kind core.Kind) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error, want %v", what, kind)
	}
	if got := core.KindOf(err); got != kind {
		t.Fatalf("%s: error %v has kind %v, want %v", what, err, got, kind)
	}
}

func isNotFound(err error) bool { return errors.Is(err, core.ErrNotFound) }

// fqdn, ip, ... build fixture observations.
func ent(ref string, a oam.Asset) oam.ObservedEntity { return oam.ObservedEntity{Ref: ref, Asset: a} }

func dns(from, to string, rr int) oam.ObservedEdge {
	return oam.ObservedEdge{From: from, To: to, Relation: oam.BasicDNSRelation{Label: "dns_record", Header: oam.DNSHeader{RRType: rr, Class: 1, TTL: 300}}}
}

func simple(from, to, label string) oam.ObservedEdge {
	return oam.ObservedEdge{From: from, To: to, Relation: oam.SimpleRelation{Label: label}}
}

func port(from, to string, n int) oam.ObservedEdge {
	return oam.ObservedEdge{From: from, To: to, Relation: oam.PortRelation{Label: "port", PortNumber: n, Protocol: "tcp"}}
}

func obs(id, tool string, at time.Time, conf float64, ents []oam.ObservedEntity, edges ...oam.ObservedEdge) oam.Observation {
	return oam.Observation{ObservationID: id, Tool: oam.Tool{Name: tool}, ObservedAt: at, Confidence: conf, Entities: ents, Edges: edges}
}
