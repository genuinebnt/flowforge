package csr

import (
	"math"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// TestCSRProp_AdjacencyListMatchesCSR runs the ReprAdjacencyList baseline
// against the CSR on random multigraphs (self loops, parallel edges,
// dangling nodes): the same reachable set size from every start, and
// PageRank scores within 1e-12 with the same iteration count and
// convergence.
func TestCSRProp_AdjacencyListMatchesCSR(t *testing.T) {
	rng := testkit.Rng(t)
	for c := range max(testkit.Cases()/10, 20) {
		n := 1 + core.IntN(rng, 40)
		g := randomGraph(t, rng, n, core.IntN(rng, 4*n), 3)
		a := NewAdjacencyList(g)
		for s := range n {
			r, err := BFS(g, uint32(s), BFSOptions{MaxDepth: -1, Direction: DirOut})
			if err != nil {
				t.Fatal(err)
			}
			if got, err := a.Reach(uint32(s)); err != nil || got != r.Reached() {
				t.Fatalf("case %d start %d: adjacency reach %d (%v), csr %d", c, s, got, err, r.Reached())
			}
		}
		want, err := PageRank(g, PageRankOptions{Workers: 1})
		if err != nil {
			t.Fatal(err)
		}
		got, err := a.PageRank(PageRankOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Iterations != want.Iterations || got.Converged != want.Converged {
			t.Fatalf("case %d: iterations %d/%v, csr %d/%v", c, got.Iterations, got.Converged, want.Iterations, want.Converged)
		}
		for v := range want.Scores {
			if math.Abs(got.Scores[v]-want.Scores[v]) > 1e-12 {
				t.Fatalf("case %d node %d: %v vs csr %v", c, v, got.Scores[v], want.Scores[v])
			}
		}
	}
	if _, err := NewAdjacencyList(randomGraph(t, rng, 3, 2, 1)).Reach(3); err == nil {
		t.Error("Reach outside the graph accepted")
	}
}
