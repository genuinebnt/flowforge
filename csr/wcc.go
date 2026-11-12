package csr

// WeaklyConnectedComponents clusters nodes into estates (spec 5.9
// components, 21.5): union-find with path halving and union by size over
// the undirected view, O(E α(V)). Groups are numbered in order of their
// smallest member.
func WeaklyConnectedComponents(g *Graph) Partition {
	nv := len(g.nodeID)
	parent := make([]uint32, nv)
	size := make([]uint32, nv)
	for i := range parent {
		parent[i] = uint32(i)
		size[i] = 1
	}
	find := func(x uint32) uint32 {
		for parent[x] != x {
			parent[x] = parent[parent[x]] // path halving
			x = parent[x]
		}
		return x
	}
	for v := range nv {
		for i := g.offsets[v]; i < g.offsets[v+1]; i++ {
			a, b := find(uint32(v)), find(g.targets[i])
			if a == b {
				continue
			}
			if size[a] < size[b] {
				a, b = b, a
			}
			parent[b] = a
			size[a] += size[b]
		}
	}
	// Number groups by first appearance, i.e. by smallest member.
	const unset = ^uint32(0)
	id := make([]uint32, nv) // root -> group id
	for i := range id {
		id[i] = unset
	}
	of := make([]uint32, nv)
	count := 0
	for v := range nv {
		r := find(uint32(v))
		if id[r] == unset {
			id[r] = uint32(count)
			count++
		}
		of[v] = id[r]
	}
	return partitionFrom(of, count)
}
