package oam_test

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/core/netnorm"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/testkit"
)

// tplSeg is one generated path segment: its canonical form and ways to
// spell it.
type tplSeg struct {
	canon     string
	spellings []string
}

func arbTemplateSeg(r core.Rng) tplSeg {
	word := pick(r, "users", "api", "v2", "Items", "a-b", "x.json", "~me")
	switch core.IntN(r, 7) {
	case 0:
		n := fmt.Sprint(core.IntN(r, 100000))
		enc := ""
		for _, c := range n {
			enc += fmt.Sprintf("%%%X", c)
		}
		return tplSeg{oam.PlaceholderID, []string{n, enc, "{id}"}}
	case 1:
		u := "550e8400-e29b-41d4-a716-44665544000" + fmt.Sprint(core.IntN(r, 10))
		return tplSeg{oam.PlaceholderUUID, []string{u, strings.ToUpper(u), "{uuid}"}}
	case 2:
		h := strings.Repeat("deadbeef", 2+core.IntN(r, 3))
		return tplSeg{oam.PlaceholderHex, []string{h, strings.ToUpper(h), "{hex}"}}
	case 3:
		return tplSeg{oam.PlaceholderExpr, []string{"${user.id}", "{userId}", "{expr}", "${a[" + fmt.Sprint(core.IntN(r, 9)) + "]}", "pre${x}post"}}
	default:
		enc := ""
		for i := 0; i < len(word); i++ {
			if c := word[i]; core.IntN(r, 2) == 0 && (c >= 'a' && c <= 'z' || c == '~' || c == '-' || c == '.') {
				enc += fmt.Sprintf("%%%02x", c)
			} else {
				enc += string(c)
			}
		}
		return tplSeg{word, []string{word, enc}}
	}
}

// arbTemplate returns a canonical template and several equivalent
// spellings of it.
func arbTemplate(r core.Rng) (string, []string) {
	hosts := []struct{ canon, spelt string }{
		{"https://api.acme.com", "HTTPS://API.Acme.COM"}, {"https://api.acme.com", "https://api.acme.com:443"},
		{"http://acme.com:8080", "http://ACME.com.:8080"}, {"https://xn--bcher-kva.de", "https://bücher.de"},
		{"http://[2001:db8::1]", "http://[2001:DB8:0::1]:80"},
	}
	h := hosts[core.IntN(r, len(hosts))]
	segs := make([]tplSeg, 1+core.IntN(r, 5))
	for i := range segs {
		segs[i] = arbTemplateSeg(r)
	}
	trailing := core.IntN(r, 4) == 0
	var canon strings.Builder
	canon.WriteString(h.canon)
	for _, s := range segs {
		canon.WriteString("/" + s.canon)
	}
	if trailing {
		canon.WriteString("/")
	}
	var spellings []string
	for range 4 {
		var b strings.Builder
		b.WriteString(pick(r, h.canon, h.spelt))
		for _, s := range segs {
			if core.IntN(r, 5) == 0 {
				b.WriteString(pick(r, "/.", "/zz/..", "/./q/.."))
			}
			b.WriteString("/" + pick(r, s.spellings...))
		}
		if trailing {
			b.WriteString("/")
		}
		b.WriteString(pick(r, "", "?q=1", "?a=${b}&c", "#frag", "?x#y"))
		spellings = append(spellings, pick(r, "", " ")+b.String()+pick(r, "", "\t"))
	}
	return canon.String(), spellings
}

// TestOAMInv01_URLTemplateIdempotent checks OAM-INV-01: normalization is
// a fixed point on every accepted input (conformance inputs and
// generated templates).
func TestOAMInv01_URLTemplateIdempotent(t *testing.T) {
	r := testkit.Rng(t)
	inputs := urlTemplateCases(t)
	for range testkit.Cases() {
		_, sp := arbTemplate(r)
		inputs = append(inputs, sp...)
	}
	for _, in := range inputs {
		n, err := oam.NormalizeURLTemplate(in)
		if err != nil {
			continue
		}
		if n2, err := oam.NormalizeURLTemplate(n); err != nil || n2 != n {
			t.Fatalf("%q -> %q -> %q (%v)", in, n, n2, err)
		}
	}
}

