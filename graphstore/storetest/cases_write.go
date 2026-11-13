package storetest

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/csr"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/oam"
)

// Idempotent checks that re-ingesting an observation id has no effect
// (spec 6.1), across programs of the tenant too.
func Idempotent(t *testing.T, f Factory) {
	e, _ := newFixtureEnv(t, f)
	before, evs := Canon(t, e.export()), len(e.changes(0))
	for _, req := range fixtureObservations() {
		req.TenantID, req.ProgramID = e.tenant, e.prog
		res, err := e.s.IngestObservation(e.ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Duplicate || res.EntitiesCreated+res.EntitiesUpdated+res.EdgesCreated+res.EdgesUpdated != 0 || len(res.Changes) != 0 {
			t.Fatalf("re-ingest %s had an effect: %+v", req.Observation.ObservationID, res)
		}
		req.ProgramID = ProgramID("other")
		if res, err = e.s.IngestObservation(e.ctx, req); err != nil || !res.Duplicate {
			t.Fatalf("observation id reused in another program: %+v %v", res, err)
		}
	}
	if after := Canon(t, e.export()); after != before || len(e.changes(0)) != evs {
		t.Fatal("re-ingest changed the graph")
	}
	o := obs("first", "dnsx", T0, 0.5, []oam.ObservedEntity{ent("a", oam.FQDN{Name: "x.acme.com"})})
	res := e.ingest(o)
	if res.EntitiesCreated != 1 || len(res.Changes) != 1 || res.Changes[0].Entity == nil || res.Changes[0].Seq == 0 {
		t.Fatalf("first ingest: %+v", res)
	}
	if res = e.ingest(o); !res.Duplicate {
		t.Fatal("second ingest not a duplicate")
	}
}

// Atomic checks that a rejected observation leaves no trace and that
// rejections are INVALID_ARGUMENT with stable reasons.
func Atomic(t *testing.T, f Factory) {
	e, fx := newFixtureEnv(t, f)
	before, evs := Canon(t, e.export()), len(e.changes(0))
	bad := []oam.Observation{
		obs("bad-1", "x", T0, 0.5, []oam.ObservedEntity{ent("a", oam.FQDN{Name: "new.acme.com"})}, simple("a", "zz", "related_to")),
		obs("bad-3", "x", T0, 0.5, []oam.ObservedEntity{ent("s", oam.Service{UniqueID: "s9"})}, simple("s", fx.ids["api"], "certificate")),
		obs("bad-4", "", T0, 0.5, []oam.ObservedEntity{ent("a", oam.FQDN{Name: "new.acme.com"})}),
		obs("bad-5", "x", T0, 0.5, []oam.ObservedEntity{ent("a", oam.FQDN{Name: "new\x00.acme.com"})}),
	}
	for _, o := range bad {
		_, err := e.s.IngestObservation(e.ctx, graphstore.IngestRequest{TenantID: e.tenant, ProgramID: e.prog, Observation: o})
		wantKind(t, o.ObservationID, err, core.KindInvalidArgument)
		if !errors.Is(err, &core.Error{Kind: core.KindInvalidArgument, Reason: core.ReasonValidationFailed}) {
			t.Errorf("%s: reason of %v is not VALIDATION_FAILED", o.ObservationID, err)
		}
	}
	if after := Canon(t, e.export()); after != before || len(e.changes(0)) != evs {
		t.Fatal("rejected observations changed the graph")
	}
	// A rejected id is not consumed.
	ok := obs("bad-1", "x", T0, 0.5, []oam.ObservedEntity{ent("a", oam.FQDN{Name: "new.acme.com"})})
	if res := e.ingest(ok); res.Duplicate || res.EntitiesCreated != 1 {
		t.Fatalf("retry after rejection: %+v", res)
	}
	for _, req := range []graphstore.IngestRequest{
		{ProgramID: e.prog, Observation: ok},
		{TenantID: e.tenant, Observation: ok},
		{TenantID: "ten_x", ProgramID: e.prog, Observation: ok},
		{TenantID: e.tenant, ProgramID: e.prog, RunID: RunID("a"), Observation: withRun(ok, RunID("b"))},
		{TenantID: e.tenant, ProgramID: e.prog, Observation: withTenant(ok, TenantID("other"))},
		{TenantID: e.tenant, ProgramID: e.prog, ToolName: "y", Observation: ok},
	} {
		_, err := e.s.IngestObservation(e.ctx, req)
		wantKind(t, "bad request", err, core.KindInvalidArgument)
	}
	pendingReferences(t, e)
}

// pendingReferences checks G-CORE-03 on a store: an observation naming an
// entity id that is not ingested yet waits without changing the graph, a
// re-send is absorbed, and the ingest that creates the entity applies it
// (IngestResult.Released) with its change events.
func pendingReferences(t *testing.T, e *env) {
	t.Helper()
	missing := oam.EntityID(e.tenant, e.prog, oam.TypeFQDN, "fqdn:missing.acme.com")
	waiting := obs("waits", "x", T0, 0.5, []oam.ObservedEntity{ent("a", oam.FQDN{Name: "waiting.acme.com"})},
		simple("a", missing, "related_to"))
	before := Canon(t, e.export())
	for range 2 {
		res := e.ingest(waiting)
		if !res.Pending || len(res.Missing) != 1 || res.Missing[0] != missing || len(res.Changes) != 0 {
			t.Fatalf("pending ingest: %+v", res)
		}
	}
	if Canon(t, e.export()) != before {
		t.Fatal("a pending observation changed the graph")
	}
	res := e.ingest(obs("creates", "x", T0, 0.5, []oam.ObservedEntity{ent("m", oam.FQDN{Name: "missing.acme.com"})}))
	if len(res.Released) != 1 || res.Released[0] != "waits" {
		t.Fatalf("released %v, want [waits]", res.Released)
	}
	var added int
	for _, c := range res.Changes {
		if c.Type == oam.ChangeEntityAdded || c.Type == oam.ChangeEdgeAdded {
			added++
		}
	}
	if added != 3 { // missing.acme.com, waiting.acme.com, the edge
		t.Fatalf("change events of the release: %+v", res.Changes)
	}
	if again := e.ingest(waiting); !again.Duplicate {
		t.Fatalf("re-sent released observation: %+v, want duplicate", again)
	}

	// An authoritative capture whose source is named by id waits for the
	// source like any other reference, and its unit is applied on release.
	page := oam.URL{URL: "https://late.acme.com/"}
	pageID := EntityIDIn(e.tenant, e.prog, page)
	capture := obs("capture", "extract", T0, 0.9, []oam.ObservedEntity{ent("s", oam.File{URL: "https://late.acme.com/a.js"})},
		simple(pageID, "s", oam.LabelLoadsScript))
	capture.Authority = &oam.Authority{Source: pageID, Profile: "default", Coverage: oam.CoverageComplete, CoveredRules: []string{oam.FamilyScripts}}
	if res := e.ingest(capture); !res.Pending || len(res.Missing) != 1 || res.Missing[0] != pageID {
		t.Fatalf("capture of a missing source: %+v, want pending on %s", res, pageID)
	}
	res = e.ingest(obs("page", "crawl", T0, 0.5, []oam.ObservedEntity{ent("p", page)}))
	if len(res.Released) != 1 || res.Released[0] != "capture" {
		t.Fatalf("released %v, want [capture]", res.Released)
	}
	var edges int
	for _, c := range res.Changes {
		if c.Type == oam.ChangeEdgeAdded {
			edges++
		}
	}
	if edges != 1 {
		t.Fatalf("change events of the authoritative release: %+v", res.Changes)
	}
}

func withRun(o oam.Observation, run string) oam.Observation       { o.RunID = run; return o }
func withTenant(o oam.Observation, tenant string) oam.Observation { o.TenantID = tenant; return o }

// Staleness checks covering-run staleness (spec 5.7) through SetStale and
// its effect on reads, traversals and snapshots.
func Staleness(t *testing.T, f Factory) {
	e := newEnv(t, f, Options{})
	seen := func(run string, at time.Time, names ...string) {
		var ents []oam.ObservedEntity
		var edges []oam.ObservedEdge
		for i, n := range names {
			ents = append(ents, ent(n, oam.FQDN{Name: n + ".acme.com"}))
			if i > 0 {
				edges = append(edges, dns(names[0], n, 5))
			}
		}
		o := obs("st-"+run+fmt.Sprint(len(names)), "dnsx", at, 0.9, ents, edges...)
		if _, err := e.s.IngestObservation(e.ctx, graphstore.IngestRequest{TenantID: e.tenant, ProgramID: e.prog, RunID: RunID(run), Observation: o}); err != nil {
			t.Fatal(err)
		}
	}
	cover := func(run string, n int) graphstore.SetStaleResult {
		t.Helper()
		res, err := e.s.SetStale(e.ctx, e.c, graphstore.SetStaleRequest{ProgramID: e.prog, RunID: RunID(run), ToolName: "dnsx", StaleAfterRuns: n})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	seen("r1", T0, "a", "b")
	cover("r1", 0)
	for i, run := range []string{"r2", "r3", "r4"} {
		seen(run, T0.Add(time.Duration(i+1)*time.Hour), "a")
		res := cover(run, 0)
		if (run == "r4") != (res.EntitiesMarked == 1 && res.EdgesMarked == 1) {
			t.Fatalf("%s: marked %d entities, %d edges", run, res.EntitiesMarked, res.EdgesMarked)
		}
		if run == "r4" && (len(res.Changes) != 2 || res.Changes[0].Type != oam.ChangeEntityStale || res.Changes[0].Entity == nil ||
			res.Changes[0].Entity.State != oam.StateStale || res.Changes[1].Type != oam.ChangeEdgeRemoved) {
			t.Fatalf("r4 changes %+v", res.Changes)
		}
	}
	if res := cover("r4", 0); !res.Duplicate || res.EntitiesMarked != 0 {
		t.Fatalf("repeated covering run: %+v", res)
	}
	a := oam.EntityID(e.tenant, e.prog, oam.TypeFQDN, "fqdn:a.acme.com")
	b := oam.EntityID(e.tenant, e.prog, oam.TypeFQDN, "fqdn:b.acme.com")
	if eb := e.entity(b); eb.State != oam.StateStale || eb.MissedRuns["dnsx"] != 3 {
		t.Fatalf("b: %+v", eb)
	}
	stale, err := e.s.FindEntities(e.ctx, e.c, graphstore.FindEntitiesRequest{ProgramID: e.prog, State: oam.StateStale})
	if err != nil || len(stale.Entities) != 1 || stale.Entities[0].ID != b {
		t.Fatalf("stale listing %+v %v", stale.Entities, err)
	}
	sg, err := e.s.Neighbors(e.ctx, e.c, graphstore.NeighborsRequest{EntityID: a, Direction: graphstore.DirectionBoth})
	if err != nil || len(sg.Entities) != 1 || len(sg.Edges) != 0 {
		t.Fatalf("stale records visible to traversal: %+v %v", sg, err)
	}
	g, _, err := e.s.BuildSnapshot(e.ctx, e.c, e.prog)
	if err != nil || g.NumNodes() != 1 || g.NumEdges() != 0 || g.Meta().Flags&csr.FlagStaleExcluded == 0 {
		t.Fatalf("snapshot includes stale records: %v", err)
	}
	seen("r5", T0.Add(5*time.Hour), "a", "b")
	if eb := e.entity(b); eb.State != oam.StateActive || eb.MissedRuns["dnsx"] != 0 {
		t.Fatalf("b not reactivated: %+v", eb)
	}
	evs := e.changes(0)
	if last := evs[len(evs)-2:]; last[0].Type != oam.ChangeEntityReactivated || last[1].Type != oam.ChangeEdgeAdded {
		t.Fatalf("reactivation events %+v", last)
	}
	if res := cover("r6", 1); res.EntitiesMarked != 2 {
		t.Fatalf("stale_after_runs 1: %+v", res)
	}
	_, err = e.s.SetStale(e.ctx, e.c, graphstore.SetStaleRequest{ProgramID: e.prog, ToolName: "dnsx"})
	wantKind(t, "set stale without run", err, core.KindInvalidArgument)
}

// TenantIsolation checks that every read and write path filters by the
// caller's tenant: another tenant's ids are NOT_FOUND (spec 10.7, 11.6).
func TenantIsolation(t *testing.T, f Factory) {
	e, fx := newFixtureEnv(t, f)
	other := TenantID("other")
	e2 := &env{t: t, s: e.s, clock: e.clock, ctx: e.ctx, tenant: other, prog: e.prog, c: graphstore.Caller{TenantID: other}}
	fx2 := e2.load()
	for ref, id := range fx.ids {
		if fx2.ids[ref] == id {
			t.Fatalf("tenants share entity id %s", id)
		}
	}
	nobody := graphstore.Caller{TenantID: TenantID("nobody")}
	ctx := e.ctx
	notFound := func(what string, err error) {
		t.Helper()
		if !isNotFound(err) {
			t.Errorf("%s: %v, want NOT_FOUND", what, err)
		}
	}
	for _, c := range []graphstore.Caller{e2.c, nobody} {
		api := fx.ids["api"]
		_, err := e.s.GetEntity(ctx, c, graphstore.GetEntityRequest{EntityID: api})
		notFound("get", err)
		_, err = e.s.Neighbors(ctx, c, graphstore.NeighborsRequest{EntityID: api, Direction: graphstore.DirectionBoth})
		notFound("neighbors", err)
		_, err = e.s.FindPath(ctx, c, graphstore.FindPathRequest{FromEntityID: api, ToEntityID: fx.ids["svc"]})
		notFound("path", err)
		_, err = e.s.BlastRadius(ctx, c, graphstore.BlastRadiusRequest{EntityID: api})
		notFound("blast", err)
		_, err = e.s.AddTag(ctx, c, graphstore.AddTagRequest{TargetKind: graphstore.TargetEntity, TargetID: api, PropertyType: oam.PropertySimple, Name: "n", Content: map[string]any{"value": "v"}})
		notFound("add tag", err)
	}
	ex, err := e.s.Export(ctx, nobody, e.prog)
	if err != nil || len(ex.Entities)+len(ex.Edges) != 0 {
		t.Errorf("export for another tenant: %d %v", len(ex.Entities), err)
	}
	fr, err := e.s.FindEntities(ctx, nobody, graphstore.FindEntitiesRequest{ProgramID: e.prog})
	if err != nil || len(fr.Entities) != 0 {
		t.Errorf("find for another tenant: %d %v", len(fr.Entities), err)
	}
	sr, err := e.s.Search(ctx, nobody, graphstore.SearchRequest{ProgramID: e.prog, Query: "acme", Fuzzy: true})
	if err != nil || len(sr.Hits) != 0 {
		t.Errorf("search for another tenant: %d %v", len(sr.Hits), err)
	}
	ch, err := e.s.ReadChanges(ctx, nobody, graphstore.ChangesRequest{ProgramID: e.prog})
	if err != nil || len(ch) != 0 {
		t.Errorf("changes for another tenant: %d %v", len(ch), err)
	}
	dr, err := e.s.Diff(ctx, nobody, graphstore.DiffRequest{ProgramID: e.prog, Since: T0.Add(-time.Hour)})
	if err != nil || len(dr.Changes) != 0 {
		t.Errorf("diff for another tenant: %d %v", len(dr.Changes), err)
	}
	_, err = e.s.Diff(ctx, nobody, graphstore.DiffRequest{ProgramID: e.prog, RunIDA: fx.runs[0]})
	notFound("diff by another tenant's run", err)
	g, wm, err := e.s.BuildSnapshot(ctx, nobody, e.prog)
	if err != nil || g.NumNodes() != 0 || wm != 0 {
		t.Errorf("snapshot for another tenant: %v", err)
	}
	comp, err := e.s.Components(ctx, nobody, e.prog)
	if err != nil || len(comp.Groups) != 0 {
		t.Errorf("components for another tenant: %v", err)
	}
	_, err = e.s.LatestSnapshot(ctx, nobody, e.prog)
	notFound("latest snapshot", err)
	// Each tenant sees exactly its own graph, identical up to ids.
	a, b := e.export(), e2.export()
	if len(a.Entities) != len(b.Entities) || len(a.Edges) != len(b.Edges) {
		t.Errorf("tenant graphs differ: %d/%d vs %d/%d", len(a.Entities), len(a.Edges), len(b.Entities), len(b.Edges))
	}
	for _, x := range b.Entities {
		if x.TenantID != other {
			t.Errorf("entity %s of tenant %s in other's export", x.ID, x.TenantID)
		}
	}
	// Referencing another tenant's entity in an observation never resolves:
	// the observation waits as a pending reference (G-CORE-03), and the
	// other tenant's graph is untouched.
	o := obs("x-ref", "x", T0, 0.5, []oam.ObservedEntity{ent("u", oam.URL{URL: "https://api.acme.com/"})}, simple("u", fx.ids["api"], "domain"))
	res, err := e.s.IngestObservation(ctx, graphstore.IngestRequest{TenantID: other, ProgramID: e.prog, Observation: o})
	if err != nil || !res.Pending || len(res.Changes) != 0 {
		t.Errorf("cross-tenant reference: %+v %v, want pending", res, err)
	}
	if after := e2.export(); len(after.Entities) != len(b.Entities) || len(after.Edges) != len(b.Edges) {
		t.Errorf("cross-tenant reference changed the other tenant's graph")
	}
	// Tagging another tenant's edge is NOT_FOUND.
	if len(a.Edges) > 0 {
		_, err = e.s.AddTag(ctx, e2.c, graphstore.AddTagRequest{TargetKind: graphstore.TargetEdge, TargetID: a.Edges[0].ID,
			PropertyType: oam.PropertySimple, Name: "n", Content: map[string]any{"value": "v"}})
		notFound("tag other tenant's edge", err)
	}
}

// WatchResume checks WatchChanges: live delivery, resumption by after_seq
// and the types filter.
func WatchResume(t *testing.T, f Factory) {
	e := newEnv(t, f, Options{})
	reqs := fixtureObservations()
	ingest := func(i int) graphstore.IngestResult {
		req := reqs[i]
		req.TenantID, req.ProgramID = e.tenant, e.prog
		res, err := e.s.IngestObservation(e.ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	first := ingest(0)
	watch := func(after int64, types ...oam.ChangeType) (<-chan graphstore.ChangeEvent, context.CancelFunc, <-chan error) {
		ctx, cancel := context.WithCancel(e.ctx)
		ch := make(chan graphstore.ChangeEvent, 100)
		done := make(chan error, 1)
		go func() {
			done <- e.s.WatchChanges(ctx, e.c, graphstore.WatchRequest{ProgramID: e.prog, AfterSeq: after, Types: types}, func(ev graphstore.ChangeEvent) error {
				ch <- ev
				return nil
			})
		}()
		return ch, cancel, done
	}
	recv := func(ch <-chan graphstore.ChangeEvent, n int) []graphstore.ChangeEvent {
		t.Helper()
		var out []graphstore.ChangeEvent
		timeout := time.After(20 * time.Second)
		for len(out) < n {
			select {
			case ev := <-ch:
				out = append(out, ev)
			case <-timeout:
				t.Fatalf("watch delivered %d of %d events", len(out), n)
			}
		}
		return out
	}
	ch, cancel, done := watch(0)
	got := recv(ch, len(first.Changes))
	for i, ev := range got {
		if ev.ID != first.Changes[i].ID || ev.Seq != first.Changes[i].Seq {
			t.Fatalf("backlog event %d: %s/%d, want %s/%d", i, ev.ID, ev.Seq, first.Changes[i].ID, first.Changes[i].Seq)
		}
	}
	second := ingest(1)
	live := recv(ch, len(second.Changes))
	if live[len(live)-1].Seq != second.Changes[len(second.Changes)-1].Seq {
		t.Fatal("live events out of order")
	}
	cancel()
	if err := <-done; core.KindOf(err) != core.KindAborted {
		t.Fatalf("cancelled watch returned %v", err)
	}
	ingest(2)
	third := ingest(3)
	resume := live[len(live)-1].Seq
	var want []string
	for _, ev := range e.changes(resume) {
		if ev.Type == oam.ChangeEntityAdded {
			want = append(want, ev.ID)
		}
	}
	ch, cancel, _ = watch(resume, oam.ChangeEntityAdded)
	defer cancel()
	var ids []string
	for _, ev := range recv(ch, len(want)) {
		ids = append(ids, ev.ID)
		if ev.Seq <= resume || ev.Type != oam.ChangeEntityAdded {
			t.Fatalf("resumed watch delivered %+v", ev)
		}
	}
	same(t, "resumed entity_added events", ids, want)
	if len(third.Changes) == 0 {
		t.Fatal("fixture step 3 has no changes")
	}
	_, err := e.s.ReadChanges(e.ctx, e.c, graphstore.ChangesRequest{ProgramID: e.prog, Types: []oam.ChangeType{"nope"}})
	wantKind(t, "unknown type filter", err, core.KindInvalidArgument)
}

// Snapshot checks the CSR snapshot builder (spec 21.2, 21.4, 21.6):
// determinism, stale exclusion, the catalogue and the blob store. With an
// oracle the sha256 must equal the oracle's for the same input.
func Snapshot(t *testing.T, f Factory, oracle Factory) {
	build := func(f Factory) (*env, *csr.Graph, uint64) {
		e, _ := newFixtureEnv(t, f)
		g, wm, err := e.s.BuildSnapshot(e.ctx, e.c, e.prog)
		if err != nil {
			t.Fatal(err)
		}
		return e, g, wm
	}
	e, g, wm := build(f)
	d1, _ := g.Digest()
	g2, wm2, _ := e.s.BuildSnapshot(e.ctx, e.c, e.prog)
	d2, _ := g2.Digest()
	if d1 != d2 || wm != wm2 {
		t.Fatal("snapshot build is not deterministic")
	}
	evs := e.changes(0)
	if wm != uint64(evs[len(evs)-1].Seq) {
		t.Errorf("watermark %d, newest seq %d", wm, evs[len(evs)-1].Seq)
	}
	if g.NumNodes() != 10 || g.NumEdges() != 10 || g.Meta().WatermarkSeq != wm {
		t.Errorf("snapshot V=%d E=%d", g.NumNodes(), g.NumEdges())
	}
	ids := g.NodeIDs()
	if !slices.IsSortedFunc(ids, func(a, b core.UUID) int { return slices.Compare(a[:], b[:]) }) {
		t.Error("snapshot nodes not in entity-id order")
	}
	if oracle != nil {
		_, og, owm := build(oracle)
		od, _ := og.Digest()
		if od != d1 || owm != wm {
			t.Errorf("snapshot differs from the oracle: wm %d vs %d", wm, owm)
		}
	}
	for i, blobs := range []graphstore.BlobStore{&graphstore.MemBlobs{}, graphstore.DirBlobs{Root: filepath.Join(t.TempDir(), "blobs")}} {
		nodes := int64(10 + i)
		if i > 0 {
			// Advance the watermark: a catalogue entry is kept per watermark.
			e.ingest(obs(fmt.Sprint("snap-", i), "x", T0, 0.5, []oam.ObservedEntity{ent("a", oam.FQDN{Name: fmt.Sprintf("s%d.acme.com", i)})}))
			g, _, _ := e.s.BuildSnapshot(e.ctx, e.c, e.prog)
			d1, _ = g.Digest()
			wm = g.Meta().WatermarkSeq
		}
		info, err := e.s.WriteSnapshot(e.ctx, e.c, e.prog, blobs)
		if err != nil {
			t.Fatal(err)
		}
		if again, err := e.s.WriteSnapshot(e.ctx, e.c, e.prog, blobs); err != nil || again.SnapshotID != info.SnapshotID || again.URI != info.URI {
			t.Errorf("rewrite at the same watermark: %+v %v", again, err)
		}
		if info.WatermarkSeq != wm || info.SHA256 != d1 || info.NodeCount != nodes || info.FormatVersion != csr.FormatVersion {
			t.Errorf("snapshot info %+v", info)
		}
		data, err := blobs.Get(e.ctx, info.URI)
		if err != nil {
			t.Fatal(err)
		}
		back, err := csr.Decode(data)
		if err != nil {
			t.Fatal(err)
		}
		if d, _ := back.Digest(); d != d1 {
			t.Error("stored snapshot digest differs")
		}
		latest, err := e.s.LatestSnapshot(e.ctx, e.c, e.prog)
		if err != nil || latest.WatermarkSeq != wm || latest.SnapshotID != info.SnapshotID {
			t.Errorf("latest %+v %v", latest, err)
		}
		if _, err := blobs.Get(e.ctx, info.URI+"x"); !errors.Is(err, graphstore.ErrBlobNotFound) {
			t.Errorf("missing blob: %v", err)
		}
	}
	if _, err := (graphstore.DirBlobs{Root: t.TempDir()}).Put(e.ctx, "../escape", nil); err == nil {
		t.Error("blob name escaping the root accepted")
	}
}

// AddTag checks API tags on entities and edges.
func AddTag(t *testing.T, f Factory) {
	e, fx := newFixtureEnv(t, f)
	e.clock.Set(T0.Add(time.Hour))
	tag, err := e.s.AddTag(e.ctx, e.c, graphstore.AddTagRequest{TargetKind: graphstore.TargetEntity, TargetID: fx.ids["api"],
		PropertyType: oam.PropertyFlowforge, Name: "triage", Content: map[string]any{"value": "high", "kind": "hypothesis"}})
	if err != nil {
		t.Fatal(err)
	}
	if tag.Name != "triage" || tag.TargetID != fx.ids["api"] || !tag.LastSeen.Equal(T0.Add(time.Hour)) || tag.ID == "" {
		t.Fatalf("tag %+v", tag)
	}
	ent := e.entity(fx.ids["api"])
	found := false
	for _, tg := range ent.Tags {
		found = found || (tg.PropertyType == oam.PropertyFlowforge && tg.Name == "triage" && tg.ID == tag.ID)
	}
	if !found || ent.Confidence != 0.96 {
		t.Fatalf("tag missing or confidence changed by tagging: %+v", ent)
	}
	ex := e.export()
	edge := ex.Edges[0]
	if _, err := e.s.AddTag(e.ctx, e.c, graphstore.AddTagRequest{TargetKind: graphstore.TargetEdge, TargetID: edge.ID,
		PropertyType: oam.PropertySimple, Content: map[string]any{"name": "note", "value": "seen in report"}}); err != nil {
		t.Fatal(err)
	}
	for _, x := range e.export().Edges {
		if x.ID == edge.ID && (len(x.Tags) != len(edge.Tags)+1) {
			t.Errorf("edge tags %+v", x.Tags)
		}
	}
	for _, bad := range []graphstore.AddTagRequest{
		{TargetKind: graphstore.TargetEntity, TargetID: fx.ids["api"], PropertyType: oam.PropertySource, Name: "x", Content: map[string]any{"confidence": 99}},
		{TargetKind: "node", TargetID: fx.ids["api"], PropertyType: oam.PropertySimple, Name: "x"},
		{TargetKind: graphstore.TargetEntity, TargetID: fx.ids["api"], PropertyType: oam.PropertyFlowforge, Name: "x", Content: map[string]any{"kind": "rumour"}},
		{TargetKind: graphstore.TargetEntity, TargetID: fx.ids["api"], PropertyType: oam.PropertySimple, Name: "x", Content: map[string]any{"name": "y"}},
	} {
		_, err := e.s.AddTag(e.ctx, e.c, bad)
		wantKind(t, fmt.Sprintf("bad tag %+v", bad), err, core.KindInvalidArgument)
	}
	_, err = e.s.AddTag(e.ctx, e.c, graphstore.AddTagRequest{TargetKind: graphstore.TargetEntity,
		TargetID: oam.EntityID(e.tenant, e.prog, oam.TypeFQDN, "fqdn:none.acme.com"), PropertyType: oam.PropertySimple, Name: "x"})
	wantKind(t, "tag unknown entity", err, core.KindNotFound)
}

// Convergence checks spec 23.5 on the store: random observation multisets
// applied in a random order with duplicates give the same graph as the
// original order, and (with an oracle) the same as the oracle store.
func Convergence(t *testing.T, f Factory, r core.Rng, cases int, oracle Factory) {
	scope := func(_, _ string, a oam.Asset) oam.ScopeStatus {
		if s, ok := a.(oam.Service); ok && s.ServiceType == "https" {
			return oam.ScopeInScope
		}
		return oam.ScopeUnknown
	}
	a := newEnv(t, f, Options{Scope: scope})
	b := newEnv(t, f, Options{Scope: scope})
	var o *env
	if oracle != nil {
		o = newEnv(t, oracle, Options{Scope: scope})
	}
	for c := range cases {
		prog := ProgramID(fmt.Sprint("conv-", c))
		list := make([]oam.Observation, core.IntN(r, 20))
		for i := range list {
			list[i] = RandomObservation(r, i, "", "")
			list[i].ObservationID = fmt.Sprintf("c%d-%s", c, list[i].ObservationID)
			AddIDReferences(r, &list[i], a.tenant, prog)
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
			t.Fatalf("case %d: state depends on order or duplication\nwant %s\n got %s", c, want, got)
		}
		if o != nil {
			if got := apply(o, list); got != want {
				t.Fatalf("case %d: state differs from the oracle\n got %s\nwant %s", c, want, got)
			}
		}
	}
}
