package csr

import (
	"fmt"
	"slices"
)

// MaxPointDepth is the depth limit of point queries (neighbors and
// blast_radius, spec 5.9).
const MaxPointDepth = 6

// BFSOptions parameterises BFS.
type BFSOptions struct {
	// MaxDepth bounds the number of hops; a negative value means unbounded
	// and 0 returns only the start node.
	MaxDepth int
	// Direction is required: DirOut, DirIn or DirBoth.
	Direction Direction
	// Labels filters the edges followed; the zero value follows all.
	Labels LabelSet
	// WithEdges also returns the edges followed (BFSResult.Edges). It
	// costs a sort of those edges, so whole-graph reachability leaves it
	// off.
	WithEdges bool
}

// BFSResult is the outcome of a breadth-first search.
type BFSResult struct {
	// Levels[d] holds the nodes at distance d, ascending. Levels[0] is the
	// start node.
	Levels [][]uint32
	// Edges holds, when BFSOptions.WithEdges is set, the forward edge
	// indices followed from a node at depth below MaxDepth (the edges
	// between the returned levels), ascending, each at most once.
	Edges []uint32
}

// Reached returns the number of nodes found, including the start.
func (r BFSResult) Reached() int {
	n := 0
	for _, l := range r.Levels {
		n += len(l)
	}
	return n
}

// Distances expands the levels into a distance array of length n with -1
// for unreached nodes.
func (r BFSResult) Distances(n int) []int32 {
	d := make([]int32, n)
	for i := range d {
		d[i] = -1
	}
	for depth, l := range r.Levels {
		for _, v := range l {
			if int(v) < n {
				d[v] = int32(depth)
			}
		}
	}
	return d
}

// BFS runs a bounded breadth-first search from start (spec 21.5): a
// frontier queue plus a visited bitset, O(V + E).
func BFS(g *Graph, start uint32, opts BFSOptions) (BFSResult, error) {
	if !g.has(start) {
		return BFSResult{}, fmt.Errorf("%w: start %d", ErrNodeRange, start)
	}
	if !opts.Direction.valid() {
		return BFSResult{}, fmt.Errorf("%w: direction %s", ErrInvalidInput, opts.Direction)
	}
	visited := newBitset(len(g.nodeID))
	visited.set(start)
	queue := []uint32{start}
	res := BFSResult{}
	var bounds []int
	out := opts.Direction == DirOut || opts.Direction == DirBoth
	in := opts.Direction == DirIn || opts.Direction == DirBoth
	for lo, depth := 0, 0; lo < len(queue); depth++ {
		hi := len(queue)
		level := queue[lo:hi]
		slices.Sort(level)
		bounds = append(bounds, hi)
		lo = hi
		if opts.MaxDepth >= 0 && depth >= opts.MaxDepth {
			break
		}
		for _, v := range level {
			if out {
				for i := g.offsets[v]; i < g.offsets[v+1]; i++ {
					if !opts.Labels.Has(g.edgeLabel[i]) {
						continue
					}
					if opts.WithEdges {
						res.Edges = append(res.Edges, uint32(i))
					}
					if w := g.targets[i]; !visited.has(w) {
						visited.set(w)
						queue = append(queue, w)
					}
				}
			}
			if in {
				for j := g.inOffsets[v]; j < g.inOffsets[v+1]; j++ {
					e := g.inEdge[j]
					if !opts.Labels.Has(g.edgeLabel[e]) {
						continue
					}
					if opts.WithEdges {
						res.Edges = append(res.Edges, e)
					}
					if w := g.inSources[j]; !visited.has(w) {
						visited.set(w)
						queue = append(queue, w)
					}
				}
			}
		}
	}
	// Levels share the final queue's backing array.
	res.Levels = make([][]uint32, len(bounds))
	prev := 0
	for i, hi := range bounds {
		res.Levels[i] = queue[prev:hi:hi]
		prev = hi
	}
	slices.Sort(res.Edges)
	res.Edges = slices.Compact(res.Edges)
	return res, nil
}
