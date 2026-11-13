package diff

import (
	"fmt"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// randTree builds a random ordered tree of n nodes: each node becomes the
// last child of a random earlier node; labels come from alpha labels.
func randTree(r core.Rng, n, alpha int) *ArenaTree {
	t := &ArenaTree{}
	for i := range n {
		parent := NoNode
		if i > 0 {
			parent = NodeID(core.IntN(r, i))
		}
		t.Add(parent, uint32(core.IntN(r, alpha)))
	}
	return t
}

// treeKey renders a tree with numeric labels.
func treeKey(t Tree) string {
	in := NewInterner()
	for i := range 64 {
		in.Intern(fmt.Sprint(i))
	}
	return FormatBracket(t, in)
}

// treeInfo holds postorder numbers and ancestor relations for the oracle.
type treeInfo struct {
	post   map[NodeID]int
	anc    map[[2]NodeID]bool // anc[{u, v}]: u is a proper ancestor of v
	nodes  []NodeID           // in postorder
	labels map[NodeID]uint32
}

func infoOf(t Tree) treeInfo {
	ti := treeInfo{post: map[NodeID]int{}, anc: map[[2]NodeID]bool{}, labels: map[NodeID]uint32{}}
	var rec func(n NodeID, path []NodeID)
	rec = func(n NodeID, path []NodeID) {
		for _, a := range path {
			ti.anc[[2]NodeID{a, n}] = true
		}
		for c := t.FirstChild(n); c != NoNode; c = t.NextSibling(c) {
			rec(c, append(path, n))
		}
		ti.post[n] = len(ti.nodes)
		ti.nodes = append(ti.nodes, n)
		ti.labels[n] = t.Label(n)
	}
	if r := t.Root(); r != NoNode {
		rec(r, nil)
	}
	return ti
}

// compatible reports whether pairs (v1, w1) and (v2, w2) may both be in a
// Tai mapping: one-to-one, ancestor order and left-to-right (postorder)
// order preserved.
func compatible(t1, t2 treeInfo, v1, w1, v2, w2 NodeID) bool {
	if (v1 == v2) != (w1 == w2) {
		return false
	}
	if v1 == v2 {
		return true
	}
	if t1.anc[[2]NodeID{v1, v2}] != t2.anc[[2]NodeID{w1, w2}] || t1.anc[[2]NodeID{v2, v1}] != t2.anc[[2]NodeID{w2, w1}] {
		return false
	}
	return (t1.post[v1] < t1.post[v2]) == (t2.post[w1] < t2.post[w2])
}

// bruteTreeDistance is the oracle: the minimum cost over every valid Tai
// mapping, by exhaustive search (tiny trees only).
func bruteTreeDistance(a, b Tree, c TreeCosts) int {
	t1, t2 := infoOf(a), infoOf(b)
	best := len(t1.nodes)*c.Delete + len(t2.nodes)*c.Insert
	var pairs [][2]NodeID
	used := map[NodeID]bool{}
	var rec func(i, cost int)
	rec = func(i, cost int) {
		if i == len(t1.nodes) {
			total := cost + (len(t1.nodes)-len(pairs))*c.Delete + (len(t2.nodes)-len(pairs))*c.Insert
			best = min(best, total)
			return
		}
		rec(i+1, cost) // v unmapped
		v := t1.nodes[i]
		for _, w := range t2.nodes {
			if used[w] {
				continue
			}
			ok := true
			for _, p := range pairs {
				if !compatible(t1, t2, v, w, p[0], p[1]) {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			ren := 0
			if t1.labels[v] != t2.labels[w] {
				ren = c.Relabel
			}
			used[w] = true
			pairs = append(pairs, [2]NodeID{v, w})
			rec(i+1, cost+ren)
			pairs = pairs[:len(pairs)-1]
			used[w] = false
		}
	}
	rec(0, 0)
	return best
}

func mustTreeDiff(t testing.TB, a, b Tree, opts TreeOptions) TreeResult {
	t.Helper()
	res, err := TreeDiff(a, b, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// checkMapping verifies that res is a valid Tai mapping whose cost is
// res.Distance and whose Deleted/Inserted lists are the unmapped nodes.
func checkMapping(t testing.TB, a, b Tree, res TreeResult, c TreeCosts) {
	t.Helper()
	t1, t2 := infoOf(a), infoOf(b)
	cost := 0
	for i, p := range res.Mapping {
		for _, q := range res.Mapping[:i] {
			if !compatible(t1, t2, p.Old, p.New, q.Old, q.New) {
				t.Fatalf("mapping pairs %v and %v are incompatible", p, q)
			}
		}
		if t1.labels[p.Old] != t2.labels[p.New] {
			cost += c.Relabel
		}
	}
	if len(res.Mapping)+len(res.Deleted) != len(t1.nodes) || len(res.Mapping)+len(res.Inserted) != len(t2.nodes) {
		t.Fatalf("mapping %d + deleted %d / inserted %d do not cover %d / %d nodes", len(res.Mapping), len(res.Deleted), len(res.Inserted), len(t1.nodes), len(t2.nodes))
	}
	cost += len(res.Deleted)*c.Delete + len(res.Inserted)*c.Insert
	if cost != res.Distance {
		t.Fatalf("mapping costs %d, distance %d", cost, res.Distance)
	}
}

// DIFF-INV-11: tree edit distance is a metric on random trees (unit
// costs): d(x, x) = 0, d(x, y) = 0 only for identical trees, d(x, y) =
// d(y, x), d(x, z) <= d(x, y) + d(y, z); the returned mapping always
// realises the distance.
func TestDiffInv11_TreeMetric(t *testing.T) {
	r := testkit.Rng(t)
	unit := TreeCosts{Insert: 1, Delete: 1, Relabel: 1}
	for range testkit.Cases() / 4 {
		x := randTree(r, core.IntN(r, 12), 3)
		y := randTree(r, core.IntN(r, 12), 3)
		z := randTree(r, core.IntN(r, 12), 3)
		dxy := mustTreeDiff(t, x, y, TreeOptions{})
		dyx := mustTreeDiff(t, y, x, TreeOptions{})
		dyz := mustTreeDiff(t, y, z, TreeOptions{})
		dxz := mustTreeDiff(t, x, z, TreeOptions{})
		if d := mustTreeDiff(t, x, x, TreeOptions{}).Distance; d != 0 {
			t.Fatalf("d(x, x) = %d", d)
		}
		if (dxy.Distance == 0) != (treeKey(x) == treeKey(y)) {
			t.Fatalf("d = %d for %s / %s", dxy.Distance, treeKey(x), treeKey(y))
		}
		if dxy.Distance != dyx.Distance {
			t.Fatalf("asymmetric: %d vs %d", dxy.Distance, dyx.Distance)
		}
		if dxz.Distance > dxy.Distance+dyz.Distance {
			t.Fatalf("triangle: %d > %d + %d", dxz.Distance, dxy.Distance, dyz.Distance)
		}
		checkMapping(t, x, y, dxy, unit)
	}
}

// DIFF-INV-12: on tiny trees the Zhang–Shasha distance equals an
// exhaustive search over all Tai mappings, for unit and non-unit costs,
// and the returned mapping is valid and realises it.
func TestDiffInv12_TreeBruteForce(t *testing.T) {
	r := testkit.Rng(t)
	for range testkit.Cases() / 4 {
		a := randTree(r, core.IntN(r, 7), 1+core.IntN(r, 3))
		b := randTree(r, core.IntN(r, 7), 1+core.IntN(r, 3))
		c := TreeCosts{Insert: 1, Delete: 1, Relabel: 1}
		if core.IntN(r, 2) == 0 {
			c = TreeCosts{Insert: 1 + core.IntN(r, 3), Delete: 1 + core.IntN(r, 3), Relabel: core.IntN(r, 5)}
			if c == (TreeCosts{}) {
				c.Insert = 1
			}
		}
		res := mustTreeDiff(t, a, b, TreeOptions{Costs: c})
		if want := bruteTreeDistance(a, b, c); res.Distance != want {
			t.Fatalf("Zhang–Shasha %d, brute force %d (costs %+v)", res.Distance, want, c)
		}
		checkMapping(t, a, b, res, c)
	}
}

// TestTreeLimits pins the size cap and malformed inputs.
func TestTreeLimits(t *testing.T) {
	r := testkit.Rng(t)
	big := randTree(r, 50, 3)
	if _, err := TreeDiff(big, big, TreeOptions{MaxNodes: 49}); err == nil {
		t.Fatal("size cap not enforced")
	}
	cyc := &ArenaTree{}
	cyc.Add(NoNode, 1)
	cyc.Add(0, 2)
	cyc.Nodes[1].NextSibling = 1 // corrupt: a sibling cycle
	if _, err := TreeDiff(cyc, cyc, TreeOptions{MaxNodes: 100}); err == nil {
		t.Fatal("cyclic tree accepted")
	}
	in := NewInterner()
	for _, s := range []string{"{", "}", "{a}{b}", "{a}}", "x"} {
		if _, err := ParseBracket(s, in); err == nil {
			t.Errorf("ParseBracket(%q) accepted", s)
		}
	}
}
