package csr

// Naive reference implementations over plain edge lists, used by the
// property tests and to generate the golden conformance files. They are
// deliberately simple (adjacency lists, matrices, brute force) so they
// share no code or structure with the CSR implementations.

import (
	"encoding/binary"
	"math"
	"slices"
	"sort"

	"github.com/genuinebnt/flowforge/core"
)

// nodeUUID returns a v7-shaped UUID whose byte order matches i, so Build
// keeps input index i as dense index i.
func nodeUUID(i int) core.UUID {
	var u core.UUID
	u[6], u[8] = 0x70, 0x80
	binary.BigEndian.PutUint32(u[12:], uint32(i))
	return u
}

func nodeUUIDs(n int) []core.UUID {
	out := make([]core.UUID, n)
	for i := range out {
		out[i] = nodeUUID(i)
	}
	return out
}

// naiveBFS returns hop distances (-1 unreached) using adjacency lists.
func naiveBFS(n int, edges []Edge, start uint32, dir Direction, maxDepth int, labels LabelSet) []int32 {
	adj := make([][]uint32, n)
	for _, e := range edges {
		if !labels.Has(e.Label) {
			continue
		}
		if dir == DirOut || dir == DirBoth {
			adj[e.Src] = append(adj[e.Src], e.Dst)
		}
		if dir == DirIn || dir == DirBoth {
			adj[e.Dst] = append(adj[e.Dst], e.Src)
		}
	}
	dist := make([]int32, n)
	for i := range dist {
		dist[i] = -1
	}
	dist[start] = 0
	q := []uint32{start}
	for len(q) > 0 {
		v := q[0]
		q = q[1:]
		if maxDepth >= 0 && int(dist[v]) >= maxDepth {
			continue
		}
		for _, w := range adj[v] {
			if dist[w] < 0 {
				dist[w] = dist[v] + 1
				q = append(q, w)
			}
		}
	}
	return dist
}

// levelsFromDist groups nodes by distance, ascending.
func levelsFromDist(dist []int32) [][]uint32 {
	var levels [][]uint32
	for v, d := range dist {
		if d < 0 {
			continue
		}
		for int(d) >= len(levels) {
			levels = append(levels, nil)
		}
		levels[d] = append(levels[d], uint32(v))
	}
	return levels
}

func reachability(n int, edges []Edge) [][]bool {
	reach := make([][]bool, n)
	for s := range n {
		d := naiveBFS(n, edges, uint32(s), DirOut, -1, AnyLabel())
		reach[s] = make([]bool, n)
		for v, x := range d {
			reach[s][v] = x >= 0
		}
	}
	return reach
}

