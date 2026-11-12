package csr

import (
	"cmp"
	"encoding/binary"
	"fmt"
	"slices"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// randomEdges draws a small random multigraph with self loops and
// duplicates.
func randomEdges(rng core.Rng, n, m, labels int) []Edge {
	edges := make([]Edge, m)
	for i := range edges {
		edges[i] = Edge{
			Src:   uint32(core.IntN(rng, n)),
			Dst:   uint32(core.IntN(rng, n)),
			Label: uint8(core.IntN(rng, labels)),
			Conf:  float32(core.IntN(rng, 11)) / 10,
		}
	}
	return edges
}

func labelNames(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("l%d", i)
	}
	return out
}

func randomGraph(t testing.TB, rng core.Rng, n, m, labels int) *Graph {
	t.Helper()
	g, err := Build(Meta{}, nodeUUIDs(n), nil, randomEdges(rng, n, m, labels), labelNames(labels))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func randomSmall(rng core.Rng) (int, []Edge) {
	n := 1 + core.IntN(rng, 25)
	return n, randomEdges(rng, n, core.IntN(rng, 3*n+1), 3)
}

func TestCSRProp_Build(t *testing.T) {
	rng := testkit.Rng(t)
	for range testkit.Cases() {
		n, edges := randomSmall(rng)
		g, err := Build(Meta{}, nodeUUIDs(n), nil, edges, labelNames(3))
		if err != nil {
			t.Fatal(err)
		}
		off := g.Offsets()
		var degSum uint64
		for v := range n {
			degSum += off[v+1] - off[v]
			row, _ := g.OutNeighbors(uint32(v))
			if !slices.IsSorted(row) {
				t.Fatalf("row %d not sorted: %v", v, row)
			}
			in, _ := g.InNeighbors(uint32(v))
			if !slices.IsSorted(in) {
				t.Fatalf("in-row %d not sorted: %v", v, in)
			}
		}
		if degSum != uint64(len(edges)) || g.NumEdges() != len(edges) {
			t.Fatalf("degree sum %d, E %d", degSum, len(edges))
		}
		// Same multiset of edges.
		key := func(a, b Edge) int {
			return cmp.Or(cmp.Compare(a.Src, b.Src), cmp.Compare(a.Dst, b.Dst), cmp.Compare(a.Label, b.Label), cmp.Compare(a.Conf, b.Conf))
		}
		want := slices.Clone(edges)
		slices.SortFunc(want, key)
		if got := g.Edges(); !slices.Equal(got, want) {
			t.Fatalf("edges %v, want %v", got, want)
		}
		// in_edge points back at the mirrored forward edge.
		for v := range n {
			srcs, _ := g.InNeighbors(uint32(v))
			idx, _ := g.InEdges(uint32(v))
			for i := range srcs {
				e, _ := g.EdgeAt(int(idx[i]))
				if e.Src != srcs[i] || e.Dst != uint32(v) {
					t.Fatalf("in_edge %d of %d is %+v", i, v, e)
				}
			}
		}
		// Transposed of transposed equals the original.
		rev := make([]Edge, len(edges))
		for i, e := range edges {
			rev[i] = Edge{Src: e.Dst, Dst: e.Src, Label: e.Label, Conf: e.Conf}
		}
		tg, err := Build(Meta{}, nodeUUIDs(n), nil, rev, labelNames(3))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(tg.InOffsets(), g.Offsets()) || !slices.Equal(tg.InSources(), g.Targets()) ||
			!slices.Equal(tg.Offsets(), g.InOffsets()) || !slices.Equal(tg.Targets(), g.InSources()) {
			t.Fatal("transpose of transpose differs")
		}
		// File round trip.
		b, err := g.Encode()
		if err != nil {
			t.Fatal(err)
		}
		d, err := Decode(b)
		if err != nil {
			t.Fatal(err)
		}
		assertSameGraph(t, g, d)
	}
}

func TestCSRProp_BFSNaive(t *testing.T) {
	rng := testkit.Rng(t)
	for range testkit.Cases() {
		n, edges := randomSmall(rng)
		g, err := Build(Meta{}, nodeUUIDs(n), nil, edges, labelNames(3))
		if err != nil {
			t.Fatal(err)
		}
		start := uint32(core.IntN(rng, n))
		dir := Direction(1 + core.IntN(rng, 3))
		depth := core.IntN(rng, 6) - 1
		labels := AnyLabel()
		if core.IntN(rng, 2) == 0 {
			labels = OnlyLabels(uint8(core.IntN(rng, 3)))
		}
		r, err := BFS(g, start, BFSOptions{MaxDepth: depth, Direction: dir, Labels: labels})
		if err != nil {
			t.Fatal(err)
		}
		want := naiveBFS(n, edges, start, dir, depth, labels)
		if got := r.Distances(n); !slices.Equal(got, want) {
			t.Fatalf("n=%d start=%d dir=%s depth=%d: dist %v, want %v", n, start, dir, depth, got, want)
		}
		if fmt.Sprint(r.Levels) != fmt.Sprint(levelsFromDist(want)) {
			t.Fatalf("levels %v", r.Levels)
		}
	}
}

func TestCSRProp_SCCNaive(t *testing.T) {
	rng := testkit.Rng(t)
	for range testkit.Cases() {
		n, edges := randomSmall(rng)
		g, err := Build(Meta{}, nodeUUIDs(n), nil, edges, labelNames(3))
		if err != nil {
			t.Fatal(err)
		}
		p := StronglyConnectedComponents(g)
		if got, want := canonicalGroups(p.Groups()), naiveSCC(n, edges); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("SCC %v, want %v", got, want)
		}
		checkReverseTopological(t, g, p)
		if got, want := WeaklyConnectedComponents(g).Groups(), naiveWCC(n, edges); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("WCC %v, want %v", got, want)
		}
		if got, want := CoreNumbers(g), naiveCoreNumbers(n, edges); !slices.Equal(got, want) {
			t.Fatalf("cores %v, want %v", got, want)
		}
		pts, brs := naiveCuts(n, edges)
		c := FindCuts(g)
		if fmt.Sprint(c.Points) != fmt.Sprint(pts) || fmt.Sprint(c.Bridges) != fmt.Sprint(brs) {
			t.Fatalf("cuts %v %v, want %v %v", c.Points, c.Bridges, pts, brs)
		}
		if got, want := TriangleCount(g), naiveTriangles(n, edges); got != want {
			t.Fatalf("triangles %d, want %d", got, want)
		}
		from, to := uint32(core.IntN(rng, n)), uint32(core.IntN(rng, n))
		p2, ok, err := ShortestPath(g, from, to, PathOptions{})
		if err != nil {
			t.Fatal(err)
		}
		wantConf, wantOK := naiveBestConfidence(n, edges, from, to, AnyLabel())
		if ok != wantOK || (ok && (p2.Confidence-wantConf > 1e-9 || wantConf-p2.Confidence > 1e-9)) {
			t.Fatalf("path %d->%d: %v %v, want %v %v", from, to, p2.Confidence, ok, wantConf, wantOK)
		}
		if ok {
			checkPathValid(t, g, p2, from, to)
		}
	}
}

