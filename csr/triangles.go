package csr

import "fmt"

// TriangleCount counts the triangles of the simple undirected view by
// sorted-row merge intersection (spec 21.5), O(E · dmax) worst case. Each
// triangle is counted once.
func TriangleCount(g *Graph) uint64 {
	u := g.undirectedView()
	var total uint64
	for a := range uint32(len(g.nodeID)) {
		ra := u.row(a)
		for _, b := range ra {
			if b <= a {
				continue
			}
			// Count c > b adjacent to both a and b.
			rb := u.row(b)
			i, j := 0, 0
			for i < len(ra) && j < len(rb) {
				switch x, y := ra[i], rb[j]; {
				case x < y:
					i++
				case x > y:
					j++
				default:
					if x > b {
						total++
					}
					i++
					j++
				}
			}
		}
	}
	return total
}

// CommonNeighbors returns the nodes adjacent to both a and b in the simple
// undirected view, ascending: the shared-infrastructure signal of
// spec 21.5.
func CommonNeighbors(g *Graph, a, b uint32) ([]uint32, error) {
	if !g.has(a) || !g.has(b) {
		return nil, fmt.Errorf("%w: common neighbours of %d and %d", ErrNodeRange, a, b)
	}
	u := g.undirectedView()
	ra, rb := u.row(a), u.row(b)
	out := []uint32{}
	i, j := 0, 0
	for i < len(ra) && j < len(rb) {
		switch x, y := ra[i], rb[j]; {
		case x < y:
			i++
		case x > y:
			j++
		default:
			out = append(out, x)
			i++
			j++
		}
	}
	return out, nil
}