// canonicalGroups sorts each group and orders groups by first member.
func canonicalGroups(groups [][]uint32) [][]uint32 {
	out := make([][]uint32, 0, len(groups))
	for _, g := range groups {
		c := slices.Clone(g)
		slices.Sort(c)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// naiveSCC groups mutually reachable nodes.
func naiveSCC(n int, edges []Edge) [][]uint32 {
	reach := reachability(n, edges)
	assigned := make([]bool, n)
	var groups [][]uint32
	for a := range n {
		if assigned[a] {
			continue
		}
		var g []uint32
		for b := a; b < n; b++ {
			if !assigned[b] && reach[a][b] && reach[b][a] {
				assigned[b] = true
				g = append(g, uint32(b))
			}
		}
		groups = append(groups, g)
	}
	return canonicalGroups(groups)
}

func naiveWCC(n int, edges []Edge) [][]uint32 {
	assigned := make([]bool, n)
	var groups [][]uint32
	for s := range n {
		if assigned[s] {
			continue
		}
		var g []uint32
		for v, d := range naiveBFS(n, edges, uint32(s), DirBoth, -1, AnyLabel()) {
			if d >= 0 {
				assigned[v] = true
				g = append(g, uint32(v))
			}
		}
		groups = append(groups, g)
	}
	return canonicalGroups(groups)
}

// simpleUndirected returns adjacency sets without self loops.
func simpleUndirected(n int, edges []Edge) []map[uint32]bool {
	adj := make([]map[uint32]bool, n)
	for i := range adj {
		adj[i] = map[uint32]bool{}
	}
	for _, e := range edges {
		if e.Src != e.Dst {
			adj[e.Src][e.Dst] = true
			adj[e.Dst][e.Src] = true
		}
	}
	return adj
}

func naiveCoreNumbers(n int, edges []Edge) []uint32 {
	adj := simpleUndirected(n, edges)
	core := make([]uint32, n)
	for k := 1; k <= n; k++ {
		alive := make([]bool, n)
		for i := range alive {
			alive[i] = true
		}
		for changed := true; changed; {
			changed = false
			for v := range n {
				if !alive[v] {
					continue
				}
				deg := 0
				for w := range adj[v] {
					if alive[w] {
						deg++
					}
				}
				if deg < k {
					alive[v] = false
					changed = true
				}
			}
		}
		any := false
		for v := range n {
			if alive[v] {
				core[v] = uint32(k)
				any = true
			}
		}
		if !any {
			break
		}
	}
	return core
}

// connectedWithout reports whether b is reachable from a in the simple
// undirected graph with node skip (if >= 0) and edge {ea, eb} removed.
func connectedWithout(adj []map[uint32]bool, a, b uint32, skip int, ea, eb uint32, dropEdge bool) bool {
	seen := map[uint32]bool{a: true}
	q := []uint32{a}
	for len(q) > 0 {
		v := q[0]
		q = q[1:]
		if v == b {
			return true
		}
		for w := range adj[v] {
			if int(w) == skip || seen[w] {
				continue
			}
			if dropEdge && ((v == ea && w == eb) || (v == eb && w == ea)) {
				continue
			}
			seen[w] = true
			q = append(q, w)
		}
	}
	return false
}

func naiveCuts(n int, edges []Edge) ([]uint32, []Bridge) {
	adj := simpleUndirected(n, edges)
	points := []uint32{}
	for v := range n {
		var nb []uint32
		for w := range adj[v] {
			nb = append(nb, w)
		}
		slices.Sort(nb)
		for _, w := range nb[min(1, len(nb)):] {
			if !connectedWithout(adj, nb[0], w, v, 0, 0, false) {
				points = append(points, uint32(v))
				break
			}
		}
	}
	bridges := []Bridge{}
	for a := range n {
		for b := range adj[a] {
			if uint32(a) < b && !connectedWithout(adj, uint32(a), b, -1, uint32(a), b, true) {
				bridges = append(bridges, Bridge{U: uint32(a), W: b})
			}
		}
	}
	slices.SortFunc(bridges, func(x, y Bridge) int {
		if x.U != y.U {
			return int(x.U) - int(y.U)
		}
		return int(x.W) - int(y.W)
	})
	return points, bridges
}

func naiveTriangles(n int, edges []Edge) uint64 {
	adj := simpleUndirected(n, edges)
	var t uint64
	for a := range n {
		for b := a + 1; b < n; b++ {
			if !adj[a][uint32(b)] {
				continue
			}
			for c := b + 1; c < n; c++ {
				if adj[a][uint32(c)] && adj[b][uint32(c)] {
					t++
				}
			}
		}
	}
	return t
}

// densePageRank is the dense-matrix power iteration reference: M[v][u] is
// the fraction of u's out-edges that lead to v; dangling mass is spread
// uniformly. It iterates to a much tighter tolerance than the default.
func densePageRank(n int, edges []Edge, d float64) []float64 {
	if n == 0 {
		return []float64{}
	}
	out := make([]float64, n)
	for _, e := range edges {
		out[e.Src]++
	}
	m := make([][]float64, n)
	for i := range m {
		m[i] = make([]float64, n)
	}
	for _, e := range edges {
		m[e.Dst][e.Src] += 1 / out[e.Src]
	}
	r := make([]float64, n)
	for i := range r {
		r[i] = 1 / float64(n)
	}
	for range 10000 {
		dangling := 0.0
		for u := range n {
			if out[u] == 0 {
				dangling += r[u]
			}
		}
		next := make([]float64, n)
		delta := 0.0
		for v := range n {
			s := 0.0
			for u := range n {
				s += m[v][u] * r[u]
			}
			next[v] = (1-d)/float64(n) + d*(s+dangling/float64(n))
			delta += math.Abs(next[v] - r[v])
		}
		r = next
		if delta < 1e-14 {
			break
		}
	}
	return r
}

// naiveBestConfidence runs Bellman-Ford over -ln(confidence) costs.
func naiveBestConfidence(n int, edges []Edge, from, to uint32, labels LabelSet) (float64, bool) {
	dist := make([]float64, n)
	for i := range dist {
		dist[i] = math.Inf(1)
	}
	dist[from] = 0
	for range n {
		for _, e := range edges {
			if e.Conf <= 0 || !labels.Has(e.Label) {
				continue
			}
			if c := dist[e.Src] - math.Log(float64(e.Conf)); c < dist[e.Dst] {
				dist[e.Dst] = c
			}
		}
	}
	if math.IsInf(dist[to], 1) {
		return 0, false
	}
	return math.Exp(-dist[to]), true
}
