package diff

import (
	"fmt"
	"strings"
)

// NodeID identifies a node of an arena tree; -1 means none.
type NodeID = int32

// NoNode is the absent node.
const NoNode NodeID = -1

// Tree is an ordered, labelled tree stored in an arena (a DOM, an AST):
// nodes are int32 ids, children are reached through first-child and
// next-sibling links, labels are interned uint32 ids. Root returns NoNode
// for an empty tree.
type Tree interface {
	Root() NodeID
	Label(n NodeID) uint32
	FirstChild(n NodeID) NodeID
	NextSibling(n NodeID) NodeID
}

// ArenaTree is a concrete Tree. The zero value is an empty tree.
type ArenaTree struct {
	Nodes []ArenaNode
}

// ArenaNode is one node of an ArenaTree.
type ArenaNode struct {
	Label                           uint32
	Parent, FirstChild, NextSibling NodeID
	lastChild                       NodeID
}

// Add appends a node labelled label as the last child of parent (NoNode
// for the root, which must be added first and only once) and returns its
// id. It returns NoNode for an invalid parent or a second root.
func (t *ArenaTree) Add(parent NodeID, label uint32) NodeID {
	if parent == NoNode && len(t.Nodes) > 0 {
		return NoNode
	}
	if parent != NoNode && (parent < 0 || int(parent) >= len(t.Nodes)) {
		return NoNode
	}
	id := NodeID(len(t.Nodes))
	t.Nodes = append(t.Nodes, ArenaNode{Label: label, Parent: parent, FirstChild: NoNode, NextSibling: NoNode, lastChild: NoNode})
	if parent != NoNode {
		p := &t.Nodes[parent]
		if p.lastChild == NoNode {
			p.FirstChild = id
		} else {
			t.Nodes[p.lastChild].NextSibling = id
		}
		p.lastChild = id
	}
	return id
}

// Root implements Tree.
func (t *ArenaTree) Root() NodeID {
	if len(t.Nodes) == 0 {
		return NoNode
	}
	return 0
}

func (t *ArenaTree) valid(n NodeID) bool { return n >= 0 && int(n) < len(t.Nodes) }

// Label implements Tree.
func (t *ArenaTree) Label(n NodeID) uint32 {
	if !t.valid(n) {
		return NoToken
	}
	return t.Nodes[n].Label
}

// FirstChild implements Tree.
func (t *ArenaTree) FirstChild(n NodeID) NodeID {
	if !t.valid(n) {
		return NoNode
	}
	return t.Nodes[n].FirstChild
}

// NextSibling implements Tree.
func (t *ArenaTree) NextSibling(n NodeID) NodeID {
	if !t.valid(n) {
		return NoNode
	}
	return t.Nodes[n].NextSibling
}

// ParseBracket parses bracket notation ("{a{b}{c}}": a node is "{" label
// children "}", a label is any text without braces; "" is the empty tree),
// interning labels with in. Errors wrap ErrMalformedTree.
func ParseBracket(s string, in *Interner) (*ArenaTree, error) {
	t := &ArenaTree{}
	if s == "" {
		return t, nil
	}
	stack := []NodeID{NoNode}
	i := 0
	for i < len(s) {
		switch s[i] {
		case '{':
			j := i + 1
			for j < len(s) && s[j] != '{' && s[j] != '}' {
				j++
			}
			if len(stack) == 1 && len(t.Nodes) > 0 {
				return nil, fmt.Errorf("%w: second root at byte %d", ErrMalformedTree, i)
			}
			id := t.Add(stack[len(stack)-1], in.Intern(s[i+1:j]))
			stack = append(stack, id)
			i = j
		case '}':
			if len(stack) == 1 {
				return nil, fmt.Errorf("%w: unbalanced '}' at byte %d", ErrMalformedTree, i)
			}
			stack = stack[:len(stack)-1]
			i++
		default:
			return nil, fmt.Errorf("%w: unexpected %q at byte %d", ErrMalformedTree, s[i], i)
		}
	}
	if len(stack) != 1 {
		return nil, fmt.Errorf("%w: unclosed '{'", ErrMalformedTree)
	}
	return t, nil
}

// FormatBracket renders t in bracket notation using in for labels.
func FormatBracket(t Tree, in *Interner) string {
	var b strings.Builder
	var rec func(n NodeID, depth int)
	rec = func(n NodeID, depth int) {
		if depth > 1<<16 {
			return
		}
		l, _ := in.Value(t.Label(n))
		b.WriteString("{" + l)
		for c := t.FirstChild(n); c != NoNode; c = t.NextSibling(c) {
			rec(c, depth+1)
		}
		b.WriteString("}")
	}
	if r := t.Root(); r != NoNode {
		rec(r, 0)
	}
	return b.String()
}

