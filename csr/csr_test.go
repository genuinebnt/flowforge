package csr

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

func specCase(t *testing.T) *confFile {
	t.Helper()
	for _, cf := range loadConformance(t) {
		if cf.Data.Name == "spec_21_3" {
			c := cf.Data
			return &c
		}
	}
	t.Fatal("spec_21_3.json missing")
	return nil
}

func namedCase(t *testing.T, name string) (*confFile, *Graph) {
	t.Helper()
	for _, cf := range loadConformance(t) {
		if cf.Data.Name == name {
			c := cf.Data
			return &c, c.graph(t)
		}
	}
	t.Fatalf("conformance case %s missing", name)
	return nil, nil
}

func TestCSR001_SpecVector(t *testing.T) {
	c := specCase(t)
	g := c.graph(t)
	if g.NumNodes() != 5 || g.NumEdges() != 6 {
		t.Fatalf("V=%d E=%d", g.NumNodes(), g.NumEdges())
	}
	checkStructure(t, c, g)
	for i := range 5 {
		if id, _ := g.NodeID(uint32(i)); id != nodeUUID(i) {
			t.Errorf("node %d id %s", i, id)
		}
		if v, ok := g.Index(nodeUUID(i)); !ok || v != uint32(i) {
			t.Errorf("Index(%d) = %d, %v", i, v, ok)
		}
	}
	// The same vector survives an encode/decode round trip unchanged.
	b, err := g.Encode()
	if err != nil {
		t.Fatal(err)
	}
	d, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	checkStructure(t, c, d)
}

