package htmlparse

import "iter"

// NodeID indexes Document.Nodes. None (-1) means "no node".
type NodeID int32

// None is the null NodeID.
const None NodeID = -1

// NodeKind is the DOM node type.
type NodeKind uint8

// Node kinds.
const (
	DocumentNode NodeKind = iota
	DoctypeNode
	ElementNode
	TextNode
	CommentNode
	DocumentFragmentNode
	// ProcessingInstructionNode is produced by the HTML processing
	// instruction syntax (<?target data>) added to the standard in 2025.
	ProcessingInstructionNode
)

var nodeKindNames = [...]string{"document", "doctype", "element", "text", "comment", "document-fragment", "processing-instruction"}

// String returns the kind's stable lower-case name.
func (k NodeKind) String() string {
	if int(k) < len(nodeKindNames) {
		return nodeKindNames[k]
	}
	return "node-kind-?"
}

// Namespace is an element or attribute namespace.
type Namespace uint8

// Namespaces (Infra "namespaces"). NoNamespace is used for ordinary
// attributes.
const (
	NoNamespace Namespace = iota
	HTMLNamespace
	SVGNamespace
	MathMLNamespace
	XLinkNamespace
	XMLNamespace
	XMLNSNamespace
)

var namespaceURIs = [...]string{
	"",
	"http://www.w3.org/1999/xhtml",
	"http://www.w3.org/2000/svg",
	"http://www.w3.org/1998/Math/MathML",
	"http://www.w3.org/1999/xlink",
	"http://www.w3.org/XML/1998/namespace",
	"http://www.w3.org/2000/xmlns/",
}

// URI returns the namespace URI ("" for NoNamespace).
func (n Namespace) URI() string {
	if int(n) < len(namespaceURIs) {
		return namespaceURIs[n]
	}
	return ""
}

// QuirksMode is the document mode (DOM "mode").
type QuirksMode uint8

// Document modes.
const (
	NoQuirks QuirksMode = iota
	Quirks
	LimitedQuirks
)

// String returns "no-quirks", "quirks" or "limited-quirks".
func (m QuirksMode) String() string {
	switch m {
	case Quirks:
		return "quirks"
	case LimitedQuirks:
		return "limited-quirks"
	}
	return "no-quirks"
}

// Span is a half-open byte range [Start, End) in the parser input.
type Span struct {
	Start, End int
}

// Attr is an attribute. Name is the local name; for the namespaced
// attributes created by the foreign-content rules (xlink:href, xml:lang,
// xmlns:xlink, ...) Namespace is set and the prefix is implied by it.
type Attr struct {
	Namespace Namespace
	Name      string
	Value     string
}

// Node is one arena node. Which fields are meaningful depends on Kind:
//
//	Element:                Namespace, Name (local name), Attrs, Content (template only)
//	Text, Comment:          Data
//	ProcessingInstruction:  Name (target), Data
//	Doctype:                Name, PublicID, SystemID (missing identifiers are "")
//	Document, Fragment:     links only
//
// Span is the source span of the token that created the node. A Text node
// built from several character tokens spans from the first token's start
// to the last token's end. Nodes the parser created without a token
// (implied html/head/body/tbody...) have an empty span at the position of
// the token that caused them; the fragment parser's synthetic html root
// and context element have the empty span at 0.
type Node struct {
	Kind      NodeKind
	Namespace Namespace
	Name      string
	Attrs     []Attr
	Data      string
	PublicID  string
	SystemID  string

	// Tree links (None when absent): children form a doubly linked list.
	Parent, FirstChild, LastChild, PrevSibling, NextSibling NodeID

	// Content is the template contents DocumentFragment of an HTML
	// template element, None otherwise.
	Content NodeID

	Span Span
}

// Document is an arena DOM. Nodes[Root] is the Document node for Parse and
// a DocumentFragment for ParseFragment. Template contents are separate
// DocumentFragment nodes referenced from Node.Content; nodes removed from
// the tree by the parser (for example body when a frameset replaces it)
// stay in the arena with Parent == None.
type Document struct {
	Nodes []Node
	Root  NodeID
	Mode  QuirksMode // from the DOCTYPE (§13.2.6.4.1; Quirks when it is missing); for a fragment, the context's Mode
}

// Node returns a pointer to the node id, or nil if id is out of range.
func (d *Document) Node(id NodeID) *Node {
	if id < 0 || int(id) >= len(d.Nodes) {
		return nil
	}
	return &d.Nodes[id]
}

func (d *Document) valid(id NodeID) bool { return id >= 0 && int(id) < len(d.Nodes) }