// DefaultMaxTreeNodes bounds each tree of TreeDiff when TreeOptions.MaxNodes
// is zero. The algorithm needs O(n1*n2) memory.
const DefaultMaxTreeNodes = 2000

// TreeCosts are the edit operation costs; all zero means unit costs
// (insert 1, delete 1, relabel 1).
type TreeCosts struct {
	Insert, Delete, Relabel int
}

// TreeOptions configures TreeDiff.
type TreeOptions struct {
	// MaxNodes bounds the size of each tree (0: DefaultMaxTreeNodes).
	MaxNodes int
	// Costs are the operation costs (zero value: unit costs).
	Costs TreeCosts
}

// NodePair maps a node of the old tree to a node of the new tree.
type NodePair struct {
	Old, New NodeID
}

// TreeResult is a tree edit distance with an optimal edit mapping.
type TreeResult struct {
	// Distance is the minimum total cost of node deletions, insertions and
	// relabellings turning the old tree into the new one.
	Distance int
	// Mapping pairs kept nodes (relabelled when labels differ), in old-tree
	// postorder. It is a valid Tai mapping: one-to-one, preserving
	// ancestor and left-to-right order.
	Mapping []NodePair
	// Deleted and Inserted are the unmapped old and new nodes, in postorder.
	Deleted, Inserted []NodeID
}

// postTree is a tree flattened in postorder.
type postTree struct {
	ids   []NodeID // postorder index -> node id
	label []uint32
	lml   []int // leftmost leaf descendant (postorder index)
	keys  []int // keyroots, ascending
}

// flatten traverses t iteratively, failing with ErrTreeTooLarge beyond
// maxNodes nodes (which also stops a cyclic Tree implementation).
func flatten(t Tree, maxNodes int) (postTree, error) {
	var p postTree
	root := t.Root()
	if root == NoNode {
		return p, nil
	}
	type frame struct {
		n        NodeID
		next     NodeID // next child to visit
		firstIdx int    // postorder index of the first child, -1 none yet
	}
	stack := []frame{{n: root, next: t.FirstChild(root), firstIdx: -1}}
	visited := 1
	for len(stack) > 0 {
		f := &stack[len(stack)-1]
		if f.next != NoNode {
			c := f.next
			f.next = t.NextSibling(c)
			visited++
			if visited > maxNodes {
				return postTree{}, fmt.Errorf("%w: more than %d nodes", ErrTreeTooLarge, maxNodes)
			}
			stack = append(stack, frame{n: c, next: t.FirstChild(c), firstIdx: -1})
			continue
		}
		idx := len(p.ids)
		lml := idx
		if f.firstIdx >= 0 {
			lml = p.lml[f.firstIdx]
		}
		p.ids = append(p.ids, f.n)
		p.label = append(p.label, t.Label(f.n))
		p.lml = append(p.lml, lml)
		stack = stack[:len(stack)-1]
		if len(stack) > 0 && stack[len(stack)-1].firstIdx < 0 {
			stack[len(stack)-1].firstIdx = idx
		}
	}
	// Keyroots: the highest node of each leftmost-leaf class.
	seen := make([]bool, len(p.ids))
	for i := len(p.ids) - 1; i >= 0; i-- {
		if !seen[p.lml[i]] {
			seen[p.lml[i]] = true
			p.keys = append(p.keys, i)
		}
	}
	for i, j := 0, len(p.keys)-1; i < j; i, j = i+1, j-1 {
		p.keys[i], p.keys[j] = p.keys[j], p.keys[i]
	}
	return p, nil
}

// zs holds the Zhang–Shasha tables.
type zs struct {
	t1, t2        postTree
	ins, del, ren int
	td            []int // tree distances, n1 x n2
	fd            []int // forest distance scratch, (n1+1) x (n2+1)
	w             int   // row width of fd
}

func (z *zs) relabel(x, y int) int {
	if z.t1.label[x] == z.t2.label[y] {
		return 0
	}
	return z.ren
}

