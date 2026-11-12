package oam_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/oam"
)

// TestOAM002_MergeTwoObservations checks spec 25.3 oam_002: created_at is
// the minimum, last_seen the maximum, tags the union, and confidence the
// noisy-or over distinct sources.
func TestOAM002_MergeTwoObservations(t *testing.T) {
	g := oam.NewGraph(oam.Config{})
	late := obs("late", "amass", t0.Add(time.Hour), 0.7, ent("a", oam.FQDN{Name: "shop.acme.com"}))
	late.Tags = []oam.ObservedTag{{Target: "a", Property: oam.SimpleProperty{Name: "cdn", Value: "fastly"}}}
	early := obs("early", "crtsh", t0, 0.6, ent("a", oam.FQDN{Name: "SHOP.acme.com."}))
	early.Tags = []oam.ObservedTag{{Target: "a", Property: oam.VulnProperty{ID: "CVE-2024-0001"}}}
	mustApply(t, g, late)
	r := mustApply(t, g, early)
	if r.EntitiesCreated != 0 || r.EntitiesUpdated != 1 {
		t.Fatalf("second observation created %d, updated %d", r.EntitiesCreated, r.EntitiesUpdated)
	}
	e := mustLookup(t, g, oam.FQDN{Name: "shop.acme.com"})
	if !e.CreatedAt.Equal(t0) || !e.LastSeen.Equal(t0.Add(time.Hour)) {
		t.Errorf("created_at %v last_seen %v", e.CreatedAt, e.LastSeen)
	}
	if e.FirstSeenRun != "run-early" || e.LastSeenRun != "run-late" {
		t.Errorf("runs %q %q", e.FirstSeenRun, e.LastSeenRun)
	}
	want := 1 - (1-0.7)*(1-0.6)
	if math.Abs(e.Confidence-want) > 1e-9 {
		t.Errorf("confidence %v, want %v", e.Confidence, want)
	}
	var types []string
	for _, tg := range e.Tags {
		types = append(types, tg.PropertyType+"/"+tg.Name)
	}
	if got := len(types); got != 4 {
		t.Fatalf("tags %v, want 2 sources + cdn + CVE", types)
	}
}

// TestOAM003_SameSourceCountedOnce checks spec 25.3 oam_003.
func TestOAM003_SameSourceCountedOnce(t *testing.T) {
	g := oam.NewGraph(oam.Config{})
	a := oam.IPAddress{Address: "198.51.100.7"}
	mustApply(t, g, obs("1", "naabu", t0, 0.4, ent("x", a)))
	mustApply(t, g, obs("2", "naabu", t0.Add(time.Minute), 0.7, ent("x", a)))
	mustApply(t, g, obs("3", "naabu", t0.Add(2*time.Minute), 0.5, ent("x", a)))
	e := mustLookup(t, g, a)
	if e.Confidence != 0.7 {
		t.Fatalf("confidence %v, want 0.7 (one source, its maximum)", e.Confidence)
	}
	if got := oam.CombineConfidence(
		oam.SourceReport{Source: "naabu", Confidence: 0.4},
		oam.SourceReport{Source: "naabu", Confidence: 0.7},
	); got != 0.7 {
		t.Fatalf("CombineConfidence %v, want 0.7", got)
	}
}

