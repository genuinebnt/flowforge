package csr

import (
	"cmp"
	"slices"
)

// Bridge is an undirected edge {U, W}, U < W, whose removal disconnects
// its component.
type Bridge struct{ U, W uint32 }

// Cuts holds the articulation points and bridges of the simple undirected
// view, both ascending.
type Cuts struct {
	Points  []uint32
	Bridges []Bridge
}

// FindCuts finds articulation points (single assets whose loss disconnects
// an estate) and bridges with an iterative low-link DFS over the simple
// undirected view (spec 21.5), O(V + E). Parallel edges and the two
// directions of a 2-cycle collapse into one undirected edge, so such a
// pair can be a bridge.
func FindCuts(g *Graph) Cuts {
	u := g.undirectedView()
	nv := len(g.nodeID)
	const none = ^uint32(0)
	disc := make([]uint32, nv) // 0 = unvisited
	low := make([]uint32, nv)
	isCut := newBitset(nv)
	type frame struct {
		v, parent uint32
		next      uint64
		children  int
	}
	var frames []frame
	var res Cuts
	t := uint32(0)
	for root := range nv {
		if disc[root] != 0 {
			continue
		}
		t++
		disc[root], low[root] = t, t
		frames = append(frames[:0], frame{v: uint32(root), parent: none, next: u.off[root]})
		for len(frames) > 0 {
			f := &frames[len(frames)-1]
			v := f.v
			if f.next < u.off[v+1] {
				w := u.adj[f.next]
				f.next++
				switch {
				case w == f.parent:
					// The simple view has one edge to the parent; skip it.
				case disc[w] == 0:
					t++
					disc[w], low[w] = t, t
					frames = append(frames, frame{v: w, parent: v, next: u.off[w]})
				default:
					low[v] = min(low[v], disc[w])
				}
				continue
			}
			frames = frames[:len(frames)-1]
			if len(frames) == 0 {
				if f.children >= 2 {
					isCut.set(v)
				}
				continue
			}
			pf := &frames[len(frames)-1]
			p := pf.v
			pf.children++
			low[p] = min(low[p], low[v])
			if low[v] > disc[p] {
				res.Bridges = append(res.Bridges, Bridge{U: min(p, v), W: max(p, v)})
			}
			if pf.parent != none && low[v] >= disc[p] {
				isCut.set(p)
			}
		}
	}
	for v := range nv {
		if isCut.has(uint32(v)) {
			res.Points = append(res.Points, uint32(v))
		}
	}
	slices.SortFunc(res.Bridges, func(a, b Bridge) int {
		if c := cmp.Compare(a.U, b.U); c != 0 {
			return c
		}
		return cmp.Compare(a.W, b.W)
	})
	return res
}
