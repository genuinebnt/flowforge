package oam_test

import (
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/testkit"
)

func pick[T any](r core.Rng, xs ...T) T { return xs[core.IntN(r, len(xs))] }

// arbAsset draws from a small universe with several spellings per asset
// and conflicting optional fields, so merges collide often.
func arbAsset(r core.Rng) oam.Asset {
	host := pick(r, "api", "www", "mail")
	switch core.IntN(r, 5) {
	case 0:
		return oam.FQDN{Name: pick(r, host+".acme.com", host+".ACME.com.", " "+host+".acme.com")}
	case 1:
		return oam.IPAddress{Address: pick(r, "203.0.113.10", "::ffff:203.0.113.10", "2001:db8::1")}
	case 2:
		return oam.URL{URL: pick(r, "https://"+host+".acme.com/", "HTTPS://"+host+".acme.com:443/"),
			Scheme: pick(r, "", "https"), Options: pick(r, "", "a=1", "a=2"), Fragment: pick(r, "", "top")}
	case 3:
		return oam.Service{UniqueID: pick(r, "s1", "s2"), ServiceType: pick(r, "", "http", "https"),
			Output: pick(r, "", "HTTP/1.1 200", "HTTP/1.1 404"), OutputLength: pick(r, 0, 12, 99),
			Attributes: pick(r, map[string][]string(nil), map[string][]string{"server": {"nginx"}})}
	default:
		return oam.Opaque{Type: oam.TypeProduct, Content: map[string]any{"name": pick(r, "nginx", "apache")}}
	}
}

func arbObservation(r core.Rng, i int) oam.Observation {
	o := oam.Observation{
		ObservationID: fmt.Sprintf("obs-%d", i), TenantID: "ten", ProgramID: "prg",
		RunID:      pick(r, "run-1", "run-2", "run-3"),
		Tool:       oam.Tool{Name: pick(r, "subfinder", "dnsx", "httpx")},
		ObservedAt: t0.Add(time.Duration(core.IntN(r, 4)) * time.Minute),
		Confidence: pick(r, 0.0, 0.3, 0.5, 0.8, 0.95, 1.0),
	}
	types := map[string]string{}
	for j := range 1 + core.IntN(r, 4) {
		ref := fmt.Sprint("r", j)
		a := arbAsset(r)
		types[ref] = a.AssetType()
		o.Entities = append(o.Entities, ent(ref, a))
	}
	n := len(o.Entities)
	for range core.IntN(r, 3) {
		from, to := fmt.Sprint("r", core.IntN(r, n)), fmt.Sprint("r", core.IntN(r, n))
		var rel oam.Relation = oam.SimpleRelation{Label: pick(r, "related_to", "seen_with")}
		if types[from] == oam.TypeFQDN && (types[to] == oam.TypeIPAddress || types[to] == oam.TypeFQDN) {
			rel = oam.BasicDNSRelation{Label: "dns_record", Header: oam.DNSHeader{RRType: 1, Class: 1, TTL: pick(r, 60, 300)}}
		}
		o.Edges = append(o.Edges, oam.ObservedEdge{From: from, To: to, Relation: rel})
	}
	for range core.IntN(r, 3) {
		target := fmt.Sprint("r", core.IntN(r, n))
		var p oam.Property = oam.SimpleProperty{Name: pick(r, "title", "note"), Value: pick(r, "x", "y", "")}
		if core.IntN(r, 3) == 0 {
			p = oam.SourceProperty{Name: pick(r, "crtsh", "dnsdumpster"), Confidence: pick(r, 40, 70)}
		}
		o.Tags = append(o.Tags, oam.ObservedTag{Target: target, Property: p})
	}
	// Edges and tags naming an existing-entity id (G-CORE-03): the entity
	// may come from a later observation, an earlier one, or none at all.
	for range core.IntN(r, 3) {
		id := entityIDOf(o.TenantID, o.ProgramID, arbAsset(r))
		if core.IntN(r, 2) == 0 {
			o.Edges = append(o.Edges, oam.ObservedEdge{From: fmt.Sprint("r", core.IntN(r, n)), To: id,
				Relation: oam.SimpleRelation{Label: pick(r, "related_to", "seen_with")}})
		} else {
			o.Tags = append(o.Tags, oam.ObservedTag{Target: id, Property: oam.SimpleProperty{Name: "ref", Value: pick(r, "x", "y")}})
		}
	}
	return o
}

// entityIDOf returns the ent_ id a's entity gets in (tenant, program),
// read back from a scratch graph.
func entityIDOf(tenant, program string, a oam.Asset) string {
	g := oam.NewGraph(oam.Config{})
	res, err := g.Apply(oam.Observation{ObservationID: "id", TenantID: tenant, ProgramID: program,
		Tool: oam.Tool{Name: "x"}, ObservedAt: t0, Entities: []oam.ObservedEntity{ent("x", a)}})
	if err != nil {
		panic(err) // arbAsset only draws valid assets
	}
	return res.RefToEntityID["x"]
}