// TestOAM005_Staleness checks spec 25.3 oam_005: an entity missed by
// stale_after_runs covering runs is marked stale and a fresh observation
// clears it.
func TestOAM005_Staleness(t *testing.T) {
	g := oam.NewGraph(oam.Config{StaleAfterRuns: 2})
	keep, drop := oam.FQDN{Name: "keep.acme.com"}, oam.FQDN{Name: "drop.acme.com"}
	run := func(i int, assets ...oam.Asset) {
		o := obs("run"+string(rune('0'+i)), "dnsx", t0.Add(time.Duration(i)*time.Hour), 0.9)
		o.RunID = "r" + string(rune('0'+i))
		for j, a := range assets {
			o.Entities = append(o.Entities, ent(string(rune('a'+j)), a))
		}
		mustApply(t, g, o)
		if _, err := g.CompleteRun(oam.CoveringRun{TenantID: "ten", ProgramID: "prg", RunID: o.RunID, Tool: "dnsx", CompletedAt: o.ObservedAt}); err != nil {
			t.Fatal(err)
		}
	}
	run(1, keep, drop)
	run(2, keep)
	if e := mustLookup(t, g, drop); e.State != oam.StateActive {
		t.Fatalf("stale after one missed run")
	}
	run(3, keep)
	if e := mustLookup(t, g, drop); e.State != oam.StateStale {
		t.Fatalf("state %s after two missed runs, want stale", e.State)
	}
	if e := mustLookup(t, g, keep); e.State != oam.StateActive {
		t.Fatalf("kept entity is %s", e.State)
	}
	r := mustApply(t, g, obs("fresh", "subfinder", t0.Add(5*time.Hour), 0.5, ent("a", drop)))
	if e := mustLookup(t, g, drop); e.State != oam.StateActive {
		t.Fatalf("fresh observation did not reactivate")
	}
	if len(r.Changes) != 1 || r.Changes[0].Type != oam.ChangeEntityReactivated {
		t.Fatalf("changes %+v", r.Changes)
	}
	if res, err := g.CompleteRun(oam.CoveringRun{TenantID: "ten", ProgramID: "prg", RunID: "r3", Tool: "dnsx"}); err != nil || !res.Duplicate {
		t.Fatalf("repeated covering run: %+v %v", res, err)
	}
	if _, err := g.CompleteRun(oam.CoveringRun{Tool: "dnsx"}); !errors.Is(err, oam.ErrInvalidRun) {
		t.Fatalf("run without id: %v", err)
	}
}

func TestGraphApplyIsAtomicAndIdempotent(t *testing.T) {
	g := oam.NewGraph(oam.Config{})
	bad := obs("bad", "x", t0, 0.5, ent("a", oam.FQDN{Name: "a.acme.com"}), ent("b", oam.FQDN{Name: "b..acme.com"}))
	if _, err := g.Apply(bad); !errors.Is(err, oam.ErrInvalidAsset) || !errors.Is(err, oam.ErrInvalidObservation) {
		t.Fatalf("bad asset: %v", err)
	}
	if n := len(g.Entities()); n != 0 {
		t.Fatalf("failed observation left %d entities", n)
	}
	good := obs("bad", "x", t0, 0.5, ent("a", oam.FQDN{Name: "a.acme.com"}))
	if r := mustApply(t, g, good); r.Duplicate || r.EntitiesCreated != 1 {
		t.Fatalf("a failed id must not count as applied: %+v", r)
	}
	before := canon(t, g)
	if r := mustApply(t, g, good); !r.Duplicate {
		t.Fatal("second apply not reported as duplicate")
	}
	if canon(t, g) != before || len(g.Changes()) != 1 {
		t.Fatal("duplicate changed state")
	}
}

func TestGraphTenantIsolation(t *testing.T) {
	g := oam.NewGraph(oam.Config{})
	r := mustApply(t, g, obs("1", "x", t0, 0.5, ent("a", oam.FQDN{Name: "a.acme.com"})))
	id := r.RefToEntityID["a"]
	o := obs("2", "x", t0, 0.5, ent("u", oam.URL{URL: "https://a.acme.com/"}))
	o.TenantID = "other"
	o.Edges = []oam.ObservedEdge{{From: "u", To: id, Relation: oam.SimpleRelation{Label: "domain"}}}
	if _, err := g.Apply(o); !errors.Is(err, oam.ErrUnknownEntity) {
		t.Fatalf("cross-tenant edge: %v", err)
	}
	// Same observation id in another tenant is a different observation.
	o2 := obs("1", "x", t0, 0.5, ent("a", oam.FQDN{Name: "a.acme.com"}))
	o2.TenantID = "other"
	if r := mustApply(t, g, o2); r.Duplicate || r.RefToEntityID["a"] == id {
		t.Fatalf("tenants share state: %+v", r)
	}
}

