package csr

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/genuinebnt/flowforge/core"
)

// powerLawEdges draws E = 10V edges whose sources and targets follow a
// skewed (roughly power-law) distribution: index = V * u^3 for a uniform
// u, so low indices act as hubs.
func powerLawEdges(v int, seed uint64) []Edge {
	rng := core.NewSplitMix64(seed)
	skew := func() uint32 { u := core.Float64(rng); return uint32(float64(v) * u * u * u) }
	edges := make([]Edge, 10*v)
	for i := range edges {
		src := skew()
		if i%2 == 0 {
			src = uint32(core.IntN(rng, v))
		}
		edges[i] = Edge{Src: src, Dst: skew(), Label: uint8(core.IntN(rng, 4)), Conf: float32(1+core.IntN(rng, 100)) / 100}
	}
	return edges
}

func benchGraph(b *testing.B, v int) *Graph {
	b.Helper()
	g, err := Build(Meta{}, nodeUUIDs(v), nil, powerLawEdges(v, 1), labelNames(4))
	if err != nil {
		b.Fatal(err)
	}
	return g
}

func BenchmarkCSR01_Build(b *testing.B) {
	for _, v := range []int{10_000, 100_000} {
		b.Run(fmt.Sprintf("V=%d", v), func(b *testing.B) {
			ids, edges := nodeUUIDs(v), powerLawEdges(v, 1)
			b.ReportAllocs()
			var g *Graph
			for b.Loop() {
				var err error
				if g, err = Build(Meta{}, ids, nil, edges, labelNames(4)); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(g.EncodedSize()), "snapshot-bytes")
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(v+len(edges)), "ns/(V+E)")
		})
	}
}

// mapAdjacency is the hash-map adjacency list baseline of b_csr_02.
type mapAdjacency struct {
	n        int
	out, in  map[uint32][]uint32
	outDeg   map[uint32]int
	outEdges int
}

func toMapAdjacency(g *Graph) *mapAdjacency {
	m := &mapAdjacency{n: g.NumNodes(), out: map[uint32][]uint32{}, in: map[uint32][]uint32{}, outDeg: map[uint32]int{}}
	for _, e := range g.Edges() {
		m.out[e.Src] = append(m.out[e.Src], e.Dst)
		m.in[e.Dst] = append(m.in[e.Dst], e.Src)
		m.outDeg[e.Src]++
	}
	return m
}

func (m *mapAdjacency) bfs(start uint32) int {
	seen := map[uint32]bool{start: true}
	q := []uint32{start}
	for i := 0; i < len(q); i++ {
		for _, w := range m.out[q[i]] {
			if !seen[w] {
				seen[w] = true
				q = append(q, w)
			}
		}
	}
	return len(q)
}

func (m *mapAdjacency) pageRank(d, tol float64, maxIt int) []float64 {
	n := float64(m.n)
	rank := map[uint32]float64{}
	for v := range m.n {
		rank[uint32(v)] = 1 / n
	}
	for range maxIt {
		dangling := 0.0
		for v := range m.n {
			if m.outDeg[uint32(v)] == 0 {
				dangling += rank[uint32(v)]
			}
		}
		next := make(map[uint32]float64, m.n)
		delta := 0.0
		for v := range m.n {
			s := 0.0
			for _, u := range m.in[uint32(v)] {
				s += rank[u] / float64(m.outDeg[u])
			}
			x := (1-d)/n + d*(s+dangling/n)
			next[uint32(v)] = x
			delta += math.Abs(x - rank[uint32(v)])
		}
		rank = next
		if delta < tol {
			break
		}
	}
	out := make([]float64, m.n)
	for v, x := range rank {
		out[v] = x
	}
	return out
}

func BenchmarkCSR02_Algorithms(b *testing.B) {
	const v = 100_000
	g := benchGraph(b, v)
	adj := toMapAdjacency(g)
	b.Run("BFS/csr", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := BFS(g, 0, BFSOptions{MaxDepth: -1, Direction: DirOut}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("BFS/hashmap", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			adj.bfs(0)
		}
	})
	b.Run("PageRank/csr", func(b *testing.B) {
		b.ReportAllocs()
		var r PageRankResult
		for b.Loop() {
			r, _ = PageRank(g, PageRankOptions{})
		}
		b.ReportMetric(float64(r.Iterations), "iterations")
	})
	b.Run("PageRank/csr-1worker", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, _ = PageRank(g, PageRankOptions{Workers: 1})
		}
	})
	b.Run("PageRank/hashmap", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			adj.pageRank(DefaultDamping, DefaultTolerance, DefaultMaxIterations)
		}
	})
	b.Run("SCC/csr", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			StronglyConnectedComponents(g)
		}
	})
	b.Run("WCC/csr", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			WeaklyConnectedComponents(g)
		}
	})
}

func BenchmarkCSR03_ColdOpen(b *testing.B) {
	g := benchGraph(b, 100_000)
	path := filepath.Join(b.TempDir(), "snapshot.ffcs")
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := g.WriteTo(f); err != nil {
		b.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
	for _, mode := range []struct {
		name string
		opts OpenOptions
	}{{"verify-all", OpenOptions{}}, {"skip-digest", OpenOptions{SkipDigest: true}}} {
		b.Run(mode.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(g.EncodedSize())
			for b.Loop() {
				s, err := OpenWith(path, mode.opts)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := s.OutNeighbors(0); err != nil { // first query
					b.Fatal(err)
				}
				if err := s.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
