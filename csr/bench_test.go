package csr

import (
	"fmt"
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

func BenchmarkCSR02_Algorithms(b *testing.B) {
	const v = 100_000
	g := benchGraph(b, v)
	adj := NewAdjacencyList(g)
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
			_, _ = adj.Reach(0)
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
			_, _ = adj.PageRank(PageRankOptions{})
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

// BenchmarkCSRAlgo compares the selectable Representations (one decision
// point: the analytics layout, spec 21.2) on whole-graph BFS reachability
// and PageRank at 10,000 and 100,000 nodes (E = 10V, skewed degrees):
//
//	go test -run '^$' -bench CSRAlgo ./csr
func BenchmarkCSRAlgo(b *testing.B) {
	for _, v := range []int{10_000, 100_000} {
		g := benchGraph(b, v)
		adj := NewAdjacencyList(g)
		algos := map[Representation]map[string]func() error{
			ReprCSR: {
				"bfs":      func() error { _, err := BFS(g, 0, BFSOptions{MaxDepth: -1, Direction: DirOut}); return err },
				"pagerank": func() error { _, err := PageRank(g, PageRankOptions{Workers: 1}); return err },
			},
			ReprAdjacencyList: {
				"bfs":      func() error { _, err := adj.Reach(0); return err },
				"pagerank": func() error { _, err := adj.PageRank(PageRankOptions{}); return err },
			},
		}
		for _, r := range Representations() {
			for _, algo := range []string{"bfs", "pagerank"} {
				run := algos[r][algo]
				b.Run(fmt.Sprintf("%s/%s/V=%d", r, algo, v), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						if err := run(); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		}
	}
}
