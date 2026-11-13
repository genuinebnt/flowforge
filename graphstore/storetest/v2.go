package storetest

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/oam"
)

// historyOp is one step of a random v2 history.
type historyOp struct {
	obs     *oam.Observation
	cover   *CoveringRun
	content int // >0: record a content change on a random entity (seeded by the value)
}

// randomHistory draws n operations: mixed classic and authoritative
// observations (with re-deliveries), covering runs and content changes.
func randomHistory(r core.Rng, n int, prefix string) []historyOp {
	var out []historyOp
	var obs []oam.Observation
	for i := range n {
		switch k := core.IntN(r, 10); {
		case k == 0:
			cr := RandomCoveringRun(r, i)
			out = append(out, historyOp{cover: &cr})
		case k == 1:
			out = append(out, historyOp{content: 1 + core.IntN(r, 1000)})
		case k == 2 && len(obs) > 0:
			o := obs[core.IntN(r, len(obs))]
			out = append(out, historyOp{obs: &o})
		default:
			o := RandomMixedObservation(r, i, "", "")
			o.ObservationID = prefix + o.ObservationID
			obs = append(obs, o)
			out = append(out, historyOp{obs: &o})
		}
	}
	return out
}

// applyOp runs one history step against e.
func (e *env) applyOp(op historyOp) {
	e.t.Helper()
	switch {
	case op.obs != nil:
		if _, err := e.s.IngestObservation(e.ctx, graphstore.IngestRequest{TenantID: e.tenant, ProgramID: e.prog, Observation: *op.obs}); err != nil {
			e.t.Fatalf("ingest %s: %v", op.obs.ObservationID, err)
		}
	case op.cover != nil:
		e.clock.Set(op.cover.At)
		if _, err := e.s.SetStale(e.ctx, e.c, graphstore.SetStaleRequest{ProgramID: e.prog, RunID: op.cover.Run, ToolName: op.cover.Tool, StaleAfterRuns: 1}); err != nil {
			e.t.Fatalf("set stale: %v", err)
		}
	case op.content > 0:
		ex := e.export()
		if len(ex.Entities) == 0 {
			return
		}
		ent := ex.Entities[op.content%len(ex.Entities)]
		p := oam.ContentChangePatch{SnapshotFrom: fmt.Sprint("s", op.content), SnapshotTo: fmt.Sprint("s", op.content+1),
			Levels: []string{oam.LevelText}, Cause: []string{"", oam.CauseParser}[op.content%2], Score: 0.25,
			ScoreFeatures: []oam.ScoreFeature{{Name: "new_endpoint", Weight: 0.25}}}
		if _, err := e.s.RecordContentChange(e.ctx, e.c, graphstore.ContentChange{EntityID: ent.ID, At: T0.Add(2 * time.Hour), Patch: p}); err != nil {
			e.t.Fatalf("content change: %v", err)
		}
	}
}

// eventView is a change event without its store-specific seq and entity
// snapshot, for comparing two stores.
type eventView struct {
	ID            string                  `json:"id"`
	Type          oam.ChangeType          `json:"type"`
	EntityID      string                  `json:"entity_id,omitempty"`
	EdgeID        string                  `json:"edge_id,omitempty"`
	RunID         string                  `json:"run_id,omitempty"`
	Patch         []oam.PatchOp           `json:"patch,omitempty"`
	At            string                  `json:"at"`
	TagType       string                  `json:"tag_type,omitempty"`
	TagName       string                  `json:"tag_name,omitempty"`
	Base          map[string]any          `json:"base,omitempty"`
	ContentChange *oam.ContentChangePatch `json:"content_change,omitempty"`
	Cause         string                  `json:"cause,omitempty"`
	Baseline      bool                    `json:"baseline,omitempty"`
}

func eventViews(evs []graphstore.ChangeEvent) []eventView {
	out := make([]eventView, 0, len(evs))
	for _, ev := range evs {
		out = append(out, eventView{ID: ev.ID, Type: ev.Type, EntityID: ev.EntityID, EdgeID: ev.EdgeID, RunID: ev.RunID, Patch: ev.Patch,
			At: ev.At.UTC().Format(time.RFC3339Nano), TagType: ev.TagType, TagName: ev.TagName, Base: ev.Base,
			ContentChange: ev.ContentChange, Cause: ev.Cause, Baseline: ev.Baseline})
	}
	return out
}

