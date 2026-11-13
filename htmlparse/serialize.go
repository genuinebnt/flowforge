package htmlparse

// The HTML fragment serialization algorithm (HTML §13.3), without the
// shadow-root and custom-element "is" steps (this DOM has neither).

// RenderOptions configures serialization.
type RenderOptions struct {
	// Scripting makes noscript contents serialize literally, as they do
	// when "scripting is enabled for the node". It should match the
	// Options.Scripting the document was parsed with.
	Scripting bool
}

// Render returns the HTML fragment serialization of node id: the markup
// of its children (innerHTML). For a template element it serializes the
// template contents. For a node that cannot have children the result is
// empty. Use RenderNode for the node itself (outerHTML).
func Render(doc *Document, id NodeID) []byte {
	return RenderWith(doc, id, RenderOptions{})
}

// RenderWith is Render with options.
func RenderWith(doc *Document, id NodeID, opts RenderOptions) []byte {
	n := doc.Node(id)
	if n == nil {
		return nil
	}
	if n.Kind == ElementNode && serializesAsVoid(n) {
		return nil
	}
	s := serializer{doc: doc, opts: opts}
	from := id
	if n.Content != None {
		from = n.Content
	}
	s.children(from)
	return s.out
}

// RenderNode serializes node id itself (outerHTML for elements, the
// serialization of a single child otherwise; a Text node is escaped or
// raw according to its parent).
func RenderNode(doc *Document, id NodeID, opts RenderOptions) []byte {
	if doc.Node(id) == nil {
		return nil
	}
	s := serializer{doc: doc, opts: opts}
	parent := doc.Nodes[id].Parent
	s.node(id, parent)
	return s.out
}

// serializer accumulates output; the traversal is iterative (children)
// so arbitrarily deep trees serialize without recursion.
type serializer struct {
	doc  *Document
	opts RenderOptions
	out  []byte
}

// frame is one level of the explicit traversal stack.
type frame struct {
	next   NodeID // next child to serialize
	parent NodeID // element (or document/fragment) whose children these are
	closer string // end tag to write when the level is done ("" for none)
}

// children serializes the children of id iteratively.
func (s *serializer) children(id NodeID) {
	stack := []frame{{next: s.doc.Nodes[id].FirstChild, parent: id}}
	for len(stack) > 0 {
		top := &stack[len(stack)-1]
		c := top.next
		if c == None {
			s.out = append(s.out, top.closer...)
			stack = stack[:len(stack)-1]
			continue
		}
		top.next = s.doc.Nodes[c].NextSibling
		n := &s.doc.Nodes[c]
		if n.Kind != ElementNode {
			s.leaf(c, top.parent)
			continue
		}
		s.startTag(n)
		if serializesAsVoid(n) {
			continue
		}
		from := c
		if n.Content != None {
			from = n.Content
		}
		stack = append(stack, frame{next: s.doc.Nodes[from].FirstChild, parent: c, closer: "</" + n.Name + ">"})
	}
}

// node serializes one node and, for an element, its subtree.
func (s *serializer) node(id, parent NodeID) {
	n := &s.doc.Nodes[id]
	if n.Kind != ElementNode {
		s.leaf(id, parent)
		return
	}
	s.startTag(n)
	if serializesAsVoid(n) {
		return
	}
	from := id
	if n.Content != None {
		from = n.Content
	}
	s.children(from)
	s.out = append(s.out, "</"...)
	s.out = append(s.out, n.Name...)
	s.out = append(s.out, '>')
}

// startTag writes "<name" and the attributes with their serialized names
// (xml:, xmlns, xmlns:, xlink: prefixes from the namespace) and escaped
// values, in stored order, then ">".
func (s *serializer) startTag(n *Node) {
	s.out = append(s.out, '<')
	s.out = append(s.out, n.Name...)
	for _, a := range n.Attrs {
		s.out = append(s.out, ' ')
		switch a.Namespace {
		case XMLNamespace:
			s.out = append(s.out, "xml:"...)
		case XMLNSNamespace:
			if a.Name != "xmlns" {
				s.out = append(s.out, "xmlns:"...)
			}
		case XLinkNamespace:
			s.out = append(s.out, "xlink:"...)
		}
		s.out = append(s.out, a.Name...)
		s.out = append(s.out, '=', '"')
		s.out = escapeString(s.out, a.Value, true)
		s.out = append(s.out, '"')
	}
	s.out = append(s.out, '>')
}

// leaf writes a non-element node. Comments and processing instructions
// are written verbatim (the standard does not escape them, so "-->" in
// comment data does not round-trip), and a DOCTYPE keeps only its name.
func (s *serializer) leaf(id, parent NodeID) {
	n := &s.doc.Nodes[id]
	switch n.Kind {
	case TextNode:
		if s.rawTextParent(parent) {
			s.out = append(s.out, n.Data...)
		} else {
			s.out = escapeString(s.out, n.Data, false)
		}
	case CommentNode:
		s.out = append(s.out, "<!--"...)
		s.out = append(s.out, n.Data...)
		s.out = append(s.out, "-->"...)
	case ProcessingInstructionNode:
		s.out = append(s.out, "<?"...)
		s.out = append(s.out, n.Name...)
		s.out = append(s.out, ' ')
		s.out = append(s.out, n.Data...)
		s.out = append(s.out, "?>"...)
	case DoctypeNode:
		s.out = append(s.out, "<!DOCTYPE "...)
		s.out = append(s.out, n.Name...)
		s.out = append(s.out, '>')
	}
}

// rawTextParent reports whether text children of parent are written
// literally: HTML style, script, xmp, iframe, noembed, noframes and
// plaintext, and noscript when scripting is enabled.
func (s *serializer) rawTextParent(parent NodeID) bool {
	p := s.doc.Node(parent)
	if p == nil || p.Kind != ElementNode || p.Namespace != HTMLNamespace {
		return false
	}
	switch p.Name {
	case "style", "script", "xmp", "iframe", "noembed", "noframes", "plaintext":
		return true
	case "noscript":
		return s.opts.Scripting
	}
	return false
}

// serializesAsVoid: the HTML void elements plus basefont, bgsound, frame,
// keygen and param. Such elements get no end tag and their children (only
// the mutation helpers can add any) are not written.
func serializesAsVoid(n *Node) bool {
	if n.Namespace != HTMLNamespace {
		return false
	}
	switch n.Name {
	case "area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "source",
		"track", "wbr", "basefont", "bgsound", "frame", "keygen", "param":
		return true
	}
	return false
}

// escapeString is "escaping a string" (§13.3): & U+00A0 < > always
// (the 2025 standard also escapes < and > in attribute values), and " in
// attribute mode. It scans bytes: U+00A0 is the UTF-8 pair C2 A0, and no
// other escaped character can be part of a multi-byte sequence.
func escapeString(dst []byte, s string, attr bool) []byte {
	last := 0
	for i := 0; i < len(s); i++ {
		var rep string
		switch c := s[i]; c {
		case '&':
			rep = "&amp;"
		case '<':
			rep = "&lt;"
		case '>':
			rep = "&gt;"
		case '"':
			if !attr {
				continue
			}
			rep = "&quot;"
		case 0xC2:
			if i+1 < len(s) && s[i+1] == 0xA0 {
				dst = append(dst, s[last:i]...)
				dst = append(dst, "&nbsp;"...)
				i++
				last = i + 1
			}
			continue
		default:
			continue
		}
		dst = append(dst, s[last:i]...)
		dst = append(dst, rep...)
		last = i + 1
	}
	return append(dst, s[last:]...)
}
