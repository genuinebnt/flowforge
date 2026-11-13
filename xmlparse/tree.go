package xmlparse

import (
	"io"
	"sort"
	"strings"
)

// NodeID indexes Document.Nodes. NoNode (-1) means "none".
type NodeID int32

// NoNode is the null NodeID.
const NoNode NodeID = -1

// NodeKind is the type of a tree node.
type NodeKind uint8

// Node kinds.
const (
	NodeDocument NodeKind = iota // the document node (always Nodes[0])
	NodeElement
	NodeText
	NodeCData
	NodeComment
	NodePI
)

var nodeKindNames = [...]string{"document", "element", "text", "cdata", "comment", "pi"}

// String returns the stable name of k.
func (k NodeKind) String() string {
	if int(k) < len(nodeKindNames) {
		return nodeKindNames[k]
	}
	return "node?"
}

// Node is one arena node.
type Node struct {
	Kind                                       NodeKind
	Parent, FirstChild, LastChild, NextSibling NodeID
	// Name is the element name, or the PI target (Local).
	Name Name
	// AttrStart and AttrEnd delimit the element's attributes in
	// Document.Attrs.
	AttrStart, AttrEnd int32
	// Text is the content of text, CDATA, comment and PI nodes.
	Text       string
	Start, End int32
}

// Document is a parsed XML document: an arena of nodes plus the prolog
// facts.
type Document struct {
	Nodes []Node
	Attrs []Attr
	// Root is the root element, or NoNode.
	Root NodeID
	// XML declaration and doctype (bare name only).
	Version, Encoding string
	Standalone        int8
	Doctype           string
	// UTF16 is set when the input was UTF-16 (offsets index the
	// transcoded UTF-8).
	UTF16 bool
	// Grade is complete, or how the read stopped (see Err).
	Grade Grade
	// Err is the error that stopped the read, or nil.
	Err *SyntaxError
}

// Children returns the child ids of id in order.
func (d *Document) Children(id NodeID) []NodeID {
	var out []NodeID
	for c := d.Nodes[id].FirstChild; c != NoNode; c = d.Nodes[c].NextSibling {
		out = append(out, c)
	}
	return out
}

// AttrsOf returns the attributes of element id.
func (d *Document) AttrsOf(id NodeID) []Attr {
	n := &d.Nodes[id]
	return d.Attrs[n.AttrStart:n.AttrEnd]
}

// Parse decodes src into a tree. On error it returns the partial tree
// built so far together with the *SyntaxError; Document.Grade tells
// partial (limit), unsupported (refused feature) and failed apart.
func Parse(src []byte, opts Options) (*Document, error) {
	dec := NewDecoder(src, opts)
	doc := &Document{Root: NoNode, Standalone: -1, UTF16: dec.UTF16()}
	doc.Nodes = append(doc.Nodes, Node{Kind: NodeDocument, Parent: NoNode, FirstChild: NoNode,
		LastChild: NoNode, NextSibling: NoNode, End: int32(len(src))})
	cur := NodeID(0)
	add := func(n Node) NodeID {
		n.Parent, n.FirstChild, n.LastChild, n.NextSibling = cur, NoNode, NoNode, NoNode
		doc.Nodes = append(doc.Nodes, n)
		id := NodeID(len(doc.Nodes) - 1)
		p := &doc.Nodes[cur]
		if p.LastChild == NoNode {
			p.FirstChild = id
		} else {
			doc.Nodes[p.LastChild].NextSibling = id
		}
		p.LastChild = id
		return id
	}
	for {
		ev, err := dec.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			se := err.(*SyntaxError)
			doc.Err = se
			doc.Grade = se.Code.Grade()
			return doc, se
		}
		switch ev.Kind {
		case EventXMLDecl:
			doc.Version, doc.Encoding, doc.Standalone = ev.Version, ev.Encoding, ev.Standalone
		case EventDoctype:
			doc.Doctype = ev.Name.QName()
		case EventStartElement:
			a0 := int32(len(doc.Attrs))
			doc.Attrs = append(doc.Attrs, ev.Attrs...)
			id := add(Node{Kind: NodeElement, Name: ev.Name, AttrStart: a0, AttrEnd: int32(len(doc.Attrs)),
				Start: ev.Start, End: ev.End})
			if cur == 0 {
				doc.Root = id
			}
			cur = id
		case EventEndElement:
			doc.Nodes[cur].End = ev.End
			cur = doc.Nodes[cur].Parent
		case EventText:
			add(Node{Kind: NodeText, Text: ev.Text, Start: ev.Start, End: ev.End})
		case EventCData:
			add(Node{Kind: NodeCData, Text: ev.Text, Start: ev.Start, End: ev.End})
		case EventComment:
			add(Node{Kind: NodeComment, Text: ev.Text, Start: ev.Start, End: ev.End})
		case EventPI:
			add(Node{Kind: NodePI, Name: ev.Name, Text: ev.Text, Start: ev.Start, End: ev.End})
		}
	}
	return doc, nil
}

