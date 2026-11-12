package vecindex

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"sync"

	"github.com/genuinebnt/flowforge/core"
)

// Vamana defaults (spec 22.4).
const (
	// DefaultR is the maximum out-degree.
	DefaultR = 64
	// DefaultLBuild is the construction search list size.
	DefaultLBuild = 100
	// DefaultAlpha is the pruning factor of the second pass.
	DefaultAlpha = 1.2
	// MaxR bounds the out-degree a segment file may declare.
	MaxR = 1024
)

// VamanaParams configures graph construction.
type VamanaParams struct {
	// R is the maximum out-degree; zero means 64.
	R int
	// LBuild is the greedy-search list size during construction; zero
	// means 100.
	LBuild int
	// Alpha is the pruning factor of the second pass; zero means 1.2. The
	// first pass always uses 1.0.
	Alpha float64
	// Batch is the number of points inserted against the same snapshot of
	// the graph. Zero or one is the sequential algorithm of spec 22.4. A
	// larger batch enables Workers: the searches and prunes of a batch run
	// in parallel, then the batch's back edges are applied per target node,
	// in batch order. The graph depends on Batch but never on Workers, so a
	// parallel build is exactly as deterministic as a sequential one.
	Batch int
	// Workers bounds parallelism when Batch > 1; zero means one.
	Workers int
}

func (p VamanaParams) withDefaults() (VamanaParams, error) {
	if p.R == 0 {
		p.R = DefaultR
	}
	if p.LBuild == 0 {
		p.LBuild = DefaultLBuild
	}
	if p.Alpha == 0 {
		p.Alpha = DefaultAlpha
	}
	if p.Batch <= 0 {
		p.Batch = 1
	}
	if p.Workers <= 0 {
		p.Workers = 1
	}
	if p.R < 1 || p.R > MaxR {
		return p, fmt.Errorf("%w: R=%d outside 1..%d", ErrInvalidConfig, p.R, MaxR)
	}
	if p.LBuild < 1 {
		return p, fmt.Errorf("%w: L_build=%d", ErrInvalidConfig, p.LBuild)
	}
	if p.Alpha < 1 || math.IsNaN(p.Alpha) || math.IsInf(p.Alpha, 0) {
		return p, fmt.Errorf("%w: alpha=%v must be >= 1", ErrInvalidConfig, p.Alpha)
	}
	return p, nil
}

// Graph is a Vamana proximity graph over a fixed set of vectors (spec
// 22.4). Node i is the i-th input vector.
type Graph struct {
	metric Metric
	dim    int
	r      int
	vecs   []float32 // n * dim, prepared (unit length under Cosine)
	adj    [][]uint32
	medoid uint32
}

// BuildGraph builds a Vamana graph over vectors (all of one dimension)
// with the given metric. Under Cosine the vectors are normalised first.
// Every random choice (the initial graph and the insertion orders) comes
// from rng.
func BuildGraph(vectors [][]float32, metric Metric, p VamanaParams, rng core.Rng) (*Graph, error) {
	if len(vectors) == 0 {
		return nil, fmt.Errorf("vecindex: build graph: %w", ErrEmpty)
	}
	if !metric.Valid() {
		return nil, fmt.Errorf("%w: %v", ErrInvalidMetric, metric)
	}
	dim := len(vectors[0])
	if dim < 1 || dim > MaxDimension {
		return nil, fmt.Errorf("%w: dimension %d", ErrInvalidConfig, dim)
	}
	flat := make([]float32, 0, len(vectors)*dim)
	for i, v := range vectors {
		pv, err := prepareVector(metric, dim, v)
		if err != nil {
			return nil, fmt.Errorf("vecindex: build graph: vector %d: %w", i, err)
		}
		flat = append(flat, pv...)
	}
	return buildGraphFlat(flat, dim, metric, p, rng)
}

// buildGraphFlat builds over already prepared row-major vectors.
func buildGraphFlat(flat []float32, dim int, metric Metric, p VamanaParams, rng core.Rng) (*Graph, error) {
	p, err := p.withDefaults()
	if err != nil {
		return nil, err
	}
	n := len(flat) / dim
	if n == 0 {
		return nil, fmt.Errorf("vecindex: build graph: %w", ErrEmpty)
	}
	if uint64(n) > math.MaxUint32 {
		return nil, fmt.Errorf("%w: %d vectors exceed the u32 node id space", ErrInvalidConfig, n)
	}
	if metric == InnerProduct {
		return buildInnerProductGraph(flat, dim, p, rng)
	}
	g := &Graph{metric: metric, dim: dim, r: p.R, vecs: flat}
	g.randomInit(rng)
	g.medoid = g.computeMedoid()
	b := &builder{g: g, p: p}
	for _, alpha := range [2]float64{1.0, p.Alpha} {
		perm := make([]uint32, n)
		for i := range perm {
			perm[i] = uint32(i)
		}
		core.Shuffle(rng, n, func(i, j int) { perm[i], perm[j] = perm[j], perm[i] })
		b.pass(perm, alpha)
	}
	g.ensureReachable()
	return g, nil
}