// AuthConvergence checks OAM-INV-05 on a store: random multisets of
// classic and authoritative observations (every coverage) applied in a
// random order with duplicates give the same graph as the original
// order, and (with an oracle) the oracle's graph byte for byte.
func AuthConvergence(t *testing.T, f Factory, r core.Rng, cases int, oracle Factory) {
	a, b := newEnv(t, f, Options{}), newEnv(t, f, Options{})
	var o *env
	if oracle != nil {
		o = newEnv(t, oracle, Options{})
	}
	for c := range cases {
		prog := ProgramID(fmt.Sprint("auth-conv-", c))
		list := make([]oam.Observation, core.IntN(r, 20))
		for i := range list {
			list[i] = RandomMixedObservation(r, i, "", "")
			list[i].ObservationID = fmt.Sprintf("ac%d-%s", c, list[i].ObservationID)
		}
		shuffled := slices.Clone(list)
		for range core.IntN(r, 8) {
			if len(list) > 0 {
				shuffled = append(shuffled, list[core.IntN(r, len(list))])
			}
		}
		core.Shuffle(r, len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		apply := func(e *env, obs []oam.Observation) string {
			e.prog = prog
			for _, x := range obs {
				if _, err := e.s.IngestObservation(e.ctx, graphstore.IngestRequest{TenantID: e.tenant, ProgramID: prog, Observation: x}); err != nil {
					t.Fatalf("case %d: ingest %s: %v", c, x.ObservationID, err)
				}
			}
			return Canon(t, e.export())
		}
		want := apply(a, list)
		if got := apply(b, shuffled); got != want {
			t.Fatalf("case %d: authoritative state depends on order or duplication\nwant %s\n got %s", c, want, got)
		}
		if o != nil {
			if got := apply(o, list); got != want {
				t.Fatalf("case %d: state differs from the oracle\n got %s\nwant %s", c, want, got)
			}
		}
	}
}

// SameHistory checks OAM-INV-13: on random v2 histories (classic and
// authoritative observations, re-deliveries, covering runs, content
// changes) the store and the oracle end with byte-identical canonical
// state and identical change logs (ids, types, patches, bases, tags,
// causes; seqs are store-local and excluded).
func SameHistory(t *testing.T, f Factory, r core.Rng, cases int, oracle Factory) {
	for c := range cases {
		ops := randomHistory(r, 1+core.IntN(r, 30), fmt.Sprintf("sh%d-", c))
		a, b := newEnv(t, f, Options{}), newEnv(t, oracle, Options{})
		for _, op := range ops {
			a.applyOp(op)
			b.applyOp(op)
		}
		if x, y := Canon(t, a.export()), Canon(t, b.export()); x != y {
			t.Fatalf("case %d: state differs\nstore  %s\noracle %s", c, x, y)
		}
		if x, y := Canon(t, eventViews(a.changes(0))), Canon(t, eventViews(b.changes(0))); x != y {
			t.Fatalf("case %d: change logs differ\nstore  %s\noracle %s", c, x, y)
		}
	}
}

// asOfView is the part of an entity AsOf reconstructs.
type asOfView struct {
	Content map[string]any    `json:"content"`
	Scope   oam.ScopeStatus   `json:"scope"`
	State   oam.State         `json:"state"`
	Tags    []graphstore.Tag  `json:"tags"`
	Edges   map[string]string `json:"edges,omitempty"`
}

func entityView(e graphstore.Entity) asOfView {
	v := asOfView{Content: e.Content, Scope: e.ScopeStatus, State: e.State, Tags: []graphstore.Tag{}}
	for _, tg := range e.Tags {
		if tg.PropertyType != oam.PropertySource {
			tg.CreatedAt, tg.LastSeen = time.Time{}, time.Time{}
			v.Tags = append(v.Tags, tg)
		}
	}
	return v
}

// AsOf checks OAM-INV-12: after a random history, GetEntityAsOf(seq) and
// NeighborsAsOf(seq) at the seq reached after every operation equal the
// reference snapshot taken right after that operation (content, scope
// status, state and non-source tags of every entity; the neighbourhood's
// entities, edges and their states). Entities created later are
// NOT_FOUND; bad seqs and since are INVALID_ARGUMENT.
func AsOf(t *testing.T, f Factory, r core.Rng, cases int) {
	for c := range cases {
		e := newEnv(t, f, Options{Scope: func(_, _ string, a oam.Asset) oam.ScopeStatus {
			if u, ok := a.(oam.URL); ok && u.URL == "https://acme.com/shop" {
				return oam.ScopeInScope
			}
			return oam.ScopeUnknown
		}})
		e.prog = ProgramID(fmt.Sprint("asof-", c))
		type snap struct {
			seq   int64
			ents  map[string]asOfView
			neigh map[string]string // entity id -> canonical neighbourhood
		}
		var snaps []snap
		var last int64
		for _, op := range randomHistory(r, 1+core.IntN(r, 25), fmt.Sprintf("ao%d-", c)) {
			e.applyOp(op)
			evs := e.changes(last)
			if len(evs) == 0 {
				continue
			}
			last = evs[len(evs)-1].Seq
			s := snap{seq: last, ents: map[string]asOfView{}, neigh: map[string]string{}}
			for _, en := range e.export().Entities {
				s.ents[en.ID] = entityView(en)
				sg, err := e.s.Neighbors(e.ctx, e.c, graphstore.NeighborsRequest{EntityID: en.ID, Direction: graphstore.DirectionBoth, Depth: 2})
				if err != nil {
					t.Fatal(err)
				}
				s.neigh[en.ID] = neighbourhood(t, sg)
			}
			snaps = append(snaps, s)
		}
		final := e.export().Entities
		for _, s := range snaps {
			for _, en := range final {
				got, err := e.s.GetEntityAsOf(e.ctx, e.c, graphstore.GetEntityAsOfRequest{EntityID: en.ID, Seq: s.seq, IncludeTags: true})
				want, existed := s.ents[en.ID]
				if !existed {
					if !isNotFound(err) {
						t.Fatalf("case %d seq %d: entity %s did not exist yet, got %v", c, s.seq, en.Key, err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("case %d seq %d: %v", c, s.seq, err)
				}
				if a, b := Canon(t, entityView(got)), Canon(t, want); a != b {
					t.Fatalf("case %d seq %d entity %s:\nas of %s\nwant  %s", c, s.seq, en.Key, a, b)
				}
				sg, err := e.s.NeighborsAsOf(e.ctx, e.c, graphstore.NeighborsAsOfRequest{Seq: s.seq,
					NeighborsRequest: graphstore.NeighborsRequest{EntityID: en.ID, Direction: graphstore.DirectionBoth, Depth: 2}})
				if err != nil {
					t.Fatal(err)
				}
				if a, b := neighbourhood(t, sg), s.neigh[en.ID]; a != b {
					t.Fatalf("case %d seq %d neighbours of %s:\nas of %s\nwant  %s", c, s.seq, en.Key, a, b)
				}
			}
		}
		if len(final) > 0 {
			_, err := e.s.GetEntityAsOf(e.ctx, e.c, graphstore.GetEntityAsOfRequest{EntityID: final[0].ID})
			wantKind(t, "seq 0", err, core.KindInvalidArgument)
			_, err = e.s.NeighborsAsOf(e.ctx, e.c, graphstore.NeighborsAsOfRequest{Seq: 1,
				NeighborsRequest: graphstore.NeighborsRequest{EntityID: final[0].ID, Direction: graphstore.DirectionOut, Since: T0}})
			wantKind(t, "since", err, core.KindInvalidArgument)
			_, err = e.s.GetEntityAsOf(e.ctx, graphstore.Caller{TenantID: TenantID("other")}, graphstore.GetEntityAsOfRequest{EntityID: final[0].ID, Seq: last})
			wantKind(t, "other tenant", err, core.KindNotFound)
		}
	}
}

func neighbourhood(t testing.TB, sg graphstore.Subgraph) string {
	type v struct {
		Entities []string `json:"entities"`
		Edges    []string `json:"edges"`
	}
	var out v
	for _, e := range sg.Entities {
		out.Entities = append(out.Entities, e.ID+":"+string(e.State)+":"+string(e.ScopeStatus)+":"+Canon(t, e.Content))
	}
	for _, e := range sg.Edges {
		out.Edges = append(out.Edges, e.ID+":"+string(e.State))
	}
	return Canon(t, out)
}

// ContentChanges checks RecordContentChange (V2 §3.4): the event is
// readable, filterable by type and carries the normalized patch with
// cause and baseline; a repeat returns the same event and appends
// nothing; graph state is unchanged; invalid patches are
// INVALID_ARGUMENT and other tenants' entities NOT_FOUND.
func ContentChanges(t *testing.T, f Factory) {
	e, fx := newFixtureEnv(t, f)
	before, n := Canon(t, e.export()), len(e.changes(0))
	req := graphstore.ContentChange{EntityID: fx.ids["api"], At: T0.Add(3 * time.Hour), Patch: oam.ContentChangePatch{
		SnapshotFrom: "snp_1", SnapshotTo: "snp_2", DiffID: "dif_1", Levels: []string{oam.LevelSemantic, oam.LevelHash},
		Summary: map[string]int{"endpoints_added": 2}, Cause: oam.CauseParser, Baseline: true, Class: oam.ClassSemanticEndpoint,
		Score: 0.5, ScoreFeatures: []oam.ScoreFeature{{Name: "z", Weight: -0.25}, {Name: "a", Weight: 0.75}}}}
	ev, err := e.s.RecordContentChange(e.ctx, e.c, req)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := req.Patch.Normalize()
	if ev.Type != oam.ChangeContentChanged || ev.EntityID != fx.ids["api"] || ev.Seq == 0 || ev.Entity == nil ||
		ev.Cause != oam.CauseParser || !ev.Baseline || ev.ContentChange == nil || Canon(t, *ev.ContentChange) != Canon(t, want) {
		t.Fatalf("event %s", Canon(t, ev))
	}
	again, err := e.s.RecordContentChange(e.ctx, e.c, req)
	if err != nil || Canon(t, eventViews([]graphstore.ChangeEvent{again})) != Canon(t, eventViews([]graphstore.ChangeEvent{ev})) || again.Seq != ev.Seq {
		t.Fatalf("repeat: %v %s", err, Canon(t, again))
	}
	evs := e.changes(0)
	if len(evs) != n+1 || Canon(t, e.export()) != before {
		t.Fatalf("content change: %d events (want %d) or graph changed", len(evs), n+1)
	}
	got, err := e.s.ReadChanges(e.ctx, e.c, graphstore.ChangesRequest{ProgramID: e.prog, Types: []oam.ChangeType{oam.ChangeContentChanged}})
	if err != nil || len(got) != 1 || Canon(t, eventViews(got)) != Canon(t, eventViews([]graphstore.ChangeEvent{ev})) {
		t.Fatalf("read by type: %v %s", err, Canon(t, got))
	}
	for _, bad := range []oam.ContentChangePatch{{SnapshotTo: "x"}, {SnapshotTo: "x", Levels: []string{"hash"}, Score: 1.5},
		{SnapshotFrom: "x", SnapshotTo: "x", Levels: []string{"hash"}}, {SnapshotTo: "x", Levels: []string{"hash"}, Class: "cosmetic"}} {
		_, err := e.s.RecordContentChange(e.ctx, e.c, graphstore.ContentChange{EntityID: fx.ids["api"], Patch: bad})
		wantKind(t, fmt.Sprintf("bad patch %+v", bad), err, core.KindInvalidArgument)
	}
	_, err = e.s.RecordContentChange(e.ctx, graphstore.Caller{TenantID: TenantID("other")}, graphstore.ContentChange{EntityID: fx.ids["api"], Patch: req.Patch})
	wantKind(t, "other tenant", err, core.KindNotFound)
}

// Commonness checks OAM-INV-14: an entity's holders are the distinct
// hosts of the active entities pointing at it over active edges; it is
// common exactly when there are more than CommonThreshold; stale edges
// and repeated hosts do not count, and the count is monotone in the
// holders added.
func Commonness(t *testing.T, f Factory) {
	e := newEnv(t, f, Options{})
	script := oam.File{URL: "https://cdn.shared.net/lib.js"}
	sid := ""
	check := func(sources, holders int) {
		t.Helper()
		c, err := e.s.Commonness(e.ctx, e.c, graphstore.CommonnessRequest{EntityID: sid})
		if err != nil {
			t.Fatal(err)
		}
		if c.Sources != sources || c.Holders != holders || c.Common != (holders > graphstore.CommonThreshold) {
			t.Fatalf("commonness %+v, want %d sources, %d holders", c, sources, holders)
		}
	}
	page := func(i, host int, auth bool) oam.Observation {
		o := obs(fmt.Sprintf("cm-%d", i), "extract", T0.Add(time.Duration(i)*time.Second), 0.9, []oam.ObservedEntity{
			ent("p", oam.URL{URL: fmt.Sprintf("https://site%d.example/p%d", host, i)}), ent("s", script)},
			simple("p", "s", oam.LabelLoadsScript))
		if auth {
			o.Authority = &oam.Authority{Source: "p", Profile: "default", Coverage: oam.CoverageComplete, CoveredRules: []string{oam.FamilyScripts}}
		}
		return o
	}
	for i := range 60 {
		res := e.ingest(page(i, i/2, true)) // two pages per host: 30 holders
		sid = res.RefToEntityID["s"]
	}
	check(60, 30)
	for i := 60; i < 81; i++ {
		e.ingest(page(i, i, true)) // 21 more hosts: 51 holders
	}
	check(81, 51)
	// Page 80 stops loading the script: its complete capture withdraws the edge.
	o := page(200, 80, true)
	o.Entities, o.Edges = o.Entities[:1], nil
	o.Entities[0] = ent("p", oam.URL{URL: "https://site80.example/p80"})
	e.ingest(o)
	check(80, 50)
}

// BenchAuthoritative measures authoritative ingest: `pages` pages with
// `edges` extracted edges each (scripts, links and form endpoints),
// re-captured round robin with small changes (about 2 % of the edges replaced per capture), every capture
// complete for all three families, so each withdraws what it no longer
// reports. One iteration is one re-capture; it reports captures/s and
// edges/s.
func BenchAuthoritative(b *testing.B, s graphstore.Store, pages, edges int) {
	tenant, prog := TenantID("bench-auth"), ProgramID("bench-auth")
	r := core.NewSplitMix64(7)
	type pg struct{ links []int }
	state := make([]pg, pages)
	for p := range state {
		for j := range edges {
			state[p].links = append(state[p].links, j)
		}
	}
	next := edges
	capture := func(i int) oam.Observation {
		p := i % pages
		st := &state[p]
		for range max(edges/50, 1) {
			st.links[core.IntN(r, len(st.links))] = next
			next++
		}
		o := oam.Observation{ObservationID: fmt.Sprintf("bench-auth-%d", i), Tool: oam.Tool{Name: "extract"},
			ObservedAt: T0.Add(time.Duration(i) * time.Millisecond), Confidence: 0.9,
			Authority: &oam.Authority{Source: "p", Profile: "default", Coverage: oam.CoverageComplete,
				CoveredRules: []string{oam.FamilyScripts, oam.FamilyLinks, oam.FamilyForms}}}
		o.Entities = append(o.Entities, ent("p", oam.URL{URL: fmt.Sprintf("https://acme.com/page%d", p)}))
		for k, l := range st.links {
			ref := fmt.Sprint("t", k)
			switch l % 3 {
			case 0:
				o.Entities = append(o.Entities, ent(ref, oam.File{URL: fmt.Sprintf("https://acme.com/js/%d.js", l)}))
				o.Edges = append(o.Edges, simple("p", ref, oam.LabelLoadsScript))
			case 1:
				o.Entities = append(o.Entities, ent(ref, oam.URL{URL: fmt.Sprintf("https://acme.com/doc/%d", l)}))
				o.Edges = append(o.Edges, simple("p", ref, oam.LabelLinksTo))
			default:
				o.Entities = append(o.Entities, ent(ref, oam.Endpoint{Method: "POST", URLTemplate: fmt.Sprintf("https://acme.com/api/%d/x", l)}))
				o.Edges = append(o.Edges, simple("p", ref, oam.LabelFormAction))
			}
		}
		return o
	}
	ctx := b.Context()
	for i := range pages { // first captures are not measured
		if _, err := s.IngestObservation(ctx, graphstore.IngestRequest{TenantID: tenant, ProgramID: prog, Observation: capture(i)}); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	i := pages
	for b.Loop() {
		if _, err := s.IngestObservation(ctx, graphstore.IngestRequest{TenantID: tenant, ProgramID: prog, Observation: capture(i)}); err != nil {
			b.Fatal(err)
		}
		i++
	}
	secs := b.Elapsed().Seconds()
	b.ReportMetric(float64(i-pages)/secs, "captures/s")
	b.ReportMetric(float64((i-pages)*edges)/secs, "edges/s")
}