// TestOAMInv02_URLTemplateEquivalentSpellings checks OAM-INV-02:
// equivalent spellings (scheme and host case, default ports, trailing
// host dot, IDN U-labels, unreserved escapes, dot segments, query and
// fragment, placeholder spellings and id-like literals) normalize to the
// same template, which is the generator's canonical form.
func TestOAMInv02_URLTemplateEquivalentSpellings(t *testing.T) {
	r := testkit.Rng(t)
	for range testkit.Cases() {
		want, spellings := arbTemplate(r)
		for _, s := range spellings {
			got, err := oam.NormalizeURLTemplate(s)
			if err != nil || got != want {
				t.Fatalf("%q -> %q (%v), want %q", s, got, err, want)
			}
		}
	}
}

// TestOAMInv03_URLTemplateConcreteHost checks OAM-INV-03: every accepted
// template has a concrete http(s) origin equal to netnorm's for the same
// origin, no placeholder before the path, and on placeholder-free URLs
// without id-like segments the result is netnorm.ParseURL's without the
// query.
func TestOAMInv03_URLTemplateConcreteHost(t *testing.T) {
	r := testkit.Rng(t)
	inputs := urlTemplateCases(t)
	for range testkit.Cases() {
		_, sp := arbTemplate(r)
		inputs = append(inputs, sp...)
	}
	for _, in := range inputs {
		n, err := oam.NormalizeURLTemplate(in)
		if err != nil {
			continue
		}
		rest, ok := strings.CutPrefix(n, "http://")
		if !ok {
			rest, ok = strings.CutPrefix(n, "https://")
		}
		slash := strings.IndexByte(rest, '/')
		if !ok || slash <= 0 || strings.ContainsAny(rest[:slash], "{}") {
			t.Fatalf("%q -> %q: origin not concrete", in, n)
		}
		origin := n[:len(n)-len(rest)+slash]
		u, err := netnorm.ParseURL(origin)
		if err != nil || strings.TrimSuffix(u.String(), "/") != origin {
			t.Fatalf("%q: origin %q is not netnorm-canonical (%v)", in, origin, err)
		}
	}
	for _, s := range []string{"https://Acme.com/a/b%2Fc?x=1", "http://acme.com:80/x/./y/../z", "https://acme.com/%7Euser/a%20b",
		"https://acme.com/Katalog/", "https://acme.com", "https://acme.com/a;b=c/@d:e"} {
		u, err := netnorm.ParseURL(s)
		if err != nil {
			t.Fatal(err)
		}
		u.Query = ""
		n, err := oam.NormalizeURLTemplate(s)
		if err != nil || n != u.String() {
			t.Fatalf("%q: template %q, netnorm %q (%v)", s, n, u.String(), err)
		}
	}
}

// TestOAMInv04_EndpointKey checks OAM-INV-04: an Endpoint's key is
// "ep:" + NormalizeHTTPMethod(method) + " " +
// NormalizeURLTemplate(url_template), Normalize is idempotent, and the
// source never changes the key.
func TestOAMInv04_EndpointKey(t *testing.T) {
	r := testkit.Rng(t)
	for range testkit.Cases() {
		want, sp := arbTemplate(r)
		m := pick(r, "get", "GET", " Post ", "*", "patch")
		wm, _ := oam.NormalizeHTTPMethod(m)
		var keys []string
		for _, s := range sp {
			e := oam.Endpoint{Method: m, URLTemplate: s, Source: pick(r, "", oam.EndpointSourceJSCall, oam.EndpointSourceObserved)}
			k, err := e.Key()
			if err != nil {
				t.Fatalf("%+v: %v", e, err)
			}
			n, err := e.Normalize()
			if err != nil {
				t.Fatal(err)
			}
			n2, _ := n.Normalize()
			if k2, _ := n.Key(); k2 != k || mustCanon(t, n) != mustCanon(t, n2) {
				t.Fatalf("normalize not idempotent or changes the key: %s", mustCanon(t, n))
			}
			keys = append(keys, k)
		}
		if keys = slices.Compact(keys); len(keys) != 1 || keys[0] != "ep:"+wm+" "+want {
			t.Fatalf("keys %v, want ep:%s %s", keys, wm, want)
		}
	}
}