// ensureReachable links every node the medoid cannot reach. Vamana does not
// guarantee connectivity (small graphs with a small R occasionally strand a
// node), and an unreachable node can never be returned by a search, so
// after the two passes each stranded node u gets an in-edge from the
// nearest reachable node with spare degree, or, when every reachable node
// is full, replaces that node's farthest neighbour (after which
// reachability is recomputed). This post-pass is not in spec 22.4; it only
// adds or swaps edges, so the degree bound R still holds.
func (g *Graph) ensureReachable() {
	n := g.len()
	reached := make([]bool, n)
	var queue []uint32
	expand := func(from uint32) {
		if reached[from] {
			return
		}
		reached[from] = true
		queue = append(queue[:0], from)
		for len(queue) > 0 {
			x := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			for _, j := range g.adj[x] {
				if !reached[j] {
					reached[j] = true
					queue = append(queue, j)
				}
			}
		}
	}
	expand(g.medoid)
	for budget := n; budget > 0; budget-- {
		u := slices.Index(reached, false)
		if u < 0 {
			return
		}
		uv := g.vec(uint32(u))
		spare, full := -1, -1
		var spareD, fullD float32
		for v := range n {
			if !reached[v] {
				continue
			}
			d := g.dist(uv, g.vec(uint32(v)))
			if len(g.adj[v]) < g.r {
				if spare < 0 || d < spareD {
					spare, spareD = v, d
				}
			} else if full < 0 || d < fullD {
				full, fullD = v, d
			}
		}
		if spare >= 0 {
			g.adj[spare] = append(g.adj[spare], uint32(u))
			expand(uint32(u))
			continue
		}
		// Every reachable node is full: swap out full's farthest neighbour
		// and recompute reachability from scratch.
		vv := g.vec(uint32(full))
		far, farD := 0, float32(-1)
		for i, j := range g.adj[full] {
			if d := g.dist(vv, g.vec(j)); d > farD {
				far, farD = i, d
			}
		}
		g.adj[full][far] = uint32(u)
		clear(reached)
		expand(g.medoid)
	}
}

// buildInnerProductGraph builds a graph for maximum inner product search.
// -dot is not a metric (it is negative, and d(p, p) is not minimal), so the
// alpha rule of robust_prune is meaningless on it. The graph is therefore
// built with L2 over the standard MIPS-to-L2 reduction: every data vector x
// gets an extra coordinate sqrt(M^2 - |x|^2), M being the largest norm, so
// all augmented vectors have norm M. A query q augmented with 0 then has
// |q|^2 + M^2 - 2 dot(q, x) as squared L2 distance to x, which ranks
// exactly like -dot(q, x); searches therefore use -dot unchanged.
func buildInnerProductGraph(flat []float32, dim int, p VamanaParams, rng core.Rng) (*Graph, error) {
	n := len(flat) / dim
	norms := make([]float64, n)
	var maxNorm float64
	for i := range n {
		for _, x := range flat[i*dim : (i+1)*dim] {
			norms[i] += float64(x) * float64(x)
		}
		maxNorm = max(maxNorm, norms[i])
	}
	aug := make([]float32, 0, n*(dim+1))
	for i := range n {
		aug = append(aug, flat[i*dim:(i+1)*dim]...)
		aug = append(aug, float32(math.Sqrt(max(0, maxNorm-norms[i]))))
	}
	g, err := buildGraphFlat(aug, dim+1, L2, p, rng)
	if err != nil {
		return nil, err
	}
	g.metric, g.dim, g.vecs = InnerProduct, dim, flat
	return g, nil
}

// randomInit gives every node min(R, n-1) distinct random out-neighbours.
func (g *Graph) randomInit(rng core.Rng) {
	n := len(g.vecs) / g.dim
	deg := min(g.r, n-1)
	g.adj = make([][]uint32, n)
	pool := make([]uint32, 0, deg)
	for i := range n {
		pool = pool[:0]
		if deg*2 >= n-1 {
			// Dense: sample by shuffling all other nodes.
			all := make([]uint32, 0, n-1)
			for j := range n {
				if j != i {
					all = append(all, uint32(j))
				}
			}
			core.Shuffle(rng, len(all), func(a, b int) { all[a], all[b] = all[b], all[a] })
			pool = append(pool, all[:deg]...)
		} else {
			for len(pool) < deg {
				j := uint32(core.IntN(rng, n))
				if int(j) == i || slices.Contains(pool, j) {
					continue
				}
				pool = append(pool, j)
			}
		}
		adj := make([]uint32, len(pool), g.r+1)
		copy(adj, pool)
		g.adj[i] = adj
	}
}

