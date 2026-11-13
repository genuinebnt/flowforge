package jsparse

import (
	"fmt"
)

// checkArena verifies JS-INV-06 on prog: every child id is in range and
// not the node itself, list ranges lie inside Children, NoNode appears in
// lists only as array holes, no node has two parents, every child span
// lies within its parent span, and (when reachable) every node is
// reachable from the root. It returns the first violation.
func checkArena(prog *Program, reachable bool) error {
	n := len(prog.Nodes)
	if prog.Root < 0 || int(prog.Root) >= n || prog.Nodes[prog.Root].Kind != KProgram {
		return fmt.Errorf("bad root %d", prog.Root)
	}
	parent := make([]NodeID, n)
	for i := range parent {
		parent[i] = NoNode
	}
	link := func(p NodeID, c NodeID) error {
		if c < 0 || int(c) >= n {
			return fmt.Errorf("node %d (%v): child id %d out of range", p, prog.Nodes[p].Kind, c)
		}
		if c == p {
			return fmt.Errorf("node %d is its own child", p)
		}
		if parent[c] != NoNode {
			return fmt.Errorf("node %d (%v) has two parents %d and %d", c, prog.Nodes[c].Kind, parent[c], p)
		}
		parent[c] = p
		ps, cs := prog.Nodes[p].Span, prog.Nodes[c].Span
		if !ps.Contains(cs) || cs.Start > cs.End {
			return fmt.Errorf("node %d (%v %v) not within parent %d (%v %v)", c, prog.Nodes[c].Kind, cs, p, prog.Nodes[p].Kind, ps)
		}
		return nil
	}
	for i := range prog.Nodes {
		id := NodeID(i)
		nd := &prog.Nodes[i]
		if nd.Kind == KInvalid || nd.Kind >= kindCount {
			return fmt.Errorf("node %d has invalid kind", i)
		}
		if nd.Span.End > int32(len(prog.Source)) || nd.Span.Start < 0 {
			return fmt.Errorf("node %d span %v outside source", i, nd.Span)
		}
		for _, f := range schemas[nd.Kind] {
			switch f.kind {
			case fNode:
				if c := slot(nd, f.slot); c != NoNode {
					if err := link(id, c); err != nil {
						return err
					}
				}
			case fList:
				l := nd.List
				if f.slot == 1 {
					l = nd.List2
				}
				if l.Start < 0 || l.Len < 0 || int(l.Start+l.Len) > len(prog.Children) {
					return fmt.Errorf("node %d: list %v out of range", i, l)
				}
				for _, c := range prog.List(l) {
					if c == NoNode {
						if nd.Kind != KArrayExpression && nd.Kind != KArrayPattern {
							return fmt.Errorf("node %d (%v): hole in %s", i, nd.Kind, f.name)
						}
						continue
					}
					if err := link(id, c); err != nil {
						return err
					}
				}
			}
		}
	}
	if reachable {
		count := 0
		prog.Walk(prog.Root, func(NodeID) bool { count++; return true })
		if count != n {
			for i := range prog.Nodes {
				if parent[i] == NoNode && NodeID(i) != prog.Root {
					return fmt.Errorf("orphan node %d (%v %v)", i, prog.Nodes[i].Kind, prog.Nodes[i].Span)
				}
			}
		}
	}
	return nil
}

// checkTiling verifies JS-INV-01: the recorded tokens and trivia cover
// the source exactly once, in order.
func checkTiling(prog *Program) error {
	pos := int32(0)
	for i, t := range prog.Tokens {
		if t.Start != pos {
			return fmt.Errorf("token %d (%v %d-%d) starts at %d, expected %d", i, t.Kind, t.Start, t.End, t.Start, pos)
		}
		if t.End <= t.Start {
			return fmt.Errorf("token %d (%v) is empty", i, t.Kind)
		}
		pos = t.End
	}
	if int(pos) != len(prog.Source) {
		return fmt.Errorf("tokens end at %d, source has %d bytes", pos, len(prog.Source))
	}
	return nil
}