// NewNode appends a detached node of the given kind and returns its id.
func (d *Document) NewNode(kind NodeKind) NodeID {
	d.Nodes = append(d.Nodes, Node{
		Kind: kind, Parent: None, FirstChild: None, LastChild: None,
		PrevSibling: None, NextSibling: None, Content: None,
	})
	return NodeID(len(d.Nodes) - 1)
}

// NewElement appends a detached element. For an HTML template element it
// also creates the template contents fragment.
func (d *Document) NewElement(ns Namespace, name string, attrs []Attr) NodeID {
	id := d.NewNode(ElementNode)
	n := &d.Nodes[id]
	n.Namespace = ns
	n.Name = name
	n.Attrs = attrs
	if ns == HTMLNamespace && name == "template" {
		c := d.NewNode(DocumentFragmentNode)
		d.Nodes[id].Content = c
	}
	return id
}

// NewText appends a detached Text node.
func (d *Document) NewText(data string) NodeID {
	id := d.NewNode(TextNode)
	d.Nodes[id].Data = data
	return id
}

// NewComment appends a detached Comment node.
func (d *Document) NewComment(data string) NodeID {
	id := d.NewNode(CommentNode)
	d.Nodes[id].Data = data
	return id
}

// isAncestorOrSelf reports whether a is b or an ancestor of b (template
// contents count as children of their template for this purpose).
func (d *Document) isAncestorOrSelf(a, b NodeID) bool {
	for n := b; n != None; {
		if n == a {
			return true
		}
		p := d.Nodes[n].Parent
		if p == None && d.Nodes[n].Kind == DocumentFragmentNode {
			p = d.hostOf(n)
		}
		n = p
	}
	return false
}

// hostOf returns the template element whose Content is frag, or None.
// It is a linear scan; only the validating public helpers use it.
func (d *Document) hostOf(frag NodeID) NodeID {
	for i := range d.Nodes {
		if d.Nodes[i].Content == frag {
			return NodeID(i)
		}
	}
	return None
}

func (d *Document) canHaveChildren(id NodeID) bool {
	switch d.Nodes[id].Kind {
	case DocumentNode, ElementNode, DocumentFragmentNode:
		return true
	}
	return false
}

// AppendChild moves child (detaching it first) to the end of parent's
// children.
func (d *Document) AppendChild(parent, child NodeID) error {
	return d.InsertBefore(parent, child, None)
}

// InsertBefore moves child (detaching it first) into parent before ref;
// ref == None appends. It refuses out-of-range ids, a ref that is not a
// child of parent, parents that cannot have children, and cycles.
func (d *Document) InsertBefore(parent, child, ref NodeID) error {
	if !d.valid(parent) || !d.valid(child) || (ref != None && !d.valid(ref)) {
		return ErrInvalidNode
	}
	if !d.canHaveChildren(parent) || d.Nodes[child].Kind == DocumentNode {
		return ErrInvalidNode
	}
	if ref != None && d.Nodes[ref].Parent != parent {
		return ErrInvalidNode
	}
	if ref == child {
		return nil
	}
	if d.isAncestorOrSelf(child, parent) {
		return ErrHierarchy
	}
	d.insertBefore(parent, child, ref)
	return nil
}

// Detach removes id from its parent (no-op if it has none).
func (d *Document) Detach(id NodeID) error {
	if !d.valid(id) {
		return ErrInvalidNode
	}
	d.detach(id)
	return nil
}

// ReparentChildren moves all children of from, in order, to the end of
// to's children.
func (d *Document) ReparentChildren(from, to NodeID) error {
	if !d.valid(from) || !d.valid(to) || !d.canHaveChildren(to) {
		return ErrInvalidNode
	}
	if from == to {
		return nil
	}
	if d.isAncestorOrSelf(from, to) {
		return ErrHierarchy
	}
	d.reparentChildren(from, to)
	return nil
}

// --- unchecked primitives used by the tree builder ---

func (d *Document) detach(id NodeID) {
	n := &d.Nodes[id]
	p := n.Parent
	if p == None {
		return
	}
	if n.PrevSibling != None {
		d.Nodes[n.PrevSibling].NextSibling = n.NextSibling
	} else {
		d.Nodes[p].FirstChild = n.NextSibling
	}
	if n.NextSibling != None {
		d.Nodes[n.NextSibling].PrevSibling = n.PrevSibling
	} else {
		d.Nodes[p].LastChild = n.PrevSibling
	}
	n.Parent, n.PrevSibling, n.NextSibling = None, None, None
}