// FuzzURLTemplate checks that NormalizeURLTemplate never panics, that
// its output is a fixed point (OAM-INV-01), that the origin is concrete
// (OAM-INV-03) and that every templated segment is one of the four
// placeholders.
func FuzzURLTemplate(f *testing.F) {
	f.Add("https://api.acme.com/v1/users/${user.id}/posts/42?x=1#y")
	f.Add("HTTPS://bücher.de:443/a/%7Bid%7D/{name}/../550E8400-E29B-41D4-A716-446655440000")
	f.Add("http://[::1]:8080/${a?`${b}`:'}'}/deadbeefdeadbeef")
	f.Add("https://ffph1z.com/ffph0z/${x}")
	f.Add("https://${h}/x")
	f.Add("https://a.com/%zz/{")
	f.Fuzz(func(t *testing.T, s string) {
		n, err := oam.NormalizeURLTemplate(s)
		if err != nil {
			return
		}
		n2, err := oam.NormalizeURLTemplate(n)
		if err != nil || n2 != n {
			t.Fatalf("%q -> %q -> %q (%v)", s, n, n2, err)
		}
		rest := n[strings.Index(n, "://")+3:]
		slash := strings.IndexByte(rest, '/')
		if slash <= 0 || strings.ContainsAny(rest[:slash], "{}$") {
			t.Fatalf("%q -> %q: host not concrete", s, n)
		}
		for _, seg := range strings.Split(rest[slash+1:], "/") {
			if strings.ContainsAny(seg, "{}") && !slices.Contains([]string{oam.PlaceholderID, oam.PlaceholderUUID, oam.PlaceholderHex, oam.PlaceholderExpr}, seg) {
				t.Fatalf("%q -> %q: segment %q", s, n, seg)
			}
		}
	})
}

// --- authoritative histories -------------------------------------------

var (
	authPages   = []string{"https://acme.com/", "https://acme.com/shop"}
	authScripts = []string{"https://acme.com/a.js", "https://acme.com/b.js", "https://cdn.acme.com/c.js"}
	authLinks   = []string{"https://acme.com/about", "https://acme.com/help"}
	authForms   = []string{"https://acme.com/login", "https://acme.com/search"}
)