// computeMedoid returns the node closest (squared L2) to the centroid.
func (g *Graph) computeMedoid() uint32 {
	n := g.len()
	c := make([]float64, g.dim)
	for i := range n {
		for j, x := range g.vec(uint32(i)) {
			c[j] += float64(x)
		}
	}
	cf := make([]float32, g.dim)
	for j := range c {
		cf[j] = float32(c[j] / float64(n))
	}
	best, bestD := uint32(0), float32(math.Inf(1))
	for i := range n {
		if d := L2Squared(cf, g.vec(uint32(i))); d < bestD {
			best, bestD = uint32(i), d
		}
	}
	return best
}

func (g *Graph) len() int { return len(g.adj) }

func (g *Graph) vec(i uint32) []float32 {
	o := int(i) * g.dim
	return g.vecs[o : o+g.dim : o+g.dim]
}

func (g *Graph) dist(a, b []float32) float32 { return sanitize(Distance(g.metric, a, b)) }

// Len returns the number of nodes.
func (g *Graph) Len() int { return g.len() }

// R returns the maximum out-degree.
func (g *Graph) R() int { return g.r }

// Medoid returns the entry node.
func (g *Graph) Medoid() uint32 { return g.medoid }

// Neighbors returns a copy of node i's out-neighbours, or nil when i is out
// of range.
func (g *Graph) Neighbors(i uint32) []uint32 {
	if int(i) >= g.len() {
		return nil
	}
	return slices.Clone(g.adj[i])
}

// Search runs the in-memory greedy search of spec 22.4 for q with list size
// L and returns the k closest nodes found, sorted by distance then node id.
func (g *Graph) Search(q []float32, k, L int) ([]GraphHit, error) {
	pq, err := prepareVector(g.metric, g.dim, q)
	if err != nil {
		return nil, err
	}
	if k < 1 || L < k {
		return nil, fmt.Errorf("%w: k=%d L=%d", ErrInvalidParams, k, L)
	}
	s := newSearchScratch(g.len())
	list, _ := g.greedySearch(s, g.medoid, pq, L)
	out := make([]GraphHit, 0, min(k, len(list)))
	for _, c := range list[:min(k, len(list))] {
		out = append(out, GraphHit{Node: c.id, Distance: c.dist})
	}
	return out, nil
}

// GraphHit is one result of Graph.Search.
type GraphHit struct {
	Node     uint32
	Distance float32
}

// cand is a node and its distance to the current query or pivot.
type cand struct {
	id       uint32
	dist     float32
	expanded bool
}

func compareCand(a, b cand) int {
	if c := cmp.Compare(a.dist, b.dist); c != 0 {
		return c
	}
	return cmp.Compare(a.id, b.id)
}

// searchScratch is reusable per-search state: a generation-stamped "seen"
// array avoids clearing an n-sized set per query.
type searchScratch struct {
	stamp   []uint32
	gen     uint32
	list    []cand
	vis     []cand
	removed []bool // robustPrune scratch
}

func newSearchScratch(n int) *searchScratch { return &searchScratch{stamp: make([]uint32, n)} }

// begin starts a new search generation.
func (s *searchScratch) begin() {
	s.gen++
	if s.gen == 0 { // wrapped: reset every stamp
		clear(s.stamp)
		s.gen = 1
	}
	s.list = s.list[:0]
	s.vis = s.vis[:0]
}

// mark records i as seen, reporting whether it was new.
func (s *searchScratch) mark(i uint32) bool {
	if s.stamp[i] == s.gen {
		return false
	}
	s.stamp[i] = s.gen
	return true
}

// insertBounded inserts c into the sorted list keeping at most L entries.
// It returns the insert position, or -1 when c did not make the cut.
func insertBounded(list []cand, c cand, L int) ([]cand, int) {
	if len(list) >= L && compareCand(c, list[len(list)-1]) >= 0 {
		return list, -1
	}
	pos, _ := slices.BinarySearchFunc(list, c, compareCand)
	if len(list) < L {
		list = append(list, cand{})
	}
	copy(list[pos+1:], list[pos:len(list)-1])
	list[pos] = c
	return list, pos
}

