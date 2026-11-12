package csr

import "slices"

// StronglyConnectedComponents finds the mutually reachable groups with an
// iterative Tarjan (spec 21.5): an explicit DFS stack replaces recursion,
// so a path of millions of nodes cannot overflow the goroutine stack.
// O(V + E).
//
// Groups are returned in reverse topological order of the condensation:
// if an edge leads from group a to a different group b, then b < a (sinks
// come first). Roots are tried in ascending node order and rows are
// scanned in CSR order, so the output is deterministic.
func StronglyConnectedComponents(g *Graph) Partition {
	nv := len(g.nodeID)
	index := make([]uint32, nv) // 0 = unvisited, else DFS index + 1
	low := make([]uint32, nv)
	onStack := newBitset(nv)
	var stack []uint32
	type frame struct {
		v    uint32
		next uint64 // next forward edge to scan
	}
	var frames []frame
	of := make([]uint32, nv)
	members := make([]uint32, 0, nv)
	starts := []int{0}
	counter := uint32(0)
	visit := func(v uint32) {
		counter++
		index[v], low[v] = counter, counter
		stack = append(stack, v)
		onStack.set(v)
		frames = append(frames, frame{v: v, next: g.offsets[v]})
	}
	for root := range nv {
		if index[root] != 0 {
			continue
		}
		visit(uint32(root))
		for len(frames) > 0 {
			f := &frames[len(frames)-1]
			v := f.v
			if f.next < g.offsets[v+1] {
				w := g.targets[f.next]
				f.next++
				if index[w] == 0 {
					visit(w)
				} else if onStack.has(w) {
					low[v] = min(low[v], index[w])
				}
				continue
			}
			frames = frames[:len(frames)-1]
			if len(frames) > 0 {
				p := frames[len(frames)-1].v
				low[p] = min(low[p], low[v])
			}
			if low[v] != index[v] {
				continue
			}
			comp := uint32(len(starts) - 1)
			begin := len(members)
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack.clear(w)
				of[w] = comp
				members = append(members, w)
				if w == v {
					break
				}
			}
			slices.Sort(members[begin:])
			starts = append(starts, len(members))
		}
	}
	return Partition{Of: of, Starts: starts, Members: members}
}
