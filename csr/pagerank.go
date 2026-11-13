package csr

import (
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
)

// PageRank defaults (spec 21.5).
const (
	DefaultDamping       = 0.85
	DefaultTolerance     = 1e-6
	DefaultMaxIterations = 100
)

// pageRankChunk is the number of nodes per unit of parallel work. Partial
// sums are combined per chunk in chunk order, so the result does not
// depend on the number of workers.
const pageRankChunk = 4096

// PageRankOptions parameterises PageRank. Zero fields take the defaults.
type PageRankOptions struct {
	Damping       float64 // in (0, 1); default 0.85
	Tolerance     float64 // L1 change that stops the iteration; default 1e-6
	MaxIterations int     // default 100
	// Workers bounds the goroutines used per iteration; default
	// runtime.GOMAXPROCS(0). It never changes the result.
	Workers int
}

// PageRankResult holds the scores, which sum to 1 up to float rounding.
type PageRankResult struct {
	Scores     []float64
	Iterations int
	Converged  bool
	// Delta is the L1 change of the last iteration.
	Delta float64
}

// PageRank ranks nodes by structural importance (spec 21.5). Iteration is
// pull-based over the transposed CSR: each node sums the contributions of
// its in-neighbours, so a parallel loop over node ranges needs no atomics.
// The mass of dangling nodes (no out-edges) is redistributed uniformly.
// Parallel edges count with multiplicity. The result is deterministic and
// independent of Workers. Errors wrap ErrInvalidInput.
//
// PORT: the Go result is bit-reproducible because every sum has a fixed
// order (in-edges in transpose order, chunk partials in chunk order), but
// a port need not match bits: conformance compares scores with absolute
// tolerance 1e-5 plus rank order (docs/PORTING_RUST.md 5.5).
func PageRank(g *Graph, opts PageRankOptions) (PageRankResult, error) {
	d, tol, maxIt, workers := opts.Damping, opts.Tolerance, opts.MaxIterations, opts.Workers
	if d == 0 {
		d = DefaultDamping
	}
	if tol == 0 {
		tol = DefaultTolerance
	}
	if maxIt == 0 {
		maxIt = DefaultMaxIterations
	}
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	if !(d > 0 && d < 1) || !(tol > 0) || math.IsInf(tol, 0) || maxIt < 0 {
		return PageRankResult{}, fmt.Errorf("%w: damping %v tolerance %v iterations %d", ErrInvalidInput, d, tol, maxIt)
	}
	nv := len(g.nodeID)
	if nv == 0 {
		return PageRankResult{Scores: []float64{}, Converged: true}, nil
	}
	n := float64(nv)
	rank := make([]float64, nv)
	next := make([]float64, nv)
	contrib := make([]float64, nv)
	for i := range rank {
		rank[i] = 1 / n
	}
	chunks := (nv + pageRankChunk - 1) / pageRankChunk
	partial := make([]float64, chunks)
	span := func(c int) (int, int) { return c * pageRankChunk, min((c+1)*pageRankChunk, nv) }
	sum := func() float64 {
		s := 0.0
		for _, x := range partial {
			s += x
		}
		return s
	}
	res := PageRankResult{}
	for it := 1; it <= maxIt; it++ {
		// Phase 1: per-node contribution rank/outdeg; dangling mass.
		parallelChunks(chunks, workers, func(c int) {
			lo, hi := span(c)
			dangling := 0.0
			for v := lo; v < hi; v++ {
				deg := g.offsets[v+1] - g.offsets[v]
				if deg == 0 {
					dangling += rank[v]
					contrib[v] = 0
				} else {
					contrib[v] = rank[v] / float64(deg)
				}
			}
			partial[c] = dangling
		})
		base := (1-d)/n + d*sum()/n
		// Phase 2: pull from in-neighbours; L1 change.
		parallelChunks(chunks, workers, func(c int) {
			lo, hi := span(c)
			delta := 0.0
			for v := lo; v < hi; v++ {
				s := 0.0
				for j := g.inOffsets[v]; j < g.inOffsets[v+1]; j++ {
					s += contrib[g.inSources[j]]
				}
				x := base + d*s
				next[v] = x
				delta += math.Abs(x - rank[v])
			}
			partial[c] = delta
		})
		rank, next = next, rank
		res.Iterations = it
		res.Delta = sum()
		if res.Delta < tol {
			res.Converged = true
			break
		}
	}
	res.Scores = rank
	return res, nil
}

// parallelChunks runs fn(c) for every c in [0, chunks) on up to workers
// goroutines and waits for all of them.
func parallelChunks(chunks, workers int, fn func(c int)) {
	workers = min(workers, chunks)
	if workers <= 1 {
		for c := range chunks {
			fn(c)
		}
		return
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for {
				c := int(next.Add(1) - 1)
				if c >= chunks {
					return
				}
				fn(c)
			}
		})
	}
	wg.Wait()
}