// arbAuthObservation draws observation i of a history mixing complete,
// partial, failed and unsupported authoritative captures of two pages
// (two profiles, random covered rule families, edges of four families,
// header tags on the page) with classic crawler observations that report
// some of the same edges and tags.
func arbAuthObservation(r core.Rng, i int) oam.Observation {
	o := oam.Observation{
		ObservationID: fmt.Sprintf("auth-%d", i), TenantID: "ten", ProgramID: "prg", RunID: pick(r, "run-1", "run-2"),
		ObservedAt: t0.Add(time.Duration(core.IntN(r, 6)) * time.Minute), Confidence: pick(r, 0.5, 0.9),
		Tool: oam.Tool{Name: "extract"},
	}
	page := pick(r, authPages...)
	o.Entities = append(o.Entities, ent("p", oam.URL{URL: page}))
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
	if core.IntN(r, 4) == 0 && n > 1 { // a non-source edge stays classic
		o.Edges = append(o.Edges, oam.ObservedEdge{From: "t0", To: "t1", Relation: oam.SimpleRelation{Label: "seen_with"}})
	}
	if core.IntN(r, 4) == 0 {
		o.Tool.Name = "crawl" // classic observation of the same facts
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

func arbAuthHistory(r core.Rng, n int) []oam.Observation {
	out := make([]oam.Observation, n)
	for i := range out {
		out[i] = arbAuthObservation(r, i)
	}
	return out
}

// TestOAMInv05_AuthoritativeConvergence checks OAM-INV-05: with
// authoritative observations of every coverage in the mix, any
// permutation and duplication of the multiset gives byte-identical
// state (spec 23.5).
func TestOAMInv05_AuthoritativeConvergence(t *testing.T) {
	r := testkit.Rng(t)
	for c := range testkit.Cases() / 2 {
		list := arbAuthHistory(r, core.IntN(r, 25))
		base := applyAll(t, oam.Config{}, list)
		shuffled := slices.Clone(list)
		for range core.IntN(r, 8) {
			if len(list) > 0 {
				shuffled = append(shuffled, list[core.IntN(r, len(list))])
			}
		}
		core.Shuffle(r, len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		if got := applyAll(t, oam.Config{}, shuffled); got != base {
			t.Fatalf("case %d: authoritative state depends on order or duplication\nbase %s\ngot  %s", c, base, got)
		}
	}
}

// refUnits computes, by brute force from the applied observations, the
// effective member set of every authority unit: the members of the
// latest complete reading plus those of partial readings newer than it.
// It also returns the edge and tag members with classic support.
func refUnits(list []oam.Observation) (held map[string]bool, classic map[string]bool) {
	type reading struct {
		stamp    oam.Stamp
		complete bool
		members  []string
	}
	units := map[string][]reading{}
	held, classic = map[string]bool{}, map[string]bool{}
	seen := map[string]bool{}
	for _, o := range list {
		if seen[o.ObservationID] {
			continue
		}
		seen[o.ObservationID] = true
		ids := map[string]string{}
		for _, e := range o.Entities {
			n, _ := e.Asset.Normalize()
			k, _ := n.Key()
			ids[e.Ref] = oam.EntityID(o.TenantID, o.ProgramID, n.AssetType(), k)
		}
		src := ""
		if o.Authority != nil {
			src = ids[o.Authority.Source]
		}
		fams := map[string][]string{}
		for _, e := range o.Edges {
			rt, l, d, _, _ := oam.SplitRelation(e.Relation)
			id := oam.EdgeKey{TenantID: o.TenantID, ProgramID: o.ProgramID, From: ids[e.From], To: ids[e.To], RelationType: rt, Label: l, Discriminator: d}.ID()
			if src != "" && ids[e.From] == src {
				fams[oam.FamilyOfLabel(l)] = append(fams[oam.FamilyOfLabel(l)], id)
			} else {
				classic[id] = true
			}
		}
		for _, tg := range o.Tags {
			m := ids[tg.Target] + "|" + tg.Property.PropertyType() + "/" + tg.Property.PropertyName()
			if src != "" && ids[tg.Target] == src {
				fams[oam.FamilyOfProperty(tg.Property.PropertyType())] = append(fams[oam.FamilyOfProperty(tg.Property.PropertyType())], m)
			} else {
				classic[m] = true
			}
		}
		if src == "" {
			continue
		}
		complete := o.Authority.Coverage == oam.CoverageComplete
		if complete {
			for _, f := range o.Authority.CoveredRules {
				if _, ok := fams[f]; !ok {
					fams[f] = nil
				}
			}
		}
		for f, ms := range fams {
			k := src + "|" + o.Authority.Profile + "|" + f
			units[k] = append(units[k], reading{oam.Stamp{At: o.ObservedAt, Source: o.Tool.Name, Observation: o.ObservationID},
				complete && slices.Contains(o.Authority.CoveredRules, f), ms})
		}
	}
	for _, rs := range units {
		latest := -1
		for i := range rs {
			if rs[i].complete && (latest < 0 || rs[i].stamp.Compare(rs[latest].stamp) > 0) {
				latest = i
			}
		}
		for i, rd := range rs {
			if i == latest || !rd.complete && (latest < 0 || rd.stamp.Compare(rs[latest].stamp) > 0) {
				for _, m := range rd.members {
					held[m] = true
				}
			}
		}
	}
	return held, classic
}

// TestOAMInv06_LatestAuthoritativeNeverStale checks OAM-INV-06 against a
// brute-force reference: an edge held by the effective member set of any
// authority unit is active (covering runs included); an edge held by none
// and never reported classically is stale; an entity that such a held
// edge points at is active; a header tag on a page is visible exactly
// when held or reported classically.
func TestOAMInv06_LatestAuthoritativeNeverStale(t *testing.T) {
	r := testkit.Rng(t)
	for c := range testkit.Cases() / 2 {
		g := oam.NewGraph(oam.Config{StaleAfterRuns: 1})
		list := arbAuthHistory(r, 1+core.IntN(r, 25))
		for i, o := range list {
			mustApply(t, g, o)
			if core.IntN(r, 4) == 0 {
				if _, err := g.CompleteRun(oam.CoveringRun{TenantID: "ten", ProgramID: "prg", RunID: fmt.Sprint("cover-", i), Tool: pick(r, "crawl", "extract"), CompletedAt: t0.Add(time.Hour)}); err != nil {
					t.Fatal(err)
				}
			}
		}
		held, classic := refUnits(list)
		targets := map[string]bool{}
		for _, e := range g.Edges() {
			switch {
			case held[e.ID]:
				if e.State != oam.StateActive {
					t.Fatalf("case %d: edge %s held by an authority unit is %s", c, e.ID, e.State)
				}
				targets[e.ToEntity] = true
			case !classic[e.ID]:
				if e.State != oam.StateStale {
					t.Fatalf("case %d: edge %s without support is %s", c, e.ID, e.State)
				}
			}
		}
		for _, e := range g.Entities() {
			if targets[e.ID] && e.State != oam.StateActive {
				t.Fatalf("case %d: entity %s pointed at by a held edge is %s", c, e.Key, e.State)
			}
			if e.AssetType != oam.TypeURL {
				continue
			}
			vis := map[string]bool{}
			for _, tg := range e.Tags {
				if tg.PropertyType == oam.PropertyHTTPHeader {
					vis[e.ID+"|"+tg.PropertyType+"/"+tg.Name] = true
				}
			}
			for _, h := range []string{"server", "x-frame-options"} {
				m := e.ID + "|" + oam.PropertyHTTPHeader + "/" + h
				if want := held[m] || classic[m]; vis[m] != want {
					t.Fatalf("case %d: tag %s visible=%v, want %v", c, m, vis[m], want)
				}
			}
		}
	}
}

// visibleTags returns the non-source tags of every entity and edge by
// "target|type/name".
func visibleTags(g *oam.Graph) map[string]map[string]any {
	out := map[string]map[string]any{}
	add := func(id string, tags []oam.Tag) {
		for _, tg := range tags {
			if tg.PropertyType != oam.PropertySource {
				out[id+"|"+tg.PropertyType+"/"+tg.Name] = tg.Content
			}
		}
	}
	for _, e := range g.Entities() {
		add(e.ID, e.Tags)
	}
	for _, e := range g.Edges() {
		add(e.ID, e.Tags)
	}
	return out
}

// TestOAMInv07_TagChangedExactlyOnce checks OAM-INV-07: each operation
// emits exactly one tag_changed per tag whose visible content changed
// (and none otherwise, so duplicates and identical re-reports emit
// none), and the event's patch takes the tag from its old visible state
// to its new one.
func TestOAMInv07_TagChangedExactlyOnce(t *testing.T) {
	r := testkit.Rng(t)
	for c := range testkit.Cases() / 2 {
		g := oam.NewGraph(oam.Config{})
		list := arbAuthHistory(r, 1+core.IntN(r, 20))
		for i := range list {
			if i > 0 && core.IntN(r, 4) == 0 {
				list[i] = list[core.IntN(r, i)] // duplicate
			}
			if i > 0 && core.IntN(r, 4) == 0 { // identical facts, new id, later time
				list[i] = list[i-1]
				list[i].ObservationID = fmt.Sprint("again-", i)
				list[i].ObservedAt = list[i-1].ObservedAt.Add(time.Second)
			}
		}
		for _, o := range list {
			before := visibleTags(g)
			res := mustApply(t, g, o)
			after := visibleTags(g)
			changed := map[string]bool{}
			for _, k := range slices.Concat(slices.Collect(maps.Keys(before)), slices.Collect(maps.Keys(after))) {
				b, inB := before[k]
				a, inA := after[k]
				if inB != inA || inB && mustCanon(t, a) != mustCanon(t, b) {
					changed[k] = true
				}
			}
			got := map[string]int{}
			for _, ev := range res.Changes {
				if ev.Type != oam.ChangeTagChanged {
					continue
				}
				k := ev.EntityID + ev.EdgeID + "|" + ev.TagType + "/" + ev.TagName
				got[k]++
				if replayed := applyTagPatch(t, before[k], ev.Patch); mustCanon(t, replayed) != mustCanon(t, after[k]) {
					t.Fatalf("case %d: patch %s turns %v into %v, want %v", c, mustCanon(t, ev.Patch), before[k], replayed, after[k])
				}
			}
			for k := range changed {
				if got[k] != 1 {
					t.Fatalf("case %d: %s changed, %d tag_changed events", c, k, got[k])
				}
			}
			for k, n := range got {
				if !changed[k] || n != 1 {
					t.Fatalf("case %d: %d tag_changed events for %s (changed=%v)", c, n, k, changed[k])
				}
			}
		}
	}
}

// applyTagPatch applies a tag_changed patch (nil content = invisible).
func applyTagPatch(t *testing.T, content map[string]any, ops []oam.PatchOp) map[string]any {
	t.Helper()
	cur := maps.Clone(content)
	for _, op := range ops {
		switch {
		case op.Path == "" && op.Op == "add":
			cur = maps.Clone(op.Value.(map[string]any))
		case op.Path == "" && op.Op == "remove":
			cur = nil
		case op.Op == "add" || op.Op == "replace":
			cur[strings.TrimPrefix(op.Path, "/")] = op.Value
		case op.Op == "remove":
			delete(cur, strings.TrimPrefix(op.Path, "/"))
		default:
			t.Fatalf("bad op %+v", op)
		}
	}
	return cur
}

// replayed is the state change events determine.
type replayed struct {
	Content map[string]any            `json:"content,omitempty"`
	Scope   string                    `json:"scope,omitempty"`
	State   string                    `json:"state"`
	Tags    map[string]map[string]any `json:"tags,omitempty"`
}

// replay folds change events into per-record state, independently of
// graphstore's AsOf code.
func replay(t *testing.T, evs []oam.ChangeEvent) map[string]*replayed {
	out := map[string]*replayed{}
	get := func(id string) *replayed {
		if out[id] == nil {
			out[id] = &replayed{Tags: map[string]map[string]any{}}
		}
		return out[id]
	}
	for _, ev := range evs {
		id := ev.EntityID + ev.EdgeID
		switch ev.Type {
		case oam.ChangeEntityAdded:
			x := get(id)
			x.Content, x.Scope, x.State = maps.Clone(ev.Base["content"].(map[string]any)), ev.Base["scope_status"].(string), string(oam.StateActive)
		case oam.ChangeEntityChanged:
			for _, op := range ev.Patch {
				get(id).Content[strings.TrimPrefix(op.Path, "/")] = op.Value
			}
		case oam.ChangeScopeStatusChanged:
			get(id).Scope = ev.Patch[0].Value.(string)
		case oam.ChangeEntityStale, oam.ChangeEdgeRemoved:
			get(id).State = string(oam.StateStale)
		case oam.ChangeEntityReactivated:
			get(id).State = string(oam.StateActive)
		case oam.ChangeEdgeAdded:
			x := get(id)
			x.State = string(oam.StateActive)
			if ev.Base != nil {
				x.Content = maps.Clone(ev.Base["content"].(map[string]any))
			}
		case oam.ChangeTagChanged:
			x := get(id)
			k := ev.TagType + "/" + ev.TagName
			if v := applyTagPatch(t, x.Tags[k], ev.Patch); v == nil {
				delete(x.Tags, k)
			} else {
				x.Tags[k] = v
			}
		}
	}
	return out
}

// TestOAMInv08_ChangeReplay checks OAM-INV-08: after every operation
// (observations, covering runs, content changes), folding the change log
// (base + patches) reproduces every entity's content, scope status,
// state and visible non-source tags, and every edge's state and tags.
func TestOAMInv08_ChangeReplay(t *testing.T) {
	r := testkit.Rng(t)
	scope := func(_, _ string, a oam.Asset) oam.ScopeStatus {
		if u, ok := a.(oam.URL); ok && strings.Contains(u.URL, "shop") {
			return oam.ScopeInScope
		}
		return oam.ScopeUnknown
	}
	for c := range testkit.Cases() / 4 {
		g := oam.NewGraph(oam.Config{Scope: scope, StaleAfterRuns: 1})
		for i := range 1 + core.IntN(r, 25) {
			switch core.IntN(r, 6) {
			case 0:
				if _, err := g.CompleteRun(oam.CoveringRun{TenantID: "ten", ProgramID: "prg", RunID: fmt.Sprint("c", i), Tool: pick(r, "crawl", "extract", "subfinder"), CompletedAt: t0}); err != nil {
					t.Fatal(err)
				}
			case 1:
				if ents := g.Entities(); len(ents) > 0 {
					e := ents[core.IntN(r, len(ents))]
					if _, _, err := g.RecordContentChange(oam.ContentChange{TenantID: "ten", ProgramID: "prg", EntityID: e.ID, At: t0,
						Patch: oam.ContentChangePatch{SnapshotFrom: fmt.Sprint("s", i), SnapshotTo: fmt.Sprint("s", i+1), Levels: []string{oam.LevelText}}}); err != nil {
						t.Fatal(err)
					}
				}
			case 2:
				o := arbObservation(r, i)
				o.ObservationID = fmt.Sprint("classic-", i)
				mustApply(t, g, o)
			default:
				mustApply(t, g, arbAuthObservation(r, i))
			}
			got := replay(t, g.Changes())
			want := map[string]*replayed{}
			for _, e := range g.Entities() {
				x := &replayed{Content: e.Content, Scope: string(e.ScopeStatus), State: string(e.State), Tags: map[string]map[string]any{}}
				for _, tg := range e.Tags {
					if tg.PropertyType != oam.PropertySource {
						x.Tags[tg.PropertyType+"/"+tg.Name] = tg.Content
					}
				}
				want[e.ID] = x
			}
			for _, e := range g.Edges() {
				x := &replayed{Content: got[e.ID].Content, State: string(e.State), Tags: map[string]map[string]any{}}
				for _, tg := range e.Tags {
					if tg.PropertyType != oam.PropertySource {
						x.Tags[tg.PropertyType+"/"+tg.Name] = tg.Content
					}
				}
				want[e.ID] = x
			}
			if a, b := mustCanon(t, got), mustCanon(t, want); a != b {
				t.Fatalf("case %d op %d: replay differs\nreplay %s\nstate  %s", c, i, a, b)
			}
		}
	}
}

// TestOAMInv09_ContentChangeIdempotent checks OAM-INV-09:
// RecordContentChange changes no graph state, is idempotent per
// (entity, snapshot_from, snapshot_to, diff_id), stores the normalized
// patch, copies cause and baseline onto the event, and rejects unknown
// entities and invalid patches.
func TestOAMInv09_ContentChangeIdempotent(t *testing.T) {
	r := testkit.Rng(t)
	for range testkit.Cases() / 10 {
		g := oam.NewGraph(oam.Config{})
		for i := range 5 {
			mustApply(t, g, arbAuthObservation(r, i))
		}
		before := canon(t, g)
		ents := g.Entities()
		seen := map[string]oam.ChangeEvent{}
		for i := range 20 {
			e := ents[core.IntN(r, len(ents))]
			p := oam.ContentChangePatch{SnapshotFrom: fmt.Sprint("a", core.IntN(r, 3)), SnapshotTo: fmt.Sprint("b", core.IntN(r, 3)),
				Levels: []string{pick(r, oam.LevelSemantic, oam.LevelHash), oam.LevelText}, Cause: pick(r, "", oam.CauseParser),
				Baseline: core.IntN(r, 2) == 0, Score: float64(core.IntN(r, 11)) / 10}
			ev, dup, err := g.RecordContentChange(oam.ContentChange{TenantID: "ten", ProgramID: "prg", EntityID: e.ID, At: t0.Add(time.Duration(i) * time.Second), Patch: p})
			if err != nil {
				t.Fatal(err)
			}
			k := e.ID + p.SnapshotFrom + p.SnapshotTo
			if first, ok := seen[k]; ok != dup || ok && mustCanon(t, first) != mustCanon(t, ev) {
				t.Fatalf("duplicate=%v for a %s change", dup, map[bool]string{true: "repeated", false: "new"}[ok])
			}
			if !dup {
				seen[k] = ev
				n, _ := p.Normalize()
				if mustCanon(t, *ev.ContentChange) != mustCanon(t, n) || ev.Baseline != p.Baseline || (ev.Cause == oam.CauseParser) != (p.Cause == oam.CauseParser) {
					t.Fatalf("event %s does not carry the patch %s", mustCanon(t, ev), mustCanon(t, n))
				}
			}
		}
		if canon(t, g) != before {
			t.Fatal("content changes altered graph state")
		}
		if _, _, err := g.RecordContentChange(oam.ContentChange{TenantID: "ten", ProgramID: "other", EntityID: ents[0].ID,
			Patch: oam.ContentChangePatch{SnapshotTo: "x", Levels: []string{oam.LevelHash}}}); err == nil {
			t.Fatal("content change on another program's entity accepted")
		}
		if _, _, err := g.RecordContentChange(oam.ContentChange{TenantID: "ten", ProgramID: "prg", EntityID: ents[0].ID,
			Patch: oam.ContentChangePatch{SnapshotTo: "x", Levels: []string{oam.LevelHash}, Score: 2}}); err == nil {
			t.Fatal("score 2 accepted")
		}
	}
}

// TestOAMInv10_CauseAndBaselineOnEvents checks OAM-INV-10: every change
// event of an observation carries its cause (target as "") and baseline
// flag, and nothing else does.
func TestOAMInv10_CauseAndBaselineOnEvents(t *testing.T) {
	r := testkit.Rng(t)
	for range testkit.Cases() / 10 {
		g := oam.NewGraph(oam.Config{})
		for i := range 10 {
			o := arbAuthObservation(r, i)
			res := mustApply(t, g, o)
			cause := o.Cause
			if cause == oam.CauseTarget {
				cause = ""
			}
			for _, ev := range res.Changes {
				if ev.Cause != cause || ev.Baseline != o.Baseline {
					t.Fatalf("event %s of observation cause=%q baseline=%v", mustCanon(t, ev), o.Cause, o.Baseline)
				}
			}
		}
		sr, err := g.CompleteRun(oam.CoveringRun{TenantID: "ten", ProgramID: "prg", RunID: "c", Tool: "crawl", StaleAfterRuns: 1, CompletedAt: t0})
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range sr.Changes {
			if ev.Cause != "" || ev.Baseline {
				t.Fatalf("covering-run event carries cause/baseline: %s", mustCanon(t, ev))
			}
		}
	}
}

// TestOAMInv11_NoSecretValues checks OAM-INV-11: an exposed_secret tag
// built by NewExposedSecret never contains the secret, always validates,
// and LooksLikeSecret flags random credentials while letting digests,
// UUIDs, previews and keyed hashes through.
func TestOAMInv11_NoSecretValues(t *testing.T) {
	r := testkit.Rng(t)
	const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	key := []byte("tenant-key")
	for range testkit.Cases() {
		var b strings.Builder
		b.WriteString(pick(r, "", "AKIA", "ghp_", "sk_live_", "xoxb-"))
		for range 24 + core.IntN(r, 24) {
			b.WriteByte(alnum[core.IntN(r, len(alnum))])
		}
		b.WriteString(fmt.Sprint(core.IntN(r, 10)))
		secret := b.String()
		if !oam.LooksLikeSecret("token=" + secret + ";") {
			t.Fatalf("%q not flagged", secret)
		}
		p, err := oam.NewExposedSecret(pick(r, "aws-access-key", "github-pat", "generic"), secret, key, "ff-secrets@1", "finding:1")
		if err != nil {
			t.Fatalf("%q: %v", secret, err)
		}
		if strings.Contains(mustCanon(t, p), secret) || strings.Contains(mustCanon(t, p), secret[4:len(secret)-2]) {
			t.Fatalf("tag %s leaks the secret", mustCanon(t, p))
		}
		leak := p
		leak.Description = secret
		if leak.Validate() == nil {
			t.Fatal("full secret accepted as preview")
		}
		leak = p
		leak.Ref = "finding:" + secret
		if leak.Validate() == nil {
			t.Fatal("secret accepted in ref")
		}
		digest := core.SHA256Hex([]byte(secret))
		for _, ok := range []string{digest, strings.ToUpper(digest), "550e8400-e29b-41d4-a716-446655440000", oam.SecretPreview(secret),
			oam.SecretKeyedHash(key, secret), "default-src 'self' https://cdn.acme.com"} {
			if oam.LooksLikeSecret(ok) {
				t.Fatalf("%q flagged as a secret", ok)
			}
		}
	}
}
