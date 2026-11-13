package csr

import (
	"fmt"
	"math"
	"slices"
)

// PathOptions parameterises ShortestPath.
type PathOptions struct {
	// Labels filters the edges a path may use; the zero value allows all.
	Labels LabelSet
	// MaxCost, when positive, discards paths whose total cost
	// (sum of -ln(confidence)) exceeds it. Zero means unbounded.
	MaxCost float64
}

// Path is a most-likely path between two nodes.
type Path struct {
	// Nodes runs from the source to the target inclusive.
	Nodes []uint32
	// Edges holds the forward edge indices, len(Nodes)-1 of them.
	Edges []uint32
	// Cost is the sum of -ln(confidence) over Edges.
	Cost float64
	// Confidence is exp(-Cost), the product of the edge confidences.
	Confidence float64
}

// ShortestPath finds the most likely path from one node to another
// (spec 5.9 path, 21.5 Dijkstra): edge cost -ln(confidence), binary heap
// with lazy deletion, O((V + E) log V). Edges with confidence 0 are never
// used. It reports false when no path exists within MaxCost. Ties between
// equally likely paths are broken deterministically (lower node index is
// settled first; the first edge in row order wins among parallel edges).
func ShortestPath(g *Graph, from, to uint32, opts PathOptions) (Path, bool, error) {
	if !g.has(from) || !g.has(to) {
		return Path{}, false, fmt.Errorf("%w: path %d -> %d", ErrNodeRange, from, to)
	}
	if math.IsNaN(opts.MaxCost) || opts.MaxCost < 0 {
		return Path{}, false, fmt.Errorf("%w: max cost %v", ErrInvalidInput, opts.MaxCost)
	}
	limit := math.Inf(1)
	if opts.MaxCost > 0 {
		limit = opts.MaxCost
	}
	nv := len(g.nodeID)
	dist := make([]float64, nv)
	for i := range dist {
		dist[i] = math.Inf(1)
	}
	const none = math.MaxUint32
	prevEdge := make([]uint32, nv)
	prevNode := make([]uint32, nv)
	dist[from] = 0
	prevEdge[from] = none
	h := minHeap{{cost: 0, node: from}}
	for len(h) > 0 {
		it := h.pop()
		if it.cost > dist[it.node] {
			continue // stale entry (lazy deletion)
		}
		v := it.node
		if v == to {
			break
		}
		for i := g.offsets[v]; i < g.offsets[v+1]; i++ {
			c := g.edgeConf[i]
			if c <= 0 || !opts.Labels.Has(g.edgeLabel[i]) {
				continue
			}
			w := g.targets[i]
			nd := it.cost + edgeCost(c)
			if nd < dist[w] && nd <= limit {
				dist[w] = nd
				prevEdge[w] = uint32(i)
				prevNode[w] = v
				h.push(heapItem{cost: nd, node: w})
			}
		}
	}
	if math.IsInf(dist[to], 1) {
		return Path{}, false, nil
	}
	p := Path{Cost: dist[to], Confidence: math.Exp(-dist[to])}
	for v := to; ; v = prevNode[v] {
		p.Nodes = append(p.Nodes, v)
		if v == from {
			break
		}
		p.Edges = append(p.Edges, prevEdge[v])
	}
	slices.Reverse(p.Nodes)
	slices.Reverse(p.Edges)
	return p, true, nil
}

// edgeCost returns -ln(c) for c in (0, 1], never negative (c = 1 gives
// -0, which compares equal to 0).
//
// PORT: Go's math.Log and Rust's f64::ln (libm) can differ in the last
// bit, so a port may break ties between equally likely paths differently;
// compare path costs with a tolerance, not exact bits.
func edgeCost(c float32) float64 {
	x := -math.Log(float64(c))
	if x < 0 {
		return 0
	}
	return x
}

type heapItem struct {
	cost float64
	node uint32
}

func (a heapItem) less(b heapItem) bool {
	return a.cost < b.cost || (a.cost == b.cost && a.node < b.node)
}

// minHeap is a binary min-heap ordered by (cost, node).
type minHeap []heapItem

func (h *minHeap) push(x heapItem) {
	*h = append(*h, x)
	s := *h
	i := len(s) - 1
	for i > 0 {
		p := (i - 1) / 2
		if !s[i].less(s[p]) {
			break
		}
		s[i], s[p] = s[p], s[i]
		i = p
	}
}

func (h *minHeap) pop() heapItem {
	s := *h
	top := s[0]
	last := len(s) - 1
	s[0] = s[last]
	s = s[:last]
	i := 0
	for {
		l, r, m := 2*i+1, 2*i+2, i
		if l < len(s) && s[l].less(s[m]) {
			m = l
		}
		if r < len(s) && s[r].less(s[m]) {
			m = r
		}
		if m == i {
			break
		}
		s[i], s[m] = s[m], s[i]
		i = m
	}
	*h = s
	return top
}