func applyAll(t testing.TB, cfg oam.Config, list []oam.Observation) string {
	t.Helper()
	g := oam.NewGraph(cfg)
	for _, o := range list {
		if _, err := g.Apply(o); err != nil {
			t.Fatalf("apply %s: %v", o.ObservationID, err)
		}
	}
	return canon(t, g)
}

// TestOAMProp_MergeConverges checks spec 25.4 / 23.5: any permutation and
// any duplication of an observation list gives byte-identical state,
// including observations that reference entity ids ingested later
// (pending references, G-CORE-03).
func TestOAMProp_MergeConverges(t *testing.T) {
	r := testkit.Rng(t)
	cfg := oam.Config{Scope: func(_, _ string, a oam.Asset) oam.ScopeStatus {
		if s, ok := a.(oam.Service); ok && s.ServiceType == "https" {
			return oam.ScopeInScope
		}
		return oam.ScopeUnknown
	}}
	for c := range testkit.Cases() {
		list := make([]oam.Observation, core.IntN(r, 30))
		for i := range list {
			list[i] = arbObservation(r, i)
		}
		base := applyAll(t, cfg, list)
		shuffled := append([]oam.Observation(nil), list...)
		core.Shuffle(r, len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		for range core.IntN(r, 10) {
			if len(list) > 0 {
				shuffled = append(shuffled, list[core.IntN(r, len(list))])
			}
		}
		core.Shuffle(r, len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		if got := applyAll(t, cfg, shuffled); got != base {
			t.Fatalf("case %d: state depends on order or duplication\nbase %s\ngot  %s", c, base, got)
		}
	}
}

func arbReports(r core.Rng) []oam.SourceReport {
	out := make([]oam.SourceReport, core.IntN(r, 6))
	for i := range out {
		out[i] = oam.SourceReport{Source: pick(r, "a", "b", "c", "d"), Confidence: float64(core.IntN(r, 101)) / 100}
	}
	return out
}

func sourcesOfReports(rs []oam.SourceReport) oam.Sources {
	s := oam.Sources{}
	for _, x := range rs {
		s.Observe(x.Source, x.Confidence)
	}
	return s
}

// TestOAMProp_ConfidenceLaws checks that confidence combination is
// commutative, associative and idempotent, bounded by MaxConfidence and
// monotone in the reports.
func TestOAMProp_ConfidenceLaws(t *testing.T) {
	r := testkit.Rng(t)
	for range testkit.Cases() {
		a, b, c := arbReports(r), arbReports(r), arbReports(r)
		cat := func(xs ...[]oam.SourceReport) []oam.SourceReport {
			var out []oam.SourceReport
			for _, x := range xs {
				out = append(out, x...)
			}
			return out
		}
		ab := oam.CombineConfidence(cat(a, b)...)
		if ba := oam.CombineConfidence(cat(b, a)...); ab != ba {
			t.Fatalf("not commutative: %v vs %v", ab, ba)
		}
		if aa := oam.CombineConfidence(cat(a, a)...); aa != oam.CombineConfidence(a...) {
			t.Fatalf("not idempotent: %v", aa)
		}
		sa, sb, sc := sourcesOfReports(a), sourcesOfReports(b), sourcesOfReports(c)
		l, rr := sa.Union(sb).Union(sc), sa.Union(sb.Union(sc))
		if !maps.Equal(l, rr) || l.Confidence() != rr.Confidence() {
			t.Fatalf("not associative: %v vs %v", l, rr)
		}
		if !maps.Equal(sa.Union(sa), sa) {
			t.Fatal("union not idempotent")
		}
		if ab < oam.CombineConfidence(a...) || ab > oam.MaxConfidence || ab < 0 {
			t.Fatalf("bounds or monotonicity broken: %v", ab)
		}
	}
}

// TestOAMProp_ContentJoinLaws checks the per-field last-writer-wins join.
func TestOAMProp_ContentJoinLaws(t *testing.T) {
	r := testkit.Rng(t)
	arb := func() oam.Content {
		c := oam.Content{}
		for range core.IntN(r, 4) {
			v := map[string]any{pick(r, "a", "b", "c"): pick[any](r, "x", "y", "", nil, []any{"z"})}
			c.Merge(v, oam.Stamp{At: t0.Add(time.Duration(core.IntN(r, 3)) * time.Second), Source: pick(r, "s", "t"), Observation: pick(r, "1", "2")})
		}
		return c
	}
	eqC := func(x, y oam.Content) bool {
		return len(x.Values) == len(y.Values) && (len(x.Values) == 0 || mustCanon(t, x.Values) == mustCanon(t, y.Values)) &&
			maps.Equal(x.Stamps, y.Stamps)
	}
	for range testkit.Cases() {
		a, b, c := arb(), arb(), arb()
		if !eqC(oam.JoinContent(a, b), oam.JoinContent(b, a)) {
			t.Fatal("join not commutative")
		}
		if !eqC(oam.JoinContent(oam.JoinContent(a, b), c), oam.JoinContent(a, oam.JoinContent(b, c))) {
			t.Fatal("join not associative")
		}
		if !eqC(oam.JoinContent(a, a), a) {
			t.Fatal("join not idempotent")
		}
	}
}
