package storetest

import (
	"fmt"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/oam"
)

func pick[T any](r core.Rng, xs ...T) T { return xs[core.IntN(r, len(xs))] }

// arbAsset draws from a small universe with several spellings per asset
// and conflicting optional fields, so merges collide often.
func arbAsset(r core.Rng) oam.Asset {
	host := pick(r, "api", "www", "mail")
	switch core.IntN(r, 6) {
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
	case 4:
		return oam.Organization{UniqueID: "acme", Name: pick(r, "", "Acme", "ACME Inc"), Headcount: pick(r, 0, 10, 20)}
	default:
		return oam.Opaque{Type: oam.TypeProduct, Content: map[string]any{"name": pick(r, "nginx", "apache")}}
	}
}

// RandomObservation draws observation number i of a random workload for
// (tenant, program): a few colliding assets, edges between them, tags
// (some SourceProperty) and observation times with frequent ties, so
// every merge rule and tie-break is exercised.
func RandomObservation(r core.Rng, i int, tenant, program string) oam.Observation {
	o := oam.Observation{
		ObservationID: fmt.Sprintf("obs-%d", i), TenantID: tenant, ProgramID: program,
		RunID:      RunID(pick(r, "run-1", "run-2", "run-3")),
		Tool:       oam.Tool{Name: pick(r, "subfinder", "dnsx", "httpx")},
		ObservedAt: T0.Add(time.Duration(core.IntN(r, 4)) * time.Minute),
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
		switch {
		case types[from] == oam.TypeFQDN && (types[to] == oam.TypeIPAddress || types[to] == oam.TypeFQDN):
			rel = oam.BasicDNSRelation{Label: "dns_record", Header: oam.DNSHeader{RRType: 1, Class: 1, TTL: pick(r, 60, 300)}}
		case types[from] == oam.TypeIPAddress && types[to] == oam.TypeService:
			rel = oam.PortRelation{Label: "port", PortNumber: pick(r, 80, 443), Protocol: "tcp"}
		case types[from] == oam.TypeOrganization:
			rel = oam.SimpleRelation{Label: "owns"}
		}
		o.Edges = append(o.Edges, oam.ObservedEdge{From: from, To: to, Relation: rel})
	}
	for range core.IntN(r, 3) {
		target := fmt.Sprint("r", core.IntN(r, n))
		var p oam.Property = oam.SimpleProperty{Name: pick(r, "title", "note"), Value: pick(r, "x", "y", "")}
		switch core.IntN(r, 4) {
		case 0:
			p = oam.SourceProperty{Name: pick(r, "crtsh", "dnsdumpster"), Confidence: pick(r, 40, 70)}
		case 1:
			p = oam.FlowforgeProperty{Name: "rank", Value: pick[any](r, "high", "low"), Kind: oam.KindHypothesis}
		}
		o.Tags = append(o.Tags, oam.ObservedTag{Target: target, Property: p})
	}
	return o
}

// CoveringRun is a random covering-run report for the RandomObservation
// workload.
type CoveringRun struct {
	Tool, Run string
	At        time.Time
}

// RandomCoveringRun draws a covering run.
func RandomCoveringRun(r core.Rng, i int) CoveringRun {
	run := pick(r, "run-1", "run-2", "run-3", fmt.Sprintf("cover-%d", i))
	return CoveringRun{Tool: pick(r, "subfinder", "dnsx", "httpx"), Run: RunID(run), At: T0.Add(time.Hour + time.Duration(i)*time.Minute)}
}

var (
	authPages   = []string{"https://acme.com/", "https://acme.com/shop"}
	authScripts = []string{"https://acme.com/a.js", "https://acme.com/b.js", "https://cdn.acme.com/c.js"}
	authLinks   = []string{"https://acme.com/about", "https://acme.com/help"}
	authForms   = []string{"https://acme.com/login", "https://acme.com/search"}
)

// RandomAuthObservation draws observation number i of a content-capture
// workload: complete, partial, failed and unsupported authoritative
// captures of two pages under two profiles with random covered rule
// families (scripts, links, frames, forms, headers), mixed with classic
// crawler observations of the same facts, header tags on the pages and
// the occasional non-source edge. Times tie often.
func RandomAuthObservation(r core.Rng, i int, tenant, program string) oam.Observation {
	o := oam.Observation{
		ObservationID: fmt.Sprintf("auth-%d", i), TenantID: tenant, ProgramID: program, RunID: RunID(pick(r, "run-1", "run-2")),
		ObservedAt: T0.Add(time.Duration(core.IntN(r, 6)) * time.Minute), Confidence: pick(r, 0.5, 0.9),
		Tool: oam.Tool{Name: "extract"},
	}
	o.Entities = append(o.Entities, ent("p", oam.URL{URL: pick(r, authPages...)}))
	n := 0
	add := func(a oam.Asset, label string) {
		ref := fmt.Sprint("t", n)
		n++
		o.Entities = append(o.Entities, ent(ref, a))
		o.Edges = append(o.Edges, oam.ObservedEdge{From: "p", To: ref, Relation: oam.SimpleRelation{Label: label}})
	}
	for _, u := range authScripts {
		if core.IntN(r, 2) == 0 {
			add(oam.File{URL: u}, oam.LabelLoadsScript)
		}
	}
	for _, u := range authLinks {
		if core.IntN(r, 2) == 0 {
			add(oam.URL{URL: u}, pick(r, oam.LabelLinksTo, oam.LabelEmbeds))
		}
	}
	for _, u := range authForms {
		if core.IntN(r, 3) == 0 {
			add(oam.Endpoint{Method: "POST", URLTemplate: u, Source: oam.EndpointSourceHTMLForm}, oam.LabelFormAction)
		}
	}
	for _, h := range []string{"server", "x-frame-options"} {
		if core.IntN(r, 2) == 0 {
			o.Tags = append(o.Tags, oam.ObservedTag{Target: "p", Property: oam.HTTPHeaderProperty{Name: h, Value: pick(r, "a", "b")}})
		}
	}
	if core.IntN(r, 4) == 0 && n > 1 {
		o.Edges = append(o.Edges, oam.ObservedEdge{From: "t0", To: "t1", Relation: oam.SimpleRelation{Label: "seen_with"}})
	}
	if core.IntN(r, 4) == 0 {
		o.Tool.Name = "crawl"
		return o
	}
	cov := pick(r, oam.CoverageComplete, oam.CoverageComplete, oam.CoverageComplete, oam.CoveragePartial, oam.CoverageFailed, oam.CoverageUnsupported)
	var rules []string
	for _, f := range []string{oam.FamilyScripts, oam.FamilyLinks, oam.FamilyFrames, oam.FamilyForms, oam.FamilyHeaders} {
		if core.IntN(r, 3) > 0 {
			rules = append(rules, f)
		}
	}
	o.Authority = &oam.Authority{Source: "p", Profile: pick(r, "default", "mobile"), Coverage: cov, CoveredRules: rules}
	o.Cause = pick(r, "", oam.CauseTarget, oam.CauseParser)
	o.Baseline = core.IntN(r, 5) == 0
	return o
}

// RandomMixedObservation draws from RandomObservation and
// RandomAuthObservation (ids kept distinct).
func RandomMixedObservation(r core.Rng, i int, tenant, program string) oam.Observation {
	if core.IntN(r, 2) == 0 {
		return RandomObservation(r, i, tenant, program)
	}
	return RandomAuthObservation(r, i, tenant, program)
}