func (d *Document) insertBefore(parent, child, ref NodeID) {
	d.detach(child)
	c := &d.Nodes[child]
	c.Parent = parent
	if ref == None {
		last := d.Nodes[parent].LastChild
		c.PrevSibling = last
		c.NextSibling = None
		if last != None {
			d.Nodes[last].NextSibling = child
		} else {
			d.Nodes[parent].FirstChild = child
		}
		d.Nodes[parent].LastChild = child
		return
	}
	prev := d.Nodes[ref].PrevSibling
	c.PrevSibling = prev
	c.NextSibling = ref
	d.Nodes[ref].PrevSibling = child
	if prev != None {
		d.Nodes[prev].NextSibling = child
	} else {
		d.Nodes[parent].FirstChild = child
	}
}

func (d *Document) reparentChildren(from, to NodeID) {
	for c := d.Nodes[from].FirstChild; c != None; c = d.Nodes[from].FirstChild {
		d.insertBefore(to, c, None)
	}
}

// --- traversal helpers (used by extraction code) ---

// Children iterates the children of id in order.
func (d *Document) Children(id NodeID) iter.Seq[NodeID] {
	return func(yield func(NodeID) bool) {
		if !d.valid(id) {
			return
		}
		for c := d.Nodes[id].FirstChild; c != None; c = d.Nodes[c].NextSibling {
			if !yield(c) {
				return
			}
		}
	}
}

// PORT: iter.Seq is a push iterator; in Rust implement Iterator over the
// same explicit stack.
//
// Descendants iterates the descendants of id in tree order (pre-order),
// not including id. Template contents are visited where the template
// element is (right after it, before its following siblings), so
// extraction sees markup inside templates too.
func (d *Document) Descendants(id NodeID) iter.Seq[NodeID] {
	return func(yield func(NodeID) bool) {
		if !d.valid(id) {
			return
		}
		// Explicit stack of "next node to visit"; iterative so deep trees
		// cannot overflow the call stack.
		var stack []NodeID
		push := func(n NodeID) {
			if d.Nodes[n].FirstChild != None {
				stack = append(stack, d.Nodes[n].FirstChild)
			}
		}
		push(id)
		if c := d.Nodes[id].Content; c != None {
			push(c)
		}
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if next := d.Nodes[n].NextSibling; next != None {
				stack = append(stack, next)
			}
			if !yield(n) {
				return
			}
			if c := d.Nodes[n].Content; c != None && d.Nodes[c].FirstChild != None {
				stack = append(stack, d.Nodes[c].FirstChild)
			}
			if d.Nodes[n].FirstChild != None {
				stack = append(stack, d.Nodes[n].FirstChild)
			}
		}
	}
}

// Elements iterates every element under id (tree order, template
// contents included) whose namespace is ns and local name is name. An
// empty name matches every element in ns.
func (d *Document) Elements(id NodeID, ns Namespace, name string) iter.Seq[NodeID] {
	return func(yield func(NodeID) bool) {
		for n := range d.Descendants(id) {
			nd := &d.Nodes[n]
			if nd.Kind == ElementNode && nd.Namespace == ns && (name == "" || nd.Name == name) {
				if !yield(n) {
					return
				}
			}
		}
	}
}

// Attr returns the value of the attribute with no namespace and the given
// local name on element id.
func (d *Document) Attr(id NodeID, name string) (string, bool) {
	return d.AttrNS(id, NoNamespace, name)
}

// AttrNS returns the value of the attribute (ns, name) on element id.
func (d *Document) AttrNS(id NodeID, ns Namespace, name string) (string, bool) {
	if !d.valid(id) {
		return "", false
	}
	for _, a := range d.Nodes[id].Attrs {
		if a.Namespace == ns && a.Name == name {
			return a.Value, true
		}
	}
	return "", false
}

// TextContent returns the concatenated data of all Text descendants of id
// (DOM textContent; template contents are not included, as in the DOM).
func (d *Document) TextContent(id NodeID) string {
	if !d.valid(id) {
		return ""
	}
	if k := d.Nodes[id].Kind; k == TextNode || k == CommentNode || k == ProcessingInstructionNode {
		return d.Nodes[id].Data
	}
	var b []byte
	var stack []NodeID
	if f := d.Nodes[id].FirstChild; f != None {
		stack = append(stack, f)
	}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if next := d.Nodes[n].NextSibling; next != None {
			stack = append(stack, next)
		}
		if d.Nodes[n].Kind == TextNode {
			b = append(b, d.Nodes[n].Data...)
		}
		if f := d.Nodes[n].FirstChild; f != None {
			stack = append(stack, f)
		}
	}
	return string(b)
}

// DocumentElement returns the first element child of the root (the html
// element of a parsed document), or None.
func (d *Document) DocumentElement() NodeID {
	if !d.valid(d.Root) {
		return None
	}
	for c := range d.Children(d.Root) {
		if d.Nodes[c].Kind == ElementNode {
			return c
		}
	}
	return None
}
