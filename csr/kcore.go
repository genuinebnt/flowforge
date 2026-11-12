package csr

// CoreNumbers returns the core number of every node of the simple
// undirected view: the largest k such that the node belongs to a subgraph
// in which every node has degree at least k. It uses the bucketed
// degree-peeling algorithm of Batagelj and Zaversnik, O(V + E).
func CoreNumbers(g *Graph) []uint32 {
	u := g.undirectedView()
	nv := len(g.nodeID)
	deg := make([]uint32, nv)
	maxDeg := uint32(0)
	for v := range nv {
		deg[v] = uint32(u.off[v+1] - u.off[v])
		maxDeg = max(maxDeg, deg[v])
	}
	// bin[d] = first position in vert of nodes with degree d.
	bin := make([]uint32, maxDeg+2)
	for _, d := range deg {
		bin[d+1]++
	}
	for d := uint32(1); d <= maxDeg+1; d++ {
		bin[d] += bin[d-1]
	}
	pos := make([]uint32, nv)
	vert := make([]uint32, nv)
	next := make([]uint32, maxDeg+1)
	copy(next, bin[:maxDeg+1])
	for v := range nv {
		d := deg[v]
		pos[v] = next[d]
		vert[pos[v]] = uint32(v)
		next[d]++
	}
	for i := range nv {
		v := vert[i]
		for _, w := range u.row(v) {
			if deg[w] <= deg[v] {
				continue
			}
			dw := deg[w]
			pw, pf := pos[w], bin[dw]
			first := vert[pf]
			if first != w {
				vert[pw], vert[pf] = first, w
				pos[first], pos[w] = pw, pf
			}
			bin[dw]++
			deg[w]--
		}
	}
	return deg
}

// KCore returns the nodes whose core number is at least k, ascending.
func KCore(g *Graph, k uint32) []uint32 {
	var out []uint32
	for v, c := range CoreNumbers(g) {
		if c >= k {
			out = append(out, uint32(v))
		}
	}
	return out
}