func TestCSRProp_PageRankDense(t *testing.T) {
	rng := testkit.Rng(t)
	for range testkit.Cases() {
		n, edges := randomSmall(rng)
		g, err := Build(Meta{}, nodeUUIDs(n), nil, edges, labelNames(3))
		if err != nil {
			t.Fatal(err)
		}
		r, err := PageRank(g, PageRankOptions{Workers: 1 + core.IntN(rng, 4)})
		if err != nil {
			t.Fatal(err)
		}
		comparePageRank(t, r.Scores, densePageRank(n, edges, DefaultDamping))
	}
}

func TestCSRProp_OverlayEqualsRebuild(t *testing.T) {
	rng := testkit.Rng(t)
	for range testkit.Cases() {
		overlayEqualsRebuild(t, rng)
	}
}

type uEdge struct {
	from, to core.UUID
	label    string
	conf     float32
}

func randomUUID(rng core.Rng) core.UUID {
	var u core.UUID
	binary.BigEndian.PutUint64(u[:8], rng.Uint64()%4096) // small space: collisions with existing ids happen
	return u
}

// overlayEqualsRebuild applies random change events to an overlay and
// checks point queries and materialisation against a fresh Build of the
// resulting edge multiset (csr_008, property 25.4).
func overlayEqualsRebuild(t *testing.T, rng core.Rng) {
	t.Helper()
	labels := []string{"dns_record", "port"}
	var ids []core.UUID
	seen := map[core.UUID]bool{}
	types := map[core.UUID]uint8{}
	addID := func() core.UUID {
		for {
			u := randomUUID(rng)
			if !seen[u] {
				seen[u] = true
				ids = append(ids, u)
				types[u] = uint8(core.IntN(rng, 4))
				return u
			}
		}
	}
	n := 1 + core.IntN(rng, 12)
	for range n {
		addID()
	}
	var truth []uEdge
	for range core.IntN(rng, 3*n) {
		truth = append(truth, uEdge{ids[core.IntN(rng, n)], ids[core.IntN(rng, n)], labels[core.IntN(rng, 2)], float32(core.IntN(rng, 11)) / 10})
	}
	build := func(ids []core.UUID, es []uEdge, labels []string) *Graph {
		idx := map[core.UUID]uint32{}
		ty := make([]uint8, len(ids))
		for i, u := range ids {
			idx[u] = uint32(i)
			ty[i] = types[u]
		}
		var edges []Edge
		for _, e := range es {
			edges = append(edges, Edge{Src: idx[e.from], Dst: idx[e.to], Label: uint8(slices.Index(labels, e.label)), Conf: e.conf})
		}
		g, err := Build(Meta{WatermarkSeq: 10}, ids, ty, edges, labels)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	base := build(ids, truth, labels)
	o := NewOverlay(base)
	seq := uint64(10)
	allLabels := slices.Clone(labels)
	for range core.IntN(rng, 20) {
		seq += 1 + uint64(core.IntN(rng, 3))
		var c Change
		switch k := core.IntN(rng, 10); {
		case k < 2:
			c = Change{Seq: seq, Kind: ChangeNodeAdded}
			if core.IntN(rng, 4) == 0 {
				c.Node = ids[core.IntN(rng, len(ids))] // re-adding is a no-op
			} else {
				c.Node = addID()
			}
			c.NodeType = types[c.Node]
		case k < 7:
			lbl := []string{"dns_record", "port", "owns"}[core.IntN(rng, 3)]
			e := uEdge{ids[core.IntN(rng, len(ids))], ids[core.IntN(rng, len(ids))], lbl, float32(core.IntN(rng, 11)) / 10}
			c = Change{Seq: seq, Kind: ChangeEdgeAdded, From: e.from, To: e.to, Label: e.label, Conf: e.conf}
			truth = append(truth, e)
			if !slices.Contains(allLabels, lbl) {
				allLabels = append(allLabels, lbl)
			}
		default:
			var e uEdge
			if len(truth) > 0 && core.IntN(rng, 4) != 0 {
				e = truth[core.IntN(rng, len(truth))]
			} else {
				e = uEdge{ids[core.IntN(rng, len(ids))], ids[core.IntN(rng, len(ids))], "port", 0}
			}
			c = Change{Seq: seq, Kind: ChangeEdgeRemoved, From: e.from, To: e.to, Label: e.label}
			truth = slices.DeleteFunc(truth, func(x uEdge) bool { return x.from == e.from && x.to == e.to && x.label == e.label })
		}
		if ok, err := o.Apply(c); err != nil || !ok {
			t.Fatalf("apply %+v: %v %v", c, ok, err)
		}
	}
	// Nodes added through the overlay but never given a type are type 0
	// in both worlds; ids lists every node.
	rebuilt := build(slices.Clone(ids), truth, allLabels)

	if o.NumNodes() != rebuilt.NumNodes() {
		t.Fatalf("nodes %d vs %d", o.NumNodes(), rebuilt.NumNodes())
	}
	type tuple struct {
		other core.UUID
		label string
		conf  float32
	}
	norm := func(ts []tuple) []tuple {
		slices.SortFunc(ts, func(a, b tuple) int {
			return cmp.Or(slices.Compare(a.other[:], b.other[:]), cmp.Compare(a.label, b.label), cmp.Compare(a.conf, b.conf))
		})
		return ts
	}
	olabels := o.Labels()
	for _, u := range ids {
		ov, ok := o.Index(u)
		rv, ok2 := rebuilt.Index(u)
		if !ok || !ok2 {
			t.Fatalf("id %s missing", u)
		}
		oe, _ := o.OutEdges(ov)
		var a, b []tuple
		for _, e := range oe {
			id, _ := o.NodeID(e.Dst)
			a = append(a, tuple{id, olabels[e.Label], e.Conf})
		}
		lo, hi, _ := rebuilt.OutRange(rv)
		for i := lo; i < hi; i++ {
			e, _ := rebuilt.EdgeAt(i)
			id, _ := rebuilt.NodeID(e.Dst)
			b = append(b, tuple{id, allLabels[e.Label], e.Conf})
		}
		if fmt.Sprint(norm(a)) != fmt.Sprint(norm(b)) {
			t.Fatalf("out-edges of %s: overlay %v, rebuild %v", u, a, b)
		}
		oi, _ := o.InEdges(ov)
		rin, _ := rebuilt.InNeighbors(rv)
		if len(oi) != len(rin) {
			t.Fatalf("in-degree of %s: %d vs %d", u, len(oi), len(rin))
		}
		// BFS over snapshot+overlay equals BFS over the rebuild, by id.
		dir := Direction(1 + core.IntN(rng, 3))
		depth := core.IntN(rng, 4)
		names := []string{"dns_record"}
		ol, err := o.BFS(ov, BFSOptions{MaxDepth: depth, Direction: dir, Labels: o.LabelSetOf(names...)})
		if err != nil {
			t.Fatal(err)
		}
		rr, err := BFS(rebuilt, rv, BFSOptions{MaxDepth: depth, Direction: dir, Labels: rebuilt.LabelSetOf(names...)})
		if err != nil {
			t.Fatal(err)
		}
		if len(ol) != len(rr.Levels) {
			t.Fatalf("BFS depth %d vs %d", len(ol), len(rr.Levels))
		}
		for d := range ol {
			var x, y []core.UUID
			for _, v := range ol[d] {
				id, _ := o.NodeID(v)
				x = append(x, id)
			}
			for _, v := range rr.Levels[d] {
				id, _ := rebuilt.NodeID(v)
				y = append(y, id)
			}
			sortIDs := func(s []core.UUID) {
				slices.SortFunc(s, func(a, b core.UUID) int { return slices.Compare(a[:], b[:]) })
			}
			sortIDs(x)
			sortIDs(y)
			if !slices.Equal(x, y) {
				t.Fatalf("BFS level %d: %v vs %v", d, x, y)
			}
		}
	}
	m, err := o.Materialize(rebuilt.Meta())
	if err != nil {
		t.Fatal(err)
	}
	da, _ := m.Digest()
	db, _ := rebuilt.Digest()
	if da != db {
		t.Fatal("materialized overlay differs from rebuild")
	}
	// Rebasing onto the rebuilt snapshot (watermark 10 < seq) and onto the
	// materialized one at the overlay's watermark both give the same view.
	mw, err := o.Materialize(Meta{WatermarkSeq: o.Watermark()})
	if err != nil {
		t.Fatal(err)
	}
	if rb, err := o.Rebase(mw); err != nil || rb.Size() != 0 {
		t.Fatalf("rebase onto current snapshot left %d events: %v", rb.Size(), err)
	}
}