// Canonical writes the document in a canonical form modelled on
// Canonical XML 1.0 (no XML declaration or doctype; top-level nodes
// separated by "\n"; empty elements as start-end pairs; namespace
// declarations first, sorted by prefix, then attributes sorted by
// namespace URI and local name; text and attribute escaping as in C14N;
// CDATA written as escaped text). Re-parsing the output and writing it
// again gives the same bytes (XML-INV-04).
func (d *Document) Canonical(withComments bool) []byte {
	var b strings.Builder
	first := true
	for c := d.Nodes[0].FirstChild; c != NoNode; c = d.Nodes[c].NextSibling {
		k := d.Nodes[c].Kind
		if k == NodeText || k == NodeCData || (k == NodeComment && !withComments) {
			continue
		}
		if !first {
			b.WriteByte('\n')
		}
		first = false
		d.writeNode(&b, c, withComments)
	}
	return []byte(b.String())
}

type frame struct {
	id    NodeID
	close bool
}

func (d *Document) writeNode(b *strings.Builder, root NodeID, withComments bool) {
	stack := []frame{{id: root}}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		n := &d.Nodes[f.id]
		if f.close {
			b.WriteString("</")
			b.WriteString(n.Name.QName())
			b.WriteByte('>')
			continue
		}
		switch n.Kind {
		case NodeElement:
			b.WriteByte('<')
			b.WriteString(n.Name.QName())
			attrs := append([]Attr(nil), d.AttrsOf(f.id)...)
			sort.SliceStable(attrs, func(i, j int) bool { return attrLess(&attrs[i], &attrs[j]) })
			for _, a := range attrs {
				b.WriteByte(' ')
				b.WriteString(a.Name.QName())
				b.WriteString("=\"")
				escapeAttr(b, a.Value)
				b.WriteByte('"')
			}
			b.WriteByte('>')
			stack = append(stack, frame{id: f.id, close: true})
			mark := len(stack)
			for c := n.FirstChild; c != NoNode; c = d.Nodes[c].NextSibling {
				stack = append(stack, frame{id: c})
			}
			for i, j := mark, len(stack)-1; i < j; i, j = i+1, j-1 {
				stack[i], stack[j] = stack[j], stack[i]
			}
		case NodeText, NodeCData:
			escapeText(b, n.Text)
		case NodeComment:
			if withComments {
				b.WriteString("<!--")
				b.WriteString(n.Text)
				b.WriteString("-->")
			}
		case NodePI:
			b.WriteString("<?")
			b.WriteString(n.Name.Local)
			if n.Text != "" {
				b.WriteByte(' ')
				b.WriteString(n.Text)
			}
			b.WriteString("?>")
		}
	}
}

func isNSDecl(a *Attr) bool { return a.Name.Space == XMLNSNamespace }

// attrLess orders namespace declarations first (default, then by prefix
// local name), then other attributes by (namespace URI, local name).
func attrLess(a, b *Attr) bool {
	an, bn := isNSDecl(a), isNSDecl(b)
	if an != bn {
		return an
	}
	if an {
		ad, bd := a.Name.Prefix == "", b.Name.Prefix == ""
		if ad != bd {
			return ad
		}
		return a.Name.Local < b.Name.Local
	}
	if a.Name.Space != b.Name.Space {
		return a.Name.Space < b.Name.Space
	}
	return a.Name.Local < b.Name.Local
}

func escapeText(b *strings.Builder, s string) {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '\r':
			b.WriteString("&#xD;")
		default:
			b.WriteByte(c)
		}
	}
}

func escapeAttr(b *strings.Builder, s string) {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '"':
			b.WriteString("&quot;")
		case '\t':
			b.WriteString("&#x9;")
		case '\n':
			b.WriteString("&#xA;")
		case '\r':
			b.WriteString("&#xD;")
		default:
			b.WriteByte(c)
		}
	}
}
