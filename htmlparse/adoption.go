package htmlparse

import "slices"

// The adoption agency algorithm (HTML §13.2.6.4.7, after "any other end
// tag"), with the outer loop limit 8 and the inner loop limit 3. The
// sanitizer and streaming steps of the current standard are omitted: this
// parser has no sanitizer configuration and no script-visible DOM, so
// those branches never apply.
//
// The limits bound the work per token: at most 8 rounds of the outer
// loop, and after the third step of the inner loop formatting entries are
// dropped instead of cloned, so the clones per round are bounded. The stack
// keeps its length (the formatting element's entry is replaced by the new
// element's), so the algorithm cannot exceed the depth limit.

const (
	adoptionOuterLimit = 8
	adoptionInnerLimit = 3
)

// adoptionAgency runs the algorithm for the end tag (or a/nobr start tag)
// tok, whose tag name is a formatting element name ("subject").
func (b *treeBuilder) adoptionAgency(tok *Token) {
	subject := tok.atom
	// Step 2: a current node with the subject's name that is not in the
	// list is simply popped.
	if c := b.current(); c.is(subject) && b.afeIndex(c.id) < 0 {
		b.pop()
		return
	}
	for outer := 0; outer < adoptionOuterLimit; outer++ {
		// The formatting element: the last element in the list, after the
		// last marker, with the subject's tag name.
		feIdx := -1
		for i := len(b.afe) - 1; i >= 0; i-- {
			if b.afe[i].id == None {
				break
			}
			if b.afe[i].tag == subject {
				feIdx = i
				break
			}
		}
		if feIdx < 0 {
			// No formatting element: act as "any other end tag".
			b.anyOtherEndTag(tok)
			return
		}
		fe := b.afe[feIdx]
		feStack := b.stackIndex(fe.id)
		if feStack < 0 {
			// In the list but no longer open: drop the entry.
			b.parseError(ErrUnclosedElement)
			b.removeAFE(feIdx)
			return
		}
		if feStack == 0 {
			return // the root is never a formatting element
		}
		if !b.nodeInScope(fe.id) {
			b.parseError(ErrUnexpectedEndTag)
			return
		}
		if b.current().id != fe.id {
			b.parseError(ErrUnclosedElement)
		}
		// The furthest block: the topmost special element below fe.
		fbStack := -1
		for i := feStack + 1; i < len(b.stack); i++ {
			if isSpecial(b.stack[i]) {
				fbStack = i
				break
			}
		}
		if fbStack < 0 {
			// No furthest block: pop through the formatting element and
			// drop its entry (the common, well-nested case).
			b.stack = b.stack[:feStack]
			b.removeAFE(feIdx)
			return
		}
		furthest := b.stack[fbStack]
		common := b.stack[feStack-1]
		// bookmark: newElement is inserted into the list before the entry
		// now at this index (fe itself is removed at the end), so every
		// removal of an earlier entry must decrement it.
		bookmark := feIdx
		node := furthest
		lastNode := furthest
		ni := fbStack
		// Inner loop: walk up from the furthest block. ni is node's stack
		// index; when node is removed from the stack, ni-1 is still the
		// entry that was above it, as the standard requires.
		for inner := 1; ; inner++ {
			ni--
			node = b.stack[ni]
			if node.id == fe.id {
				break
			}
			ai := b.afeIndex(node.id)
			if inner > adoptionInnerLimit && ai >= 0 {
				b.removeAFE(ai)
				if ai < bookmark {
					bookmark--
				}
				ai = -1
			}
			if ai < 0 {
				b.stack = slices.Delete(b.stack, ni, ni+1)
				continue
			}
			// Replace node with a new element for its token.
			on := &b.doc.Nodes[node.id]
			id := b.createElement(on.Name, node.tag, HTMLNamespace, slices.Clone(on.Attrs), on.Span)
			b.afe[ai].id = id
			b.stack[ni].id = id
			node.id = id
			if lastNode.id == furthest.id {
				// Move the bookmark to just after the new element.
				bookmark = ai + 1
			}
			b.doc.insertBefore(node.id, lastNode.id, None)
			lastNode = node
		}
		// Insert lastNode at the appropriate place for inserting a node,
		// with the common ancestor as the override target. The place is
		// computed before lastNode is detached, as the standard computes
		// it first. The move is skipped when it is impossible: the
		// reference child is no longer a child of the parent (it was
		// lastNode itself, which the DOM would instead re-insert before
		// its next sibling; no input reaching this through the table
		// modes is known), or lastNode is a host-including ancestor of the
		// parent (the DOM would throw).
		parent, ref := b.insertionLocation(common)
		b.doc.detach(lastNode.id)
		if parent != None && (ref == None || b.doc.Nodes[ref].Parent == parent) && !b.hostIncludingAncestor(lastNode.id, parent) {
			b.doc.insertBefore(parent, lastNode.id, ref)
		}
		// A new element for the formatting element's token takes the
		// furthest block's children and becomes its only child.
		fn := &b.doc.Nodes[fe.id]
		nid := b.createElement(fn.Name, fe.tag, HTMLNamespace, slices.Clone(fn.Attrs), fn.Span)
		b.doc.reparentChildren(furthest.id, nid)
		b.doc.insertBefore(furthest.id, nid, None)
		// Replace fe's list entry by the new element at the bookmark.
		b.afe = slices.Insert(b.afe, bookmark, afeEntry{id: nid, tag: fe.tag})
		if i := b.afeIndex(fe.id); i >= 0 {
			b.removeAFE(i)
		}
		// Replace fe on the stack: remove it and insert the new element
		// immediately below the furthest block (the spec's "below" is
		// nearer the current node: index fi+1).
		b.removeFromStack(fe.id)
		fi := b.stackIndex(furthest.id)
		b.stack = slices.Insert(b.stack, fi+1, stackEntry{id: nid, tag: fe.tag, ns: HTMLNamespace})
	}
}

// hostIncludingAncestor reports whether a is a host-including inclusive
// ancestor of n (template contents count as children of their template),
// the DOM check that keeps a move from creating a cycle.
func (b *treeBuilder) hostIncludingAncestor(a, n NodeID) bool {
	for n != None {
		if n == a {
			return true
		}
		p := b.doc.Nodes[n].Parent
		if p == None {
			p = b.templateHost[n]
			if p == 0 {
				p = None
			} else {
				p--
			}
		}
		n = p
	}
	return false
}