func TestCSR002_EdgeCases(t *testing.T) {
	type tc struct {
		name   string
		nodes  int
		edges  []Edge
		labels []string
		check  func(t *testing.T, g *Graph)
	}
	cases := []tc{
		{name: "empty", check: func(t *testing.T, g *Graph) {
			if _, err := BFS(g, 0, BFSOptions{Direction: DirOut}); !errors.Is(err, ErrNodeRange) {
				t.Errorf("BFS on empty graph: %v", err)
			}
			if r, _ := PageRank(g, PageRankOptions{}); len(r.Scores) != 0 || !r.Converged {
				t.Errorf("PageRank on empty graph: %+v", r)
			}
			if StronglyConnectedComponents(g).Count() != 0 || WeaklyConnectedComponents(g).Count() != 0 {
				t.Error("components of empty graph")
			}
		}},
		{name: "single_node", nodes: 1, check: func(t *testing.T, g *Graph) {
			r, _ := BFS(g, 0, BFSOptions{MaxDepth: -1, Direction: DirBoth, WithEdges: true})
			if len(r.Levels) != 1 || len(r.Edges) != 0 {
				t.Errorf("BFS %+v", r)
			}
			if pr, _ := PageRank(g, PageRankOptions{}); math.Abs(pr.Scores[0]-1) > 1e-12 {
				t.Errorf("PageRank %v", pr.Scores)
			}
		}},
		{name: "self_loop", nodes: 1, labels: []string{"l"}, edges: []Edge{{0, 0, 0, 0.5}}, check: func(t *testing.T, g *Graph) {
			r, _ := BFS(g, 0, BFSOptions{MaxDepth: -1, Direction: DirBoth, WithEdges: true})
			if len(r.Levels) != 1 || !slices.Equal(r.Edges, []uint32{0}) {
				t.Errorf("BFS %+v", r)
			}
			if c := CoreNumbers(g); c[0] != 0 {
				t.Errorf("self loop counted in core: %v", c)
			}
			p, ok, _ := ShortestPath(g, 0, 0, PathOptions{})
			if !ok || p.Confidence != 1 || len(p.Edges) != 0 {
				t.Errorf("trivial path %+v", p)
			}
		}},
		{name: "duplicate_edges", nodes: 2, labels: []string{"a", "b"},
			edges: []Edge{{0, 1, 1, 0.3}, {0, 1, 0, 0.9}, {0, 1, 0, 0.2}, {1, 0, 0, 1}},
			check: func(t *testing.T, g *Graph) {
				if got := g.EdgeLabels(); !slices.Equal(got, []uint8{0, 0, 1, 0}) {
					t.Errorf("labels %v", got)
				}
				if got := g.EdgeConfs(); !slices.Equal(got, []float32{0.2, 0.9, 0.3, 1}) {
					t.Errorf("confs %v", got)
				}
				p, ok, _ := ShortestPath(g, 0, 1, PathOptions{})
				if !ok || !slices.Equal(p.Edges, []uint32{1}) {
					t.Errorf("best parallel edge not chosen: %+v", p)
				}
				if b := FindCuts(g).Bridges; len(b) != 1 {
					t.Errorf("bridges %v", b)
				}
			}},
		{name: "isolated_nodes", nodes: 5, labels: []string{"l"}, edges: []Edge{{1, 3, 0, 1}}, check: func(t *testing.T, g *Graph) {
			if n := WeaklyConnectedComponents(g).Count(); n != 4 {
				t.Errorf("WCC count %d", n)
			}
			if n := StronglyConnectedComponents(g).Count(); n != 5 {
				t.Errorf("SCC count %d", n)
			}
			r, _ := BFS(g, 0, BFSOptions{MaxDepth: -1, Direction: DirBoth})
			if r.Reached() != 1 {
				t.Errorf("isolated node reached %d", r.Reached())
			}
			if _, ok, _ := ShortestPath(g, 0, 4, PathOptions{}); ok {
				t.Error("path between isolated nodes")
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, err := Build(Meta{}, nodeUUIDs(c.nodes), nil, c.edges, c.labels)
			if err != nil {
				t.Fatal(err)
			}
			b, err := g.Encode()
			if err != nil {
				t.Fatal(err)
			}
			d, err := Decode(b)
			if err != nil {
				t.Fatal(err)
			}
			for _, h := range []*Graph{g, d} {
				exerciseAll(t, h)
				c.check(t, h)
			}
		})
	}
}

// exerciseAll runs every algorithm once and checks the generic contracts
// (no error on valid input, scores sum to one, partitions cover V).
func exerciseAll(t testing.TB, g *Graph) {
	t.Helper()
	nv := g.NumNodes()
	pr, err := PageRank(g, PageRankOptions{MaxIterations: 20})
	if err != nil {
		t.Fatal(err)
	}
	if nv > 0 {
		s := 0.0
		for _, x := range pr.Scores {
			s += x
		}
		if math.Abs(s-1) > 1e-9 {
			t.Errorf("PageRank sums to %v", s)
		}
	}
	for _, p := range []Partition{StronglyConnectedComponents(g), WeaklyConnectedComponents(g)} {
		if len(p.Members) != nv || len(p.Of) != nv {
			t.Errorf("partition does not cover V: %+v", p)
		}
	}
	if len(CoreNumbers(g)) != nv {
		t.Error("core numbers length")
	}
	FindCuts(g)
	TriangleCount(g)
	if nv > 0 {
		last := uint32(nv - 1)
		for _, d := range []Direction{DirOut, DirIn, DirBoth} {
			if _, err := BFS(g, 0, BFSOptions{MaxDepth: -1, Direction: d}); err != nil {
				t.Error(err)
			}
		}
		if _, _, err := ShortestPath(g, 0, last, PathOptions{}); err != nil {
			t.Error(err)
		}
		if _, err := CommonNeighbors(g, 0, last); err != nil {
			t.Error(err)
		}
		if _, err := BlastRadius(g, 0, MaxPointDepth); err != nil {
			t.Error(err)
		}
	}
}

func specMeta() Meta {
	return Meta{
		TenantID:     core.MustParseUUID("01890a5d-ac96-774b-bcce-b302099a8057"),
		ProgramID:    core.MustParseUUID("01890a5d-ac96-774b-bcce-b302099a8058"),
		WatermarkSeq: 4242,
		CreatedAt:    time.Date(2026, 11, 12, 14, 2, 12, 311_000_000, time.UTC),
		Flags:        FlagStaleExcluded,
	}
}

func assertSameGraph(t testing.TB, a, b *Graph) {
	t.Helper()
	if a.Meta().TenantID != b.Meta().TenantID || a.Meta().ProgramID != b.Meta().ProgramID ||
		a.Meta().WatermarkSeq != b.Meta().WatermarkSeq || !a.Meta().CreatedAt.Equal(b.Meta().CreatedAt) ||
		a.Meta().Flags != b.Meta().Flags {
		t.Fatalf("meta %+v != %+v", a.Meta(), b.Meta())
	}
	ok := slices.Equal(a.Offsets(), b.Offsets()) && slices.Equal(a.Targets(), b.Targets()) &&
		slices.Equal(a.EdgeLabels(), b.EdgeLabels()) && slices.Equal(a.EdgeConfs(), b.EdgeConfs()) &&
		slices.Equal(a.NodeTypes(), b.NodeTypes()) && slices.Equal(a.NodeIDs(), b.NodeIDs()) &&
		slices.Equal(a.Labels(), b.Labels()) && slices.Equal(a.InOffsets(), b.InOffsets()) &&
		slices.Equal(a.InSources(), b.InSources()) && slices.Equal(a.InEdgeIndex(), b.InEdgeIndex())
	if !ok {
		t.Fatal("graph arrays differ")
	}
}

func TestCSR003_FileRoundTrip(t *testing.T) {
	c := specCase(t)
	types := []uint8{1, 2, 3, 4, 5}
	for _, flags := range []uint16{FlagStaleExcluded, FlagStaleExcluded | FlagConfU8} {
		meta := specMeta()
		meta.Flags = flags
		g, err := Build(meta, nodeUUIDs(c.Nodes), types, c.edges(t), c.Labels)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "4242.ffcs")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		n, err := g.WriteTo(f)
		if err != nil || n != g.EncodedSize() {
			t.Fatalf("WriteTo: %d %v (size %d)", n, err, g.EncodedSize())
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		opened, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		assertSameGraph(t, g, opened)
		d1, _ := g.Digest()
		d2, _ := opened.Digest()
		raw, _ := os.ReadFile(path)
		if d1 != d2 || !bytes.Equal(raw[len(raw)-32:], d1[:]) {
			t.Fatal("digest mismatch")
		}
		if len(raw)%sectionAlign != 32 {
			t.Errorf("footer not 64-byte aligned: size %d", len(raw))
		}
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
		if err := opened.Close(); err != nil {
			t.Fatal("second Close:", err)
		}

		// Corrupting any single byte makes open fail with a checksum error.
		for i := range raw {
			bad := slices.Clone(raw)
			bad[i] ^= 0x5a
			if _, err := Decode(bad); !errors.Is(err, ErrChecksum) {
				t.Fatalf("flags %#x: byte %d corrupted: err = %v", flags, i, err)
			}
		}
		// Without the sha256 pass the per-section crc32c still catches
		// corruption inside a section.
		secStart := int(alignUp(headerSize(numSections)))
		for i := secStart; i < len(raw)-32; i++ {
			bad := slices.Clone(raw)
			bad[i] ^= 0x01
			_, err := DecodeWith(bad, OpenOptions{SkipDigest: true})
			if err == nil && !isPadding(t, raw, i) {
				t.Fatalf("byte %d corrupted accepted without digest", i)
			}
			if err != nil && !errors.Is(err, ErrChecksum) {
				t.Fatalf("byte %d: %v", i, err)
			}
		}
		badPath := filepath.Join(t.TempDir(), "bad.ffcs")
		bad := slices.Clone(raw)
		bad[secStart] ^= 1
		if err := os.WriteFile(badPath, bad, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(badPath); !errors.Is(err, ErrChecksum) {
			t.Fatalf("Open corrupt file: %v", err)
		}
	}
	if _, err := Open(filepath.Join(t.TempDir(), "missing.ffcs")); err == nil {
		t.Fatal("Open of a missing file succeeded")
	}
	empty := filepath.Join(t.TempDir(), "empty.ffcs")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(empty); !errors.Is(err, ErrFormat) {
		t.Fatalf("Open empty file: %v", err)
	}
}

// isPadding reports whether byte i of an encoded file lies in the zero
// padding between sections (not covered by any crc32c).
func isPadding(t *testing.T, b []byte, i int) bool {
	t.Helper()
	n := int(binary.LittleEndian.Uint32(b[headerFixed:]))
	for k := range n {
		p := b[headerFixed+4+k*tableEntrySize:]
		off, l := int(binary.LittleEndian.Uint64(p[2:])), int(binary.LittleEndian.Uint64(p[10:]))
		if i >= off && i < off+l {
			return false
		}
	}
	return true
}

func TestCSR004_BFS(t *testing.T) {
	forEachCase(t, checkBFS)
	_, g := namedCase(t, "spec_21_3")
	r, _ := BFS(g, 0, BFSOptions{MaxDepth: 1, Direction: DirOut, WithEdges: true})
	if !slices.Equal(r.Edges, []uint32{0, 1}) {
		t.Errorf("edges %v", r.Edges)
	}
	r, _ = BFS(g, 2, BFSOptions{MaxDepth: 1, Direction: DirBoth, WithEdges: true})
	if !slices.Equal(r.Edges, []uint32{1, 3}) {
		t.Errorf("edges %v", r.Edges)
	}
	if _, err := BFS(g, 0, BFSOptions{}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("unspecified direction accepted: %v", err)
	}
	if _, err := BFS(g, 5, BFSOptions{Direction: DirOut}); !errors.Is(err, ErrNodeRange) {
		t.Errorf("out-of-range start accepted: %v", err)
	}
}

func pathGraph(t testing.TB, n int) *Graph {
	t.Helper()
	edges := make([]Edge, n-1)
	for i := range edges {
		edges[i] = Edge{Src: uint32(i), Dst: uint32(i + 1), Conf: 1}
	}
	g, err := Build(Meta{}, nodeUUIDs(n), nil, edges, []string{"next"})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestCSR005_SCC(t *testing.T) {
	forEachCase(t, checkSCC)
	t.Run("path_200000", func(t *testing.T) {
		const n = 200_000
		g := pathGraph(t, n)
		p := StronglyConnectedComponents(g)
		if p.Count() != n {
			t.Fatalf("%d components, want %d", p.Count(), n)
		}
		// Reverse topological order: the sink comes first.
		if p.Group(0)[0] != n-1 || p.Group(n - 1)[0] != 0 {
			t.Fatalf("order: first %v last %v", p.Group(0), p.Group(n-1))
		}
		checkReverseTopological(t, g, p)
		// The other DFS-based algorithms are iterative too.
		if c := FindCuts(g); len(c.Points) != n-2 || len(c.Bridges) != n-1 {
			t.Fatalf("cuts on path: %d points, %d bridges", len(c.Points), len(c.Bridges))
		}
		if WeaklyConnectedComponents(g).Count() != 1 {
			t.Fatal("path not weakly connected")
		}
	})
}

func TestCSR006_PageRank(t *testing.T) {
	for _, name := range []string{"star_9", "cycle_8", "two_cliques_bridge"} {
		c, g := namedCase(t, name)
		t.Run(name, func(t *testing.T) {
			checkPageRank(t, c, g)
			one, _ := PageRank(g, PageRankOptions{Workers: 1})
			many, _ := PageRank(g, PageRankOptions{Workers: 7})
			if !slices.Equal(one.Scores, many.Scores) || one.Iterations != many.Iterations {
				t.Error("PageRank depends on the worker count")
			}
			s := one.Scores
			switch name {
			case "cycle_8":
				for _, x := range s {
					if math.Abs(x-1.0/8) > 1e-12 {
						t.Errorf("cycle scores not equal: %v", s)
					}
				}
			case "star_9":
				for v := 1; v < len(s); v++ {
					if !(s[0] > s[v]) {
						t.Errorf("centre %v not above leaf %d %v", s[0], v, s[v])
					}
				}
			}
		})
	}
	// Large enough for several parallel chunks.
	g := randomGraph(t, core.NewSplitMix64(6), 3*pageRankChunk+17, 40000, 3)
	one, _ := PageRank(g, PageRankOptions{Workers: 1})
	many, _ := PageRank(g, PageRankOptions{Workers: 8})
	if !slices.Equal(one.Scores, many.Scores) {
		t.Error("multi-chunk PageRank depends on the worker count")
	}
	if _, err := PageRank(g, PageRankOptions{Damping: 1.5}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("damping 1.5 accepted: %v", err)
	}
}

func TestCSR007_Dijkstra(t *testing.T) {
	forEachCase(t, checkPaths)
	// Confidence 0 edges are impassable; label filters restrict paths.
	g, err := Build(Meta{}, nodeUUIDs(3), nil, []Edge{{0, 1, 0, 0}, {0, 2, 1, 0.5}, {2, 1, 1, 0.5}}, []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	p, ok, _ := ShortestPath(g, 0, 1, PathOptions{})
	if !ok || !slices.Equal(p.Nodes, []uint32{0, 2, 1}) || math.Abs(p.Confidence-0.25) > 1e-12 {
		t.Errorf("path %+v %v", p, ok)
	}
	if _, ok, _ := ShortestPath(g, 0, 1, PathOptions{Labels: OnlyLabels(0)}); ok {
		t.Error("path over a zero-confidence edge")
	}
	if _, _, err := ShortestPath(g, 0, 9, PathOptions{}); !errors.Is(err, ErrNodeRange) {
		t.Errorf("out-of-range target: %v", err)
	}
}

func TestCSR008_OverlayEqualsRebuild(t *testing.T) {
	// A scripted scenario first, then randomized cases.
	ids := nodeUUIDs(4)
	g, err := Build(specMeta(), ids[:3], nil, []Edge{{0, 1, 0, 0.5}, {1, 2, 0, 0.5}}, []string{"dns_record"})
	if err != nil {
		t.Fatal(err)
	}
	o := NewOverlay(g)
	w := g.Meta().WatermarkSeq
	steps := []Change{
		{Seq: w, Kind: ChangeEdgeRemoved, From: ids[0], To: ids[1], Label: "dns_record"}, // already reflected
		{Seq: w + 1, Kind: ChangeNodeAdded, Node: ids[3], NodeType: 9},
		{Seq: w + 2, Kind: ChangeEdgeAdded, From: ids[2], To: ids[3], Label: "port", Conf: 0.9},
		{Seq: w + 3, Kind: ChangeEdgeRemoved, From: ids[0], To: ids[1], Label: "dns_record"},
	}
	for i, c := range steps {
		applied, err := o.Apply(c)
		if err != nil || applied != (i > 0) {
			t.Fatalf("step %d: %v %v", i, applied, err)
		}
	}
	if _, err := o.Apply(Change{Seq: w + 2, Kind: ChangeNodeAdded}); !errors.Is(err, ErrSeqOrder) {
		t.Errorf("out of order: %v", err)
	}
	if _, err := o.Apply(Change{Seq: w + 9, Kind: ChangeEdgeAdded, From: ids[0], To: nodeUUID(99), Label: "x", Conf: 1}); !errors.Is(err, ErrUnknownNode) {
		t.Errorf("unknown node: %v", err)
	}
	lv, err := o.BFS(1, BFSOptions{MaxDepth: -1, Direction: DirOut})
	if err != nil || len(lv) != 3 || lv[2][0] != 3 {
		t.Errorf("overlay BFS %v %v", lv, err)
	}
	rebuilt, err := Build(Meta{}, ids, []uint8{0, 0, 0, 9}, []Edge{{1, 2, 0, 0.5}, {2, 3, 1, 0.9}}, []string{"dns_record", "port"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := o.Materialize(Meta{})
	if err != nil {
		t.Fatal(err)
	}
	assertSameGraph(t, rebuilt, m)
	if o.Size() != 3 || o.Watermark() != w+3 {
		t.Errorf("size %d watermark %d", o.Size(), o.Watermark())
	}

	rng := testkit.Rng(t)
	for range max(testkit.Cases()/5, 20) {
		overlayEqualsRebuild(t, rng)
	}
}

func TestCSR009_DeterministicBuild(t *testing.T) {
	rng := testkit.Rng(t)
	n := 60
	ids := make([]core.UUID, n)
	for i := range ids {
		binary.LittleEndian.PutUint64(ids[i][:8], rng.Uint64())
		binary.LittleEndian.PutUint64(ids[i][8:], rng.Uint64())
	}
	types := make([]uint8, n)
	for i := range types {
		types[i] = uint8(rng.Uint64())
	}
	labels := []string{"dns_record", "port", "certificate"}
	var edges []Edge
	for range 300 {
		edges = append(edges, Edge{Src: uint32(core.IntN(rng, n)), Dst: uint32(core.IntN(rng, n)), Label: uint8(core.IntN(rng, 3)), Conf: float32(core.IntN(rng, 5)) / 4})
	}
	edges = append(edges, edges[:40]...) // exact duplicates too
	ref, err := Build(specMeta(), ids, types, edges, labels)
	if err != nil {
		t.Fatal(err)
	}
	want, err := ref.Encode()
	if err != nil {
		t.Fatal(err)
	}
	for range 25 {
		perm := make([]int, n)
		for i := range perm {
			perm[i] = i
		}
		core.Shuffle(rng, n, func(i, j int) { perm[i], perm[j] = perm[j], perm[i] })
		inv := make([]uint32, n)
		ids2, types2 := make([]core.UUID, n), make([]uint8, n)
		for newPos, old := range perm {
			inv[old] = uint32(newPos)
			ids2[newPos], types2[newPos] = ids[old], types[old]
		}
		edges2 := make([]Edge, len(edges))
		for i, e := range edges {
			edges2[i] = Edge{Src: inv[e.Src], Dst: inv[e.Dst], Label: e.Label, Conf: e.Conf}
		}
		core.Shuffle(rng, len(edges2), func(i, j int) { edges2[i], edges2[j] = edges2[j], edges2[i] })
		g, err := Build(specMeta(), ids2, types2, edges2, labels)
		if err != nil {
			t.Fatal(err)
		}
		got, err := g.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatal("same content in a different order produced different snapshot bytes")
		}
	}
}