func TestGraphScopeStatusChanged(t *testing.T) {
	// A content-dependent scope rule: services are in scope once their
	// service_type is known to be https.
	scope := func(_, _ string, a oam.Asset) oam.ScopeStatus {
		if s, ok := a.(oam.Service); ok && s.ServiceType == "https" {
			return oam.ScopeInScope
		}
		return "bogus" // invalid answers mean unknown
	}
	g := oam.NewGraph(oam.Config{Scope: scope})
	mustApply(t, g, obs("1", "naabu", t0, 0.5, ent("s", oam.Service{UniqueID: "abc"})))
	if e := mustLookup(t, g, oam.Service{UniqueID: "abc"}); e.ScopeStatus != oam.ScopeUnknown {
		t.Fatalf("scope %s, want unknown", e.ScopeStatus)
	}
	r := mustApply(t, g, obs("2", "httpx", t0.Add(time.Minute), 0.9, ent("s", oam.Service{UniqueID: "ABC", ServiceType: "https"})))
	if len(r.Changes) != 2 || r.Changes[0].Type != oam.ChangeEntityChanged || r.Changes[1].Type != oam.ChangeScopeStatusChanged {
		t.Fatalf("changes %+v", r.Changes)
	}
	if e := mustLookup(t, g, oam.Service{UniqueID: "abc"}); e.ScopeStatus != oam.ScopeInScope {
		t.Fatalf("scope %s, want in_scope", e.ScopeStatus)
	}
}

func TestGraphEdgeTagsAndViews(t *testing.T) {
	g := oam.NewGraph(oam.Config{})
	o := obs("1", "dnsx", t0, 0.8, ent("a", oam.FQDN{Name: "mail.acme.com"}), ent("m", oam.FQDN{Name: "mx.acme.com"}))
	o.Edges = []oam.ObservedEdge{{From: "a", To: "m", Relation: oam.PrefDNSRelation{Label: "DNS_Record", Header: oam.DNSHeader{RRType: 15, Class: 1, TTL: 60}, Preference: 10}}}
	mustApply(t, g, o)
	edges := g.Edges()
	if len(edges) != 1 || edges[0].Label != "dns_record" || edges[0].Discriminator != "rr15" {
		t.Fatalf("edges %+v", edges)
	}
	rel, err := edges[0].Relation()
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := rel.(oam.PrefDNSRelation); !ok || p.Preference != 10 || p.Header.TTL != 60 {
		t.Fatalf("relation %#v", rel)
	}
	tagged := obs("2", "analyst", t0.Add(time.Minute), 1)
	tagged.Tags = []oam.ObservedTag{{Target: edges[0].ID, Property: oam.FlowforgeProperty{Name: "note", Value: map[string]any{"rank": 1}, Kind: oam.KindHypothesis}}}
	mustApply(t, g, tagged)
	e, _ := g.Edge(edges[0].ID)
	// Tagging is not a sighting: the analyst does not become a source.
	if len(e.Tags) != 2 || e.Confidence != 0.8 || !e.LastSeen.Equal(t0) {
		t.Fatalf("edge %+v", e)
	}
	p, err := e.Tags[0].Property()
	if err != nil {
		t.Fatal(err)
	}
	if fp, ok := p.(oam.FlowforgeProperty); !ok || fp.Kind != oam.KindHypothesis {
		t.Fatalf("tag property %#v", p)
	}
	ent0 := g.Entities()[0]
	ent0.Content["name"] = "mutated"
	if g.Entities()[0].Content["name"] == "mutated" {
		t.Fatal("view aliases graph state")
	}
	if a, err := ent0.Asset(); err != nil || a.AssetType() != oam.TypeFQDN {
		t.Fatalf("asset %v %v", a, err)
	}
}

func TestGraphIDsAreDerived(t *testing.T) {
	g := oam.NewGraph(oam.Config{})
	r := mustApply(t, g, obs("1", "x", t0, 0.5, ent("a", oam.FQDN{Name: "a.acme.com"})))
	want := oam.EntityID("ten", "prg", oam.TypeFQDN, "fqdn:a.acme.com")
	if r.RefToEntityID["a"] != want {
		t.Fatalf("id %s, want %s", r.RefToEntityID["a"], want)
	}
	if oam.EntityID("ten", "prg", oam.TypeFQDN, "fqdn:b.acme.com") == want ||
		oam.EntityID("ten", "prg2", oam.TypeFQDN, "fqdn:a.acme.com") == want {
		t.Fatal("ids collide")
	}
}
