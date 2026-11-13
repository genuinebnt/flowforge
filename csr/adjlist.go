package csr

import (
	"fmt"
	"math"
)

// Representation names a graph layout the analytics can run on
// (docs/CONVENTIONS.md "Pluggable algorithms"; BenchmarkCSRAlgo and
// docs/ALGORITHMS.md have the comparison).
type Representation string

// Representations.
const (
	// ReprCSR is the compressed sparse row layout of spec 21.2: contiguous
	// offset and target arrays, the transposed copy for in-edges. Every
	// analysis in this package runs on it; it is the default.
	ReprCSR Representation = "csr"
	// ReprAdjacencyList is a hash-map adjacency list (node -> neighbour
	// slice), the textbook baseline spec 21 measures CSR against (b_csr_02).
	// Only BFS reachability and PageRank are implemented on it. It is
	// cheaper to mutate, which CSR is not; for read-mostly analytics it
	// loses on cache locality and map lookups.
	ReprAdjacencyList Representation = "adjacency_list"
)

// Representations returns every Representation, default first.
func Representations() []Representation { return []Representation{ReprCSR, ReprAdjacencyList} }

// AdjacencyList is the ReprAdjacencyList baseline built from a Graph. It
// keeps parallel edges (so PageRank counts them with multiplicity, as
// PageRank on the CSR does) and ignores labels and confidences. It is
// immutable after NewAdjacencyList and safe for concurrent reads.
type AdjacencyList struct {
	n       int
	out, in map[uint32][]uint32
	outDeg  map[uint32]int
}

// NewAdjacencyList copies g's edges into maps.
func NewAdjacencyList(g *Graph) *AdjacencyList {
	a := &AdjacencyList{n: g.NumNodes(), out: map[uint32][]uint32{}, in: map[uint32][]uint32{}, outDeg: map[uint32]int{}}
	for _, e := range g.Edges() {
		a.out[e.Src] = append(a.out[e.Src], e.Dst)
		a.in[e.Dst] = append(a.in[e.Dst], e.Src)
		a.outDeg[e.Src]++
	}
	return a
}

// NumNodes returns the number of nodes.
func (a *AdjacencyList) NumNodes() int { return a.n }

// Reach returns the number of nodes reachable from start along out-edges,
// start included: BFS(g, start, BFSOptions{MaxDepth: -1, Direction:
// DirOut}).Reached() on the CSR.
func (a *AdjacencyList) Reach(start uint32) (int, error) {
	if int(start) >= a.n {
		return 0, fmt.Errorf("%w: start node %d of %d", ErrInvalidInput, start, a.n)
	}
	seen := map[uint32]bool{start: true}
	q := []uint32{start}
	for i := 0; i < len(q); i++ {
		for _, w := range a.out[q[i]] {
			if !seen[w] {
				seen[w] = true
				q = append(q, w)
			}
		}
	}
	return len(q), nil
}

// PageRank is PageRank (spec 21.5) on the adjacency list, with the same
// options, defaults, dangling-mass rule and stopping rule; scores agree
// with the CSR version up to floating-point summation order. Workers is
// ignored (single-threaded).
func (a *AdjacencyList) PageRank(opts PageRankOptions) (PageRankResult, error) {
	d, tol, maxIt := opts.Damping, opts.Tolerance, opts.MaxIterations
	if d == 0 {
		d = DefaultDamping
	}
	if tol == 0 {
		tol = DefaultTolerance
	}
	if maxIt == 0 {
		maxIt = DefaultMaxIterations
	}
	if !(d > 0 && d < 1) || !(tol > 0) || math.IsInf(tol, 0) || maxIt < 0 {
		return PageRankResult{}, fmt.Errorf("%w: damping %v tolerance %v iterations %d", ErrInvalidInput, d, tol, maxIt)
	}
	if a.n == 0 {
		return PageRankResult{Scores: []float64{}, Converged: true}, nil
	}
	n := float64(a.n)
	rank := make(map[uint32]float64, a.n)
	for v := range a.n {
		rank[uint32(v)] = 1 / n
	}
	res := PageRankResult{}
	for it := 1; it <= maxIt; it++ {
		dangling := 0.0
		for v := range a.n {
			if a.outDeg[uint32(v)] == 0 {
				dangling += rank[uint32(v)]
			}
		}
		next := make(map[uint32]float64, a.n)
		delta := 0.0
		for v := range a.n {
			s := 0.0
			for _, u := range a.in[uint32(v)] {
				s += rank[u] / float64(a.outDeg[u])
			}
			x := (1-d)/n + d*(s+dangling/n)
			next[uint32(v)] = x
			delta += math.Abs(x - rank[uint32(v)])
		}
		rank = next
		res.Iterations, res.Delta = it, delta
		if delta < tol {
			res.Converged = true
			break
		}
	}
	res.Scores = make([]float64, a.n)
	for v, x := range rank {
		res.Scores[v] = x
	}
	return res, nil
}
