package storetest

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/oam"
)

func sortStrings(s []string) { slices.Sort(s) }

func same(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

// level returns fixture refs ordered as Neighbors orders entities:
// start, then each further level sorted by id.
func (fx fixture) level(levels ...[]string) []string {
	var out []string
	for _, l := range levels {
		out = append(out, fx.sorted(l...)...)
	}
	return out
}

// Neighbors checks the bounded BFS of spec 5.9: direction, labels, depth,
// since, max_nodes truncation and each entity and edge at most once.
func Neighbors(t *testing.T, f Factory) {
	e, fx := newFixtureEnv(t, f)
	nb := func(req graphstore.NeighborsRequest) graphstore.Subgraph {
		t.Helper()
		sg, err := e.s.Neighbors(e.ctx, e.c, req)
		if err != nil {
			t.Fatalf("neighbors %+v: %v", req, err)
		}
		seen := map[string]bool{}
		for _, x := range sg.Entities {
			if seen[x.ID] {
				t.Fatalf("entity %s returned twice", x.ID)
			}
			seen[x.ID] = true
		}
		for _, x := range sg.Edges {
			if seen[x.ID] || !seen[x.FromEntityID] || !seen[x.ToEntityID] {
				t.Fatalf("edge %s duplicated or dangling", x.ID)
			}
			seen[x.ID] = true
		}
		return sg
	}
	api := fx.ids["api"]
	sg := nb(graphstore.NeighborsRequest{EntityID: api, Direction: graphstore.DirectionOut})
	same(t, "out depth 1", fx.refs(entityIDs(sg.Entities)), fx.level([]string{"api"}, []string{"ip1"}))
	if len(sg.Edges) != 1 || sg.Edges[0].Label != "dns_record" || sg.Truncated {
		t.Errorf("out depth 1 edges: %+v truncated=%v", sg.Edges, sg.Truncated)
	}
	sg = nb(graphstore.NeighborsRequest{EntityID: api, Direction: graphstore.DirectionOut, Depth: 2})
	same(t, "out depth 2", fx.refs(entityIDs(sg.Entities)), fx.level([]string{"api"}, []string{"ip1"}, []string{"svc"}))
	sg = nb(graphstore.NeighborsRequest{EntityID: api, Direction: graphstore.DirectionIn})
	same(t, "in depth 1", fx.refs(entityIDs(sg.Entities)), fx.level([]string{"api"}, []string{"www", "org"}))
	sg = nb(graphstore.NeighborsRequest{EntityID: api, Direction: graphstore.DirectionBoth, Depth: 2, Labels: []string{"DNS_Record"}})
	same(t, "both dns depth 2", fx.refs(entityIDs(sg.Entities)), fx.level([]string{"api"}, []string{"ip1", "www"}))
	if len(sg.Edges) != 2 {
		t.Errorf("both dns depth 2: %d edges, want 2", len(sg.Edges))
	}
	// www -> ip1 (related_to) and www -> api (dns) form a triangle with
	// api -> ip1: the edge between two depth-1 nodes is followed at depth 2.
	sg = nb(graphstore.NeighborsRequest{EntityID: fx.ids["www"], Direction: graphstore.DirectionOut, Depth: 2})
	same(t, "www out depth 2", fx.refs(entityIDs(sg.Entities)), fx.level([]string{"www"}, []string{"api", "ip1"}, []string{"svc"}))
	if len(sg.Edges) != 4 {
		t.Errorf("www out depth 2: %d edges, want 4 (www-api, www-ip1, api-ip1, ip1-svc)", len(sg.Edges))
	}
	sg = nb(graphstore.NeighborsRequest{EntityID: api, Direction: graphstore.DirectionBoth, Depth: 6, MaxNodes: 3})
	if len(sg.Entities) != 3 || !sg.Truncated {
		t.Errorf("max_nodes 3: %d entities, truncated=%v", len(sg.Entities), sg.Truncated)
	}
	sg = nb(graphstore.NeighborsRequest{EntityID: api, Direction: graphstore.DirectionBoth, Depth: 6})
	if len(sg.Entities) != 9 || sg.Truncated {
		t.Errorf("whole component: %d entities (want 9), truncated=%v", len(sg.Entities), sg.Truncated)
	}
	sg = nb(graphstore.NeighborsRequest{EntityID: fx.ids["ip1"], Direction: graphstore.DirectionBoth, Since: T0.Add(2 * time.Minute)})
	same(t, "since", fx.refs(entityIDs(sg.Entities)), fx.level([]string{"ip1"}, []string{"net"}))
	for _, bad := range []graphstore.NeighborsRequest{
		{EntityID: api},
		{EntityID: api, Direction: graphstore.DirectionOut, Depth: 7},
		{EntityID: api, Direction: graphstore.DirectionOut, MaxNodes: -1},
		{EntityID: "nope", Direction: graphstore.DirectionOut},
	} {
		_, err := e.s.Neighbors(e.ctx, e.c, bad)
		wantKind(t, "bad neighbors request", err, core.KindInvalidArgument)
	}
}

// Path checks Dijkstra with cost -ln(confidence) (spec 5.9).
func Path(t *testing.T, f Factory) {
	e, fx := newFixtureEnv(t, f)
	path := func(req graphstore.FindPathRequest) graphstore.Path {
		t.Helper()
		p, err := e.s.FindPath(e.ctx, e.c, req)
		if err != nil {
			t.Fatalf("path: %v", err)
		}
		return p
	}
	p := path(graphstore.FindPathRequest{FromEntityID: fx.ids["www"], ToEntityID: fx.ids["svc"]})
	same(t, "most likely path", fx.refs(p.EntityIDs), []string{"www", "api", "ip1", "svc"})
	if math.Abs(p.CombinedConfidence-0.9*0.9*0.8) > 1e-12 || math.Abs(p.TotalCost+math.Log(0.9*0.9*0.8)) > 1e-9 || len(p.Edges) != 3 {
		t.Errorf("path confidence %v cost %v edges %d", p.CombinedConfidence, p.TotalCost, len(p.Edges))
	}
	p = path(graphstore.FindPathRequest{FromEntityID: fx.ids["www"], ToEntityID: fx.ids["svc"], Labels: []string{"related_to", "port"}})
	same(t, "label-restricted path", fx.refs(p.EntityIDs), []string{"www", "ip1", "svc"})
	if math.Abs(p.CombinedConfidence-0.3*0.8) > 1e-12 {
		t.Errorf("restricted confidence %v", p.CombinedConfidence)
	}
	if p = path(graphstore.FindPathRequest{FromEntityID: fx.ids["www"], ToEntityID: fx.ids["svc"], MaxCost: 0.3}); len(p.EntityIDs) != 0 {
		t.Errorf("max_cost 0.3 found %v", fx.refs(p.EntityIDs))
	}
	if p = path(graphstore.FindPathRequest{FromEntityID: fx.ids["svc"], ToEntityID: fx.ids["www"]}); len(p.EntityIDs) != 0 {
		t.Errorf("paths are directed, found %v", fx.refs(p.EntityIDs))
	}
	if p = path(graphstore.FindPathRequest{FromEntityID: fx.ids["api"], ToEntityID: fx.ids["api"]}); len(p.EntityIDs) != 1 || p.CombinedConfidence != 1 {
		t.Errorf("trivial path %+v", p)
	}
	_, err := e.s.FindPath(e.ctx, e.c, graphstore.FindPathRequest{FromEntityID: fx.ids["api"], ToEntityID: fx.ids["svc"], MaxCost: -1})
	wantKind(t, "negative max_cost", err, core.KindInvalidArgument)
}

// BlastAndComponents checks blast_radius and components (spec 5.9).
func BlastAndComponents(t *testing.T, f Factory) {
	e, fx := newFixtureEnv(t, f)
	reach, err := e.s.BlastRadius(e.ctx, e.c, graphstore.BlastRadiusRequest{EntityID: fx.ids["org"]})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	var dist []int
	for _, r := range reach {
		ids, dist = append(ids, r.Entity.ID), append(dist, r.Distance)
	}
	same(t, "blast radius", fx.refs(ids), fx.level([]string{"api", "jenkins"}, []string{"ip1", "ip2"}, []string{"svc", "svc2"}))
	if !slices.Equal(dist, []int{1, 1, 2, 2, 3, 3}) {
		t.Errorf("blast distances %v", dist)
	}
	reach, err = e.s.BlastRadius(e.ctx, e.c, graphstore.BlastRadiusRequest{EntityID: fx.ids["org"], Depth: 1})
	if err != nil || len(reach) != 2 {
		t.Errorf("blast depth 1: %d %v", len(reach), err)
	}
	comp, err := e.s.Components(e.ctx, e.c, e.prog)
	if err != nil {
		t.Fatal(err)
	}
	if len(comp.Groups) != 2 || len(comp.Groups[0])+len(comp.Groups[1]) != 10 || comp.WatermarkSeq == 0 {
		t.Fatalf("components %+v", comp)
	}
	for _, g := range comp.Groups {
		if len(g) == 1 && g[0] != fx.ids["old"] {
			t.Errorf("singleton component %v, want old", fx.refs(g))
		}
	}
}

// FindEntities checks filters and paging (page_size default 50, max 500).
func FindEntities(t *testing.T, f Factory) {
	e, fx := newFixtureEnv(t, f)
	find := func(req graphstore.FindEntitiesRequest) graphstore.FindEntitiesResult {
		t.Helper()
		req.ProgramID = e.prog
		res, err := e.s.FindEntities(e.ctx, e.c, req)
		if err != nil {
			t.Fatalf("find %+v: %v", req, err)
		}
		return res
	}
	all := find(graphstore.FindEntitiesRequest{})
	if len(all.Entities) != 10 || all.NextPageToken != "" {
		t.Fatalf("all: %d entities, token %q", len(all.Entities), all.NextPageToken)
	}
	if !slices.IsSortedFunc(all.Entities, func(a, b graphstore.Entity) int { return cmpStr(a.ID, b.ID) }) {
		t.Error("entities not ordered by id")
	}
	var paged []string
	tok := ""
	for range 10 {
		res := find(graphstore.FindEntitiesRequest{AssetTypes: []string{oam.TypeFQDN}, Page: graphstore.PageRequest{PageSize: 3, PageToken: tok}})
		paged = append(paged, entityIDs(res.Entities)...)
		if tok = res.NextPageToken; tok == "" {
			break
		}
	}
	same(t, "paged FQDNs", fx.refs(paged), fx.sorted("api", "jenkins", "www", "old"))
	same(t, "out_of_scope", fx.refs(entityIDs(find(graphstore.FindEntitiesRequest{ScopeStatus: oam.ScopeOutOfScope}).Entities)), []string{"old"})
	same(t, "key prefix", fx.refs(entityIDs(find(graphstore.FindEntitiesRequest{KeyPrefix: "fqdn:j"}).Entities)), []string{"jenkins"})
	same(t, "seen after", fx.refs(entityIDs(find(graphstore.FindEntitiesRequest{SeenAfter: T0.Add(3 * time.Minute)}).Entities)), fx.sorted("jenkins", "old"))
	if n := len(find(graphstore.FindEntitiesRequest{State: oam.StateStale}).Entities); n != 0 {
		t.Errorf("stale: %d", n)
	}
	for _, bad := range []graphstore.FindEntitiesRequest{
		{ProgramID: e.prog, Page: graphstore.PageRequest{PageSize: graphstore.MaxPageSize + 1}},
		{ProgramID: e.prog, Page: graphstore.PageRequest{PageToken: "garbage!"}},
		{ProgramID: e.prog, State: "gone"},
		{ProgramID: "prg_bad"},
	} {
		_, err := e.s.FindEntities(e.ctx, e.c, bad)
		wantKind(t, "bad find request", err, core.KindInvalidArgument)
	}
	ent := e.entity(fx.ids["jenkins"])
	if ent.Confidence != oam.CombineConfidence(oam.SourceReport{Source: "dnsx", Confidence: 0.9},
		oam.SourceReport{Source: "asnmap", Confidence: 0.6}, oam.SourceReport{Source: "subfinder", Confidence: 0.5}) {
		t.Errorf("jenkins confidence %v", ent.Confidence)
	}
	if len(ent.Tags) != 3 || ent.ScopeStatus != oam.ScopeInScope || ent.FirstSeenRun != fx.runs[0] || ent.LastSeenRun != fx.runs[3] {
		t.Errorf("jenkins %+v", ent)
	}
}

func cmpStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Search checks the trigram search (spec 5.9, 10.7, 12.7 example).
func Search(t *testing.T, f Factory) {
	e, fx := newFixtureEnv(t, f)
	search := func(req graphstore.SearchRequest) []string {
		t.Helper()
		req.ProgramID = e.prog
		res, err := e.s.Search(e.ctx, e.c, req)
		if err != nil {
			t.Fatalf("search %+v: %v", req, err)
		}
		var ids []string
		for i, h := range res.Hits {
			if i > 0 && h.Score > res.Hits[i-1].Score {
				t.Errorf("hits not ranked by score")
			}
			ids = append(ids, h.Entity.ID)
		}
		return fx.refs(ids)
	}
	same(t, "spec example jenkin fuzzy limit 2", search(graphstore.SearchRequest{Query: "jenkin", Fuzzy: true, Limit: 2}), []string{"jenkins", "svc2"})
	same(t, "jenkin limit 1", search(graphstore.SearchRequest{Query: "Jenkin", Limit: 1}), []string{"jenkins"})
	same(t, "typo, not fuzzy", search(graphstore.SearchRequest{Query: "jenkims.acme.com"}), nil)
	same(t, "typo, fuzzy", search(graphstore.SearchRequest{Query: "jenkims.acme.com", Fuzzy: true, Limit: 1}), []string{"jenkins"})
	same(t, "types", search(graphstore.SearchRequest{Query: "jenkin", AssetTypes: []string{oam.TypeService}}), []string{"svc2"})
	same(t, "short query", search(graphstore.SearchRequest{Query: "ap"}), []string{"api"})
	same(t, "ip substring", search(graphstore.SearchRequest{Query: "113.2"}), fx.sorted("ip2"))
	same(t, "like metacharacters are literal", search(graphstore.SearchRequest{Query: "a%e"}), nil)
	res, err := e.s.Search(e.ctx, e.c, graphstore.SearchRequest{ProgramID: e.prog, Query: "jenkin", Limit: 1})
	if err != nil || len(res.Hits) != 1 || len(res.Hits[0].MatchedTrigrams) != 6 || math.Abs(res.Hits[0].Score-6.0/18) > 1e-12 {
		t.Errorf("score/trigrams: %+v %v", res.Hits, err)
	}
	for _, bad := range []graphstore.SearchRequest{{ProgramID: e.prog}, {ProgramID: e.prog, Query: "x", Limit: graphstore.MaxSearchLimit + 1}} {
		_, err := e.s.Search(e.ctx, e.c, bad)
		wantKind(t, "bad search", err, core.KindInvalidArgument)
	}
}

// Diff checks diff(since) and diff(run_a, run_b) from change events, paged.
func Diff(t *testing.T, f Factory) {
	e, fx := newFixtureEnv(t, f)
	all := e.changes(0)
	diff := func(req graphstore.DiffRequest) []graphstore.ChangeEvent {
		t.Helper()
		req.ProgramID = e.prog
		var out []graphstore.ChangeEvent
		for range 100 {
			res, err := e.s.Diff(e.ctx, e.c, req)
			if err != nil {
				t.Fatalf("diff: %v", err)
			}
			if len(res.Changes) > max(req.Page.PageSize, 0) && req.Page.PageSize > 0 {
				t.Fatalf("page of %d > %d", len(res.Changes), req.Page.PageSize)
			}
			out = append(out, res.Changes...)
			if req.Page.PageToken = res.NextPageToken; req.Page.PageToken == "" {
				return out
			}
		}
		t.Fatal("diff paging does not end")
		return nil
	}
	since := T0.Add(2 * time.Minute)
	var want []string
	for _, ev := range all {
		if !ev.At.Before(since) {
			want = append(want, ev.ID)
		}
	}
	got := diff(graphstore.DiffRequest{Since: since, Page: graphstore.PageRequest{PageSize: 2}})
	var ids []string
	for _, ev := range got {
		ids = append(ids, ev.ID)
		if ev.EntityID != "" && (ev.Entity == nil || ev.Entity.ID != ev.EntityID) {
			t.Errorf("event %s without entity snapshot", ev.ID)
		}
	}
	same(t, "diff since", ids, want)
	if len(want) == 0 {
		t.Fatal("fixture has no events after since")
	}
	// Between runs r1 and r2: exactly the events after r1's last change up
	// to r2's last change (fx-2: two services and two port edges).
	got = diff(graphstore.DiffRequest{RunIDA: fx.runs[0], RunIDB: fx.runs[1], Page: graphstore.PageRequest{PageSize: 3}})
	types := map[oam.ChangeType]int{}
	for _, ev := range got {
		types[ev.Type]++
		if ev.RunID != fx.runs[1] {
			t.Errorf("diff r1..r2 contains event of run %q", ev.RunID)
		}
	}
	if types[oam.ChangeEntityAdded] != 2 || types[oam.ChangeEdgeAdded] != 2 || len(got) != 4 {
		t.Errorf("diff r1..r2: %v", types)
	}
	if got = diff(graphstore.DiffRequest{RunIDA: fx.runs[2]}); len(got) != 1 || got[0].Type != oam.ChangeEntityAdded {
		t.Errorf("diff after r3: %+v", got)
	}
	_, err := e.s.Diff(e.ctx, e.c, graphstore.DiffRequest{ProgramID: e.prog, RunIDA: RunID("never")})
	wantKind(t, "unknown run", err, core.KindNotFound)
	_, err = e.s.Diff(e.ctx, e.c, graphstore.DiffRequest{ProgramID: e.prog})
	wantKind(t, "neither since nor run", err, core.KindInvalidArgument)
	_, err = e.s.Diff(e.ctx, e.c, graphstore.DiffRequest{ProgramID: e.prog, Since: since, RunIDA: fx.runs[0]})
	wantKind(t, "both since and run", err, core.KindInvalidArgument)
}