// forest fills fd for the subtree pair (i, j) and records tree distances
// of pairs on both leftmost paths (Zhang & Shasha 1989, treedist).
func (z *zs) forest(i, j int) {
	l1, l2 := z.t1.lml, z.t2.lml
	li, lj := l1[i], l2[j]
	n2 := len(z.t2.ids)
	w := z.w
	fd := z.fd
	fd[0] = 0
	for x := li; x <= i; x++ {
		fd[(x-li+1)*w] = fd[(x-li)*w] + z.del
	}
	for y := lj; y <= j; y++ {
		fd[y-lj+1] = fd[y-lj] + z.ins
	}
	for x := li; x <= i; x++ {
		dx := x - li + 1
		for y := lj; y <= j; y++ {
			dy := y - lj + 1
			best := fd[(dx-1)*w+dy] + z.del
			if v := fd[dx*w+dy-1] + z.ins; v < best {
				best = v
			}
			if l1[x] == li && l2[y] == lj {
				if v := fd[(dx-1)*w+dy-1] + z.relabel(x, y); v < best {
					best = v
				}
				z.td[x*n2+y] = best
			} else if v := fd[(l1[x]-li)*w+(l2[y]-lj)] + z.td[x*n2+y]; v < best {
				best = v
			}
			fd[dx*w+dy] = best
		}
	}
}

// TreeDiff computes the Zhang–Shasha tree edit distance between two
// ordered labelled trees with an optimal mapping. Time is
// O(n1*n2*min(depth1, leaves1)*min(depth2, leaves2)), memory O(n1*n2).
// Trees larger than MaxNodes fail with ErrTreeTooLarge.
func TreeDiff(oldTree, newTree Tree, opts TreeOptions) (TreeResult, error) {
	maxNodes := opts.MaxNodes
	if maxNodes <= 0 {
		maxNodes = DefaultMaxTreeNodes
	}
	c := opts.Costs
	if c == (TreeCosts{}) {
		c = TreeCosts{Insert: 1, Delete: 1, Relabel: 1}
	}
	if c.Insert < 0 || c.Delete < 0 || c.Relabel < 0 {
		return TreeResult{}, fmt.Errorf("%w: negative tree costs", ErrInvalidOptions)
	}
	t1, err := flatten(oldTree, maxNodes)
	if err != nil {
		return TreeResult{}, err
	}
	t2, err := flatten(newTree, maxNodes)
	if err != nil {
		return TreeResult{}, err
	}
	n1, n2 := len(t1.ids), len(t2.ids)
	var res TreeResult
	if n1 == 0 || n2 == 0 {
		res.Distance = n1*c.Delete + n2*c.Insert
		res.Deleted = append(res.Deleted, t1.ids...)
		res.Inserted = append(res.Inserted, t2.ids...)
		return res, nil
	}
	z := &zs{t1: t1, t2: t2, ins: c.Insert, del: c.Delete, ren: c.Relabel,
		td: make([]int, n1*n2), fd: make([]int, (n1+1)*(n2+1)), w: n2 + 1}
	for _, i := range t1.keys {
		for _, j := range t2.keys {
			z.forest(i, j)
		}
	}
	res.Distance = z.td[(n1-1)*n2+n2-1]
	// Backtrack: resolve subtree pairs from a stack, recomputing the forest
	// table of each and tracing one optimal path through it.
	mapped1 := make([]bool, n1)
	mapped2 := make([]bool, n2)
	pairs := [][2]int{{n1 - 1, n2 - 1}}
	var mapping [][2]int
	for len(pairs) > 0 {
		pr := pairs[len(pairs)-1]
		pairs = pairs[:len(pairs)-1]
		i, j := pr[0], pr[1]
		z.forest(i, j)
		li, lj := t1.lml[i], t2.lml[j]
		w := z.w
		x, y := i, j
		for x >= li || y >= lj {
			dx, dy := x-li+1, y-lj+1
			cur := z.fd[dx*w+dy]
			switch {
			case x >= li && cur == z.fd[(dx-1)*w+dy]+z.del:
				x--
			case y >= lj && cur == z.fd[dx*w+dy-1]+z.ins:
				y--
			case x >= li && y >= lj && t1.lml[x] == li && t2.lml[y] == lj:
				mapping = append(mapping, [2]int{x, y})
				mapped1[x], mapped2[y] = true, true
				x, y = x-1, y-1
			case x >= li && y >= lj:
				pairs = append(pairs, [2]int{x, y})
				x, y = t1.lml[x]-1, t2.lml[y]-1
			default:
				return TreeResult{}, fmt.Errorf("%w: inconsistent tree distance table", ErrInvalidOptions)
			}
		}
	}
	// Order the mapping by old postorder.
	byOld := make([]int, n1)
	for i := range byOld {
		byOld[i] = -1
	}
	for _, m := range mapping {
		byOld[m[0]] = m[1]
	}
	for i, j := range byOld {
		if j >= 0 {
			res.Mapping = append(res.Mapping, NodePair{Old: t1.ids[i], New: t2.ids[j]})
		}
	}
	for i, m := range mapped1 {
		if !m {
			res.Deleted = append(res.Deleted, t1.ids[i])
		}
	}
	for j, m := range mapped2 {
		if !m {
			res.Inserted = append(res.Inserted, t2.ids[j])
		}
	}
	return res, nil
}