// greedySearch is greedy_search of spec 22.4. It returns the final list
// (at most L entries, sorted) and the visited (expanded) nodes with their
// distances to q. Both alias s.
func (g *Graph) greedySearch(s *searchScratch, start uint32, q []float32, L int) ([]cand, []cand) {
	s.begin()
	s.mark(start)
	s.list = append(s.list, cand{id: start, dist: g.dist(q, g.vec(start))})
	cur := 0 // first unexpanded position
	for cur < len(s.list) {
		p := s.list[cur]
		s.list[cur].expanded = true
		s.vis = append(s.vis, p)
		next := len(s.list) // lowest newly inserted position
		for _, n := range g.adj[p.id] {
			if !s.mark(n) {
				continue
			}
			c := cand{id: n, dist: g.dist(q, g.vec(n))}
			var pos int
			s.list, pos = insertBounded(s.list, c, L)
			if pos >= 0 && pos < next {
				next = pos
			}
		}
		// Advance to the closest unexpanded entry.
		cur = min(cur+1, next)
		for cur < len(s.list) && s.list[cur].expanded {
			cur++
		}
	}
	return s.list, s.vis
}

// robustPrune is robust_prune of spec 22.4: candidates carry their
// distance to p; the result has at most R entries. cands is reordered.
func (g *Graph) robustPrune(p uint32, cands []cand, alpha float64, R int, s *searchScratch) []uint32 {
	// V = V - {p}, deduplicated, ordered by distance to p.
	cands = slices.DeleteFunc(cands, func(c cand) bool { return c.id == p })
	slices.SortFunc(cands, compareCand)
	cands = slices.CompactFunc(cands, func(a, b cand) bool { return a.id == b.id })
	if cap(s.removed) < len(cands) {
		s.removed = make([]bool, len(cands))
	}
	removed := s.removed[:len(cands)]
	clear(removed)
	out := make([]uint32, 0, R+1)
	a := float32(alpha)
	for i, c := range cands {
		if removed[i] {
			continue
		}
		// c is the argmin over the remaining V.
		out = append(out, c.id)
		if len(out) == R {
			break
		}
		pv := g.vec(c.id)
		for j := i + 1; j < len(cands); j++ {
			if removed[j] {
				continue
			}
			if a*g.dist(pv, g.vec(cands[j].id)) <= cands[j].dist {
				removed[j] = true
			}
		}
	}
	return out
}

// builder carries the reusable state of one build.
type builder struct {
	g *Graph
	p VamanaParams
}

// insertResult is the outcome of the parallel phase for one point.
type insertResult struct {
	p   uint32
	out []uint32
}

// pass runs one construction pass over perm with the given alpha.
func (b *builder) pass(perm []uint32, alpha float64) {
	g := b.g
	workers := min(b.p.Workers, b.p.Batch)
	scratch := make([]*searchScratch, workers)
	for w := range scratch {
		scratch[w] = newSearchScratch(g.len())
	}
	results := make([]insertResult, b.p.Batch)
	for start := 0; start < len(perm); start += b.p.Batch {
		batch := perm[start:min(start+b.p.Batch, len(perm))]
		// Phase 1: search and prune every point of the batch against the
		// same graph snapshot.
		parallelFor(len(batch), workers, func(w, i int) {
			p := batch[i]
			_, vis := g.greedySearch(scratch[w], g.medoid, g.vec(p), b.p.LBuild)
			cands := slices.Clone(vis)
			results[i] = insertResult{p: p, out: g.robustPrune(p, cands, alpha, b.p.R, scratch[w])}
		})
		// Phase 2: install the new out-lists, then group back edges by
		// target, in batch order.
		back := make(map[uint32][]uint32)
		var targets []uint32
		for _, r := range results[:len(batch)] {
			g.adj[r.p] = r.out
			for _, j := range r.out {
				if _, ok := back[j]; !ok {
					targets = append(targets, j)
				}
				back[j] = append(back[j], r.p)
			}
		}
		// Phase 3: each target is independent, so targets run in parallel.
		parallelFor(len(targets), workers, func(w, i int) {
			j := targets[i]
			adj := g.adj[j]
			for _, p := range back[j] {
				if p != j && !slices.Contains(adj, p) {
					adj = append(adj, p)
				}
			}
			if len(adj) > b.p.R {
				jv := g.vec(j)
				cands := make([]cand, len(adj))
				for x, n := range adj {
					cands[x] = cand{id: n, dist: g.dist(jv, g.vec(n))}
				}
				adj = g.robustPrune(j, cands, alpha, b.p.R, scratch[w])
			}
			g.adj[j] = adj
		})
	}
}

// parallelFor runs fn(worker, i) for i in [0, n) on up to workers
// goroutines; worker identifies per-goroutine scratch state.
func parallelFor(n, workers int, fn func(w, i int)) {
	if workers <= 1 || n <= 1 {
		for i := range n {
			fn(0, i)
		}
		return
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	next := 0
	for w := range min(workers, n) {
		wg.Go(func() {
			for {
				mu.Lock()
				i := next
				next++
				mu.Unlock()
				if i >= n {
					return
				}
				fn(w, i)
			}
		})
	}
	wg.Wait()
}
