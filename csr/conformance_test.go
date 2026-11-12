package csr

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/genuinebnt/flowforge/testkit"
)

// regen rewrites the generated golden files (every conformance/csr file
// except spec_21_3.json, which is written by hand from the spec):
//
//	go test ./csr -run TestCSRGoldenRegenerate -csr.regen
//
// Expected values come from the naive references in reference_test.go,
// never from the implementation under test.
var regen = flag.Bool("csr.regen", false, "regenerate generated conformance/csr golden files")

const suite = "csr"

type confEdge struct {
	Src   uint32  `json:"src"`
	Dst   uint32  `json:"dst"`
	Label string  `json:"label"`
	Conf  float32 `json:"conf"`
}

type confBFS struct {
	Start     uint32     `json:"start"`
	Direction string     `json:"direction"`
	MaxDepth  int        `json:"max_depth"`
	Labels    []string   `json:"labels,omitempty"`
	Levels    [][]uint32 `json:"levels"`
	Dist      []int32    `json:"dist"`
}

type confPath struct {
	From       uint32   `json:"from"`
	To         uint32   `json:"to"`
	MaxCost    float64  `json:"max_cost,omitempty"`
	Reachable  bool     `json:"reachable"`
	Nodes      []uint32 `json:"nodes,omitempty"`
	Edges      []uint32 `json:"edges,omitempty"`
	Confidence float64  `json:"confidence,omitempty"`
}

type confExpect struct {
	Degree             []uint64     `json:"degree,omitempty"`
	Offsets            []uint64     `json:"offsets,omitempty"`
	Targets            []uint32     `json:"targets,omitempty"`
	InOffsets          []uint64     `json:"in_offsets,omitempty"`
	InSources          []uint32     `json:"in_sources,omitempty"`
	InEdge             []uint32     `json:"in_edge,omitempty"`
	BFS                []confBFS    `json:"bfs,omitempty"`
	SCC                [][]uint32   `json:"scc,omitempty"`
	WCCCount           *int         `json:"wcc_count,omitempty"`
	WCC                [][]uint32   `json:"wcc,omitempty"`
	CoreNumbers        []uint32     `json:"core_numbers,omitempty"`
	ArticulationPoints *[]uint32    `json:"articulation_points,omitempty"`
	Bridges            *[][2]uint32 `json:"bridges,omitempty"`
	Triangles          *uint64      `json:"triangles,omitempty"`
	PageRank           []float64    `json:"pagerank,omitempty"`
	Paths              []confPath   `json:"paths,omitempty"`
}

type confFile struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Nodes       int        `json:"nodes"`
	Labels      []string   `json:"labels"`
	Edges       []confEdge `json:"edges"`
	Expect      confExpect `json:"expect"`
}

func (c *confFile) edges(t testing.TB) []Edge {
	t.Helper()
	out := make([]Edge, len(c.Edges))
	for i, e := range c.Edges {
		l := slices.Index(c.Labels, e.Label)
		if l < 0 {
			t.Fatalf("%s: edge %d label %q not in labels", c.Name, i, e.Label)
		}
		out[i] = Edge{Src: e.Src, Dst: e.Dst, Label: uint8(l), Conf: e.Conf}
	}
	return out
}

func (c *confFile) graph(t testing.TB) *Graph {
	t.Helper()
	g, err := Build(Meta{}, nodeUUIDs(c.Nodes), nil, c.edges(t), c.Labels)
	if err != nil {
		t.Fatalf("%s: build: %v", c.Name, err)
	}
	return g
}

func loadConformance(t *testing.T) []testkit.CaseFile[confFile] {
	t.Helper()
	return testkit.LoadCases[confFile](t, suite)
}

// forEachCase runs fn as a subtest for every conformance file.
func forEachCase(t *testing.T, fn func(t *testing.T, c *confFile, g *Graph)) {
	for _, cf := range loadConformance(t) {
		t.Run(cf.Data.Name, func(t *testing.T) {
			c := cf.Data
			fn(t, &c, c.graph(t))
		})
	}
}

func labelSetFor(g *Graph, names []string) LabelSet {
	if names == nil {
		return AnyLabel()
	}
	return g.LabelSetOf(names...)
}

