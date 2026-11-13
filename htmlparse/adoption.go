package htmlparse

import "slices"

// The adoption agency algorithm (HTML §13.2.6.4.7, after "any other end
// tag"), with the outer loop limit 8 and the inner loop limit 3. The
// sanitizer and streaming steps of the current standard are omitted: this
// parser has no sanitizer configuration and no script-visible DOM, so
// those branches never apply.

const (
	adoptionOuterLimit = 8
	adoptionInnerLimit = 3
)

// adoptionAgency runs the algorithm for the end tag (or a/nobr start tag)
// tok, whose tag name is a formatting element name.
func (b *treeBuilder) adoptionAgency(tok *Token) {
	subject := tok.atom
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
			b.anyOtherEndTag(tok)
			return
		}
		fe := b.afe[feIdx]
		feStack := b.stackIndex(fe.id)
		if feStack < 0 {
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
			b.stack = b.stack[:feStack]
			b.removeAFE(feIdx)
			return
		}
		furthest := b.stack[fbStack]
		common := b.stack[feStack-1]
		// bookmark: newElement is inserted into the list before the entry
		// now at this index (fe itself is removed at the end).
		bookmark := feIdx
		node := furthest
		lastNode := furthest
		ni := fbStack
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
				bookmark = ai + 1
			}
			b.doc.insertBefore(node.id, lastNode.id, None)
			lastNode = node
		}
		// Insert lastNode at the appropriate place for inserting a node,
		// with the common ancestor as the override target. As in the
		// standard, the place is computed before lastNode is removed, and
		// the insertion is skipped if removing lastNode detached the
		// reference child.
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
		// Move fe's list entry to the bookmark.
		b.afe = slices.Insert(b.afe, bookmark, afeEntry{id: nid, tag: fe.tag})
		if i := b.afeIndex(fe.id); i >= 0 {
			b.removeAFE(i)
		}
		// Replace fe on the stack: remove it and insert the new element
		// immediately below the furthest block.
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
