package csr

// undirected is the simple undirected view of a Graph: for every node the
// ascending, duplicate-free union of its out- and in-neighbours with self
// loops removed. Components, k-core, articulation points, bridges and
// triangle counting run on it.
type undirected struct {
	off []uint64
	adj []uint32
}

func (u *undirected) row(v uint32) []uint32 { return u.adj[u.off[v]:u.off[v+1]] }

// undirectedView builds the view once per Graph; it costs O(V + E) time
// and up to 8E + 8V bytes.
func (g *Graph) undirectedView() *undirected {
	g.undirOnce.Do(func() {
		nv := len(g.nodeID)
		u := &undirected{off: make([]uint64, nv+1), adj: make([]uint32, 0, 2*len(g.targets))}
		for v := range nv {
			out := g.targets[g.offsets[v]:g.offsets[v+1]]
			in := g.inSources[g.inOffsets[v]:g.inOffsets[v+1]]
			u.adj = mergeUnique(u.adj, out, in, uint32(v))
			u.off[v+1] = uint64(len(u.adj))
		}
		g.undir = u
	})
	return g.undir
}

// mergeUnique appends the sorted union of a and b (both ascending) to dst,
// dropping duplicates and the value skip.
func mergeUnique(dst, a, b []uint32, skip uint32) []uint32 {
	start := len(dst)
	push := func(x uint32) {
		if x == skip || (len(dst) > start && dst[len(dst)-1] == x) {
			return
		}
		dst = append(dst, x)
	}
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i] <= b[j] {
			push(a[i])
			i++
		} else {
			push(b[j])
			j++
		}
	}
	for ; i < len(a); i++ {
		push(a[i])
	}
	for ; j < len(b); j++ {
		push(b[j])
	}
	return dst
}

// bitset is a fixed-size set of node indices.
type bitset []uint64

func newBitset(n int) bitset { return make(bitset, (n+63)/64) }

func (b bitset) has(i uint32) bool { return b[i>>6]&(1<<(i&63)) != 0 }
func (b bitset) set(i uint32)      { b[i>>6] |= 1 << (i & 63) }
func (b bitset) clear(i uint32)    { b[i>>6] &^= 1 << (i & 63) }