func checkBFS(t *testing.T, c *confFile, g *Graph) {
	t.Helper()
	for _, b := range c.Expect.BFS {
		dir, err := ParseDirection(b.Direction)
		if err != nil {
			t.Fatal(err)
		}
		r, err := BFS(g, b.Start, BFSOptions{MaxDepth: b.MaxDepth, Direction: dir, Labels: labelSetFor(g, b.Labels)})
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(r.Levels) != fmt.Sprint(b.Levels) {
			t.Errorf("BFS %+v: levels %v, want %v", b, r.Levels, b.Levels)
		}
		if got := r.Distances(g.NumNodes()); !slices.Equal(got, b.Dist) {
			t.Errorf("BFS %+v: dist %v, want %v", b, got, b.Dist)
		}
	}
}

// checkReverseTopological asserts that every edge between different SCCs
// points from a later group to an earlier one.
func checkReverseTopological(t *testing.T, g *Graph, p Partition) {
	t.Helper()
	for _, e := range g.Edges() {
		if a, b := p.Of[e.Src], p.Of[e.Dst]; a != b && b > a {
			t.Fatalf("edge %d->%d goes from group %d to later group %d", e.Src, e.Dst, a, b)
		}
	}
}

func checkSCC(t *testing.T, c *confFile, g *Graph) {
	t.Helper()
	if c.Expect.SCC == nil {
		return
	}
	p := StronglyConnectedComponents(g)
	if got := canonicalGroups(p.Groups()); fmt.Sprint(got) != fmt.Sprint(c.Expect.SCC) {
		t.Errorf("SCC %v, want %v", got, c.Expect.SCC)
	}
	checkReverseTopological(t, g, p)
}

func checkPageRank(t *testing.T, c *confFile, g *Graph) {
	t.Helper()
	want := c.Expect.PageRank
	if want == nil {
		return
	}
	r, err := PageRank(g, PageRankOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Converged {
		t.Errorf("PageRank did not converge in %d iterations", r.Iterations)
	}
	comparePageRank(t, r.Scores, want)
}

// comparePageRank applies the spec 21.5 rule: absolute tolerance 1e-5 and
// the same rank order wherever the reference separates two scores.
func comparePageRank(t testing.TB, got, want []float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("PageRank has %d scores, want %d", len(got), len(want))
	}
	sum := 0.0
	for v := range got {
		sum += got[v]
		if math.Abs(got[v]-want[v]) > 1e-5 {
			t.Errorf("PageRank[%d] = %.9f, want %.9f", v, got[v], want[v])
		}
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Errorf("PageRank scores sum to %.12f", sum)
	}
	for a := range want {
		for b := range want {
			if want[a] > want[b]+2e-5 && !(got[a] > got[b]) {
				t.Errorf("rank order: node %d (%.9f) should outrank %d (%.9f)", a, got[a], b, got[b])
			}
		}
	}
}

func checkPaths(t *testing.T, c *confFile, g *Graph) {
	t.Helper()
	for _, pc := range c.Expect.Paths {
		p, ok, err := ShortestPath(g, pc.From, pc.To, PathOptions{MaxCost: pc.MaxCost})
		if err != nil {
			t.Fatal(err)
		}
		if ok != pc.Reachable {
			t.Errorf("path %d->%d reachable=%v, want %v", pc.From, pc.To, ok, pc.Reachable)
			continue
		}
		if !ok {
			continue
		}
		if math.Abs(p.Confidence-pc.Confidence) > 1e-6 {
			t.Errorf("path %d->%d confidence %.9f, want %.9f", pc.From, pc.To, p.Confidence, pc.Confidence)
		}
		if pc.Nodes != nil && !slices.Equal(p.Nodes, pc.Nodes) {
			t.Errorf("path %d->%d nodes %v, want %v", pc.From, pc.To, p.Nodes, pc.Nodes)
		}
		if pc.Nodes != nil && !slices.Equal(p.Edges, append([]uint32{}, pc.Edges...)) {
			t.Errorf("path %d->%d edges %v, want %v", pc.From, pc.To, p.Edges, pc.Edges)
		}
		checkPathValid(t, g, p, pc.From, pc.To)
	}
}

// checkPathValid asserts that p is a real path whose confidence is the
// product of its edges' confidences.
func checkPathValid(t testing.TB, g *Graph, p Path, from, to uint32) {
	t.Helper()
	if len(p.Nodes) != len(p.Edges)+1 || p.Nodes[0] != from || p.Nodes[len(p.Nodes)-1] != to {
		t.Fatalf("malformed path %+v", p)
	}
	prod := 1.0
	for i, ei := range p.Edges {
		e, err := g.EdgeAt(int(ei))
		if err != nil || e.Src != p.Nodes[i] || e.Dst != p.Nodes[i+1] {
			t.Fatalf("path edge %d (%+v) does not join %d->%d", ei, e, p.Nodes[i], p.Nodes[i+1])
		}
		prod *= float64(e.Conf)
	}
	if math.Abs(prod-p.Confidence) > 1e-9 {
		t.Fatalf("path confidence %.12f, edge product %.12f", p.Confidence, prod)
	}
}

func checkStructure(t *testing.T, c *confFile, g *Graph) {
	t.Helper()
	x := c.Expect
	if x.Degree != nil {
		off := g.Offsets()
		var deg []uint64
		for v := range g.NumNodes() {
			deg = append(deg, off[v+1]-off[v])
		}
		if !slices.Equal(deg, x.Degree) {
			t.Errorf("degree %v, want %v", deg, x.Degree)
		}
	}
	eq := func(name string, got, want any) {
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	if x.Offsets != nil {
		eq("offsets", g.Offsets(), x.Offsets)
	}
	if x.Targets != nil {
		eq("targets", g.Targets(), x.Targets)
	}
	if x.InOffsets != nil {
		eq("in_offsets", g.InOffsets(), x.InOffsets)
	}
	if x.InSources != nil {
		eq("in_sources", g.InSources(), x.InSources)
	}
	if x.InEdge != nil {
		eq("in_edge", g.InEdgeIndex(), x.InEdge)
	}
}

func checkOthers(t *testing.T, c *confFile, g *Graph) {
	t.Helper()
	x := c.Expect
	wcc := WeaklyConnectedComponents(g)
	if x.WCCCount != nil && wcc.Count() != *x.WCCCount {
		t.Errorf("WCC count %d, want %d", wcc.Count(), *x.WCCCount)
	}
	if x.WCC != nil && fmt.Sprint(wcc.Groups()) != fmt.Sprint(x.WCC) {
		t.Errorf("WCC %v, want %v", wcc.Groups(), x.WCC)
	}
	if x.CoreNumbers != nil {
		if got := CoreNumbers(g); !slices.Equal(got, x.CoreNumbers) {
			t.Errorf("core numbers %v, want %v", got, x.CoreNumbers)
		}
	}
	cuts := FindCuts(g)
	if x.ArticulationPoints != nil && fmt.Sprint(cuts.Points) != fmt.Sprint(*x.ArticulationPoints) {
		t.Errorf("articulation points %v, want %v", cuts.Points, *x.ArticulationPoints)
	}
	if x.Bridges != nil {
		var got [][2]uint32
		for _, b := range cuts.Bridges {
			got = append(got, [2]uint32{b.U, b.W})
		}
		if fmt.Sprint(got) != fmt.Sprint(*x.Bridges) {
			t.Errorf("bridges %v, want %v", got, *x.Bridges)
		}
	}
	if x.Triangles != nil && TriangleCount(g) != *x.Triangles {
		t.Errorf("triangles %d, want %d", TriangleCount(g), *x.Triangles)
	}
}

// TestCSRConformance checks every expectation of every conformance file.
func TestCSRConformance(t *testing.T) {
	forEachCase(t, func(t *testing.T, c *confFile, g *Graph) {
		checkStructure(t, c, g)
		checkBFS(t, c, g)
		checkSCC(t, c, g)
		checkPageRank(t, c, g)
		checkPaths(t, c, g)
		checkOthers(t, c, g)
	})
}

// goldenGraphs are the generated graphs of spec 21.5.
func goldenGraphs() []confFile {
	var out []confFile
	conf := func(i int) float32 { return float32(0.5 + 0.05*float64(i%10)) }

	path := confFile{Name: "path_10", Description: "Directed path 0->1->...->9.", Nodes: 10, Labels: []string{"link"}}
	for i := range 9 {
		path.Edges = append(path.Edges, confEdge{uint32(i), uint32(i + 1), "link", conf(i)})
	}
	out = append(out, path)

	star := confFile{Name: "star_9", Description: "Star: leaves 1..8 point to centre 0 (centre is dangling).", Nodes: 9, Labels: []string{"link"}}
	for i := 1; i < 9; i++ {
		star.Edges = append(star.Edges, confEdge{uint32(i), 0, "link", conf(i)})
	}
	out = append(out, star)

	cycle := confFile{Name: "cycle_8", Description: "Directed cycle 0->1->...->7->0.", Nodes: 8, Labels: []string{"link"}}
	for i := range 8 {
		cycle.Edges = append(cycle.Edges, confEdge{uint32(i), uint32((i + 1) % 8), "link", conf(i)})
	}
	out = append(out, cycle)

	grid := confFile{Name: "grid_4x4", Description: "4x4 grid, node r*4+c; edges right (label right) and down (label down).", Nodes: 16, Labels: []string{"right", "down"}}
	for r := range 4 {
		for c := range 4 {
			v := uint32(r*4 + c)
			if c < 3 {
				grid.Edges = append(grid.Edges, confEdge{v, v + 1, "right", conf(int(v))})
			}
			if r < 3 {
				grid.Edges = append(grid.Edges, confEdge{v, v + 4, "down", conf(int(v) + 3)})
			}
		}
	}
	out = append(out, grid)

	tc := confFile{Name: "two_cliques_bridge", Description: "Two directed 5-cliques {0..4} and {5..9} (both directions) joined by the single edge 4->5.", Nodes: 10, Labels: []string{"link", "bridge"}}
	for base := 0; base < 10; base += 5 {
		for a := base; a < base+5; a++ {
			for b := base; b < base+5; b++ {
				if a != b {
					tc.Edges = append(tc.Edges, confEdge{uint32(a), uint32(b), "link", conf(a + b)})
				}
			}
		}
	}
	tc.Edges = append(tc.Edges, confEdge{4, 5, "bridge", 0.9})
	out = append(out, tc)
	return out
}

// fillExpectations computes every expectation with the naive references.
func fillExpectations(t testing.TB, c *confFile) {
	edges := c.edges(t)
	n := c.Nodes
	x := &c.Expect
	labels := func(names []string) LabelSet {
		if names == nil {
			return AnyLabel()
		}
		s := LabelSet{restricted: true}
		for _, nm := range names {
			if i := slices.Index(c.Labels, nm); i >= 0 {
				s.add(uint8(i))
			}
		}
		return s
	}
	cases := []confBFS{
		{Start: 0, Direction: "out", MaxDepth: -1},
		{Start: uint32(n - 1), Direction: "in", MaxDepth: -1},
		{Start: uint32(n / 2), Direction: "both", MaxDepth: 2},
		{Start: 0, Direction: "out", MaxDepth: 2},
	}
	if len(c.Labels) > 1 {
		cases = append(cases, confBFS{Start: 0, Direction: "out", MaxDepth: -1, Labels: c.Labels[:1]})
	}
	for _, b := range cases {
		dir, _ := ParseDirection(b.Direction)
		b.Dist = naiveBFS(n, edges, b.Start, dir, b.MaxDepth, labels(b.Labels))
		b.Levels = levelsFromDist(b.Dist)
		x.BFS = append(x.BFS, b)
	}
	x.SCC = naiveSCC(n, edges)
	x.WCC = naiveWCC(n, edges)
	cnt := len(x.WCC)
	x.WCCCount = &cnt
	x.CoreNumbers = naiveCoreNumbers(n, edges)
	pts, brs := naiveCuts(n, edges)
	x.ArticulationPoints = &pts
	bridges := [][2]uint32{}
	for _, b := range brs {
		bridges = append(bridges, [2]uint32{b.U, b.W})
	}
	x.Bridges = &bridges
	tri := naiveTriangles(n, edges)
	x.Triangles = &tri
	x.PageRank = densePageRank(n, edges, DefaultDamping)
	for _, p := range [][2]uint32{{0, uint32(n - 1)}, {uint32(n - 1), 0}, {1, uint32(n / 2)}} {
		conf, ok := naiveBestConfidence(n, edges, p[0], p[1], AnyLabel())
		x.Paths = append(x.Paths, confPath{From: p[0], To: p[1], Reachable: ok, Confidence: conf})
	}
}

// TestCSRGoldenRegenerate writes the generated golden files when run with
// -csr.regen, and otherwise checks that the files on disk match what the
// references produce (so a stale file is caught).
func TestCSRGoldenRegenerate(t *testing.T) {
	dir := testkit.ConformanceDir(t, suite)
	for _, c := range goldenGraphs() {
		fillExpectations(t, &c)
		b, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		b = append(b, '\n')
		path := filepath.Join(dir, c.Name+".json")
		if *regen {
			if err := os.WriteFile(path, b, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		disk, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (run with -csr.regen to create)", err)
		}
		if string(disk) != string(b) {
			t.Errorf("%s is stale; run go test ./csr -run TestCSRGoldenRegenerate -csr.regen", path)
		}
	}
}
