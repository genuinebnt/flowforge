package htmlparse

import (
	"sort"
	"strings"
)

// Dump renders the tree under doc.Root in the html5lib tree-construction
// test format (the "#document" section of the .dat files): one node per
// line, "| " plus two spaces per depth, attributes on their own lines
// sorted by their displayed name, "svg "/"math " element prefixes,
// "xlink "/"xml "/"xmlns " attribute prefixes, and template contents
// under a "content" line. Lines are joined with "\n" and the result has
// no trailing newline.
func Dump(doc *Document) string {
	var b strings.Builder
	if doc == nil || doc.Node(doc.Root) == nil {
		return ""
	}
	for c := range doc.Children(doc.Root) {
		dumpNode(&b, doc, c, 0)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// DumpNode renders one subtree (the node itself at depth 0).
func DumpNode(doc *Document, id NodeID) string {
	var b strings.Builder
	if doc.Node(id) != nil {
		dumpNode(&b, doc, id, 0)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func dumpIndent(b *strings.Builder, depth int) {
	b.WriteString("| ")
	for range depth {
		b.WriteString("  ")
	}
}

// dumpNode is iterative over siblings and recursive over depth. The
// recursion depth is the tree depth: about MaxDepth for parser output,
// plus one level per template whose contents are nested, and unbounded
// for trees built with the mutation helpers.
//
// PORT: Go stacks grow on demand; a Rust port should make this iterative
// (as serializer.children is) rather than rely on a fixed thread stack.
func dumpNode(b *strings.Builder, doc *Document, id NodeID, depth int) {
	n := &doc.Nodes[id]
	dumpIndent(b, depth)
	switch n.Kind {
	case DoctypeNode:
		b.WriteString("<!DOCTYPE ")
		b.WriteString(n.Name)
		if n.PublicID != "" || n.SystemID != "" {
			b.WriteString(` "`)
			b.WriteString(n.PublicID)
			b.WriteString(`" "`)
			b.WriteString(n.SystemID)
			b.WriteString(`"`)
		}
		b.WriteString(">\n")
	case TextNode:
		b.WriteString(`"`)
		b.WriteString(n.Data)
		b.WriteString("\"\n")
	case CommentNode:
		b.WriteString("<!-- ")
		b.WriteString(n.Data)
		b.WriteString(" -->\n")
	case ProcessingInstructionNode:
		b.WriteString("<?")
		b.WriteString(n.Name)
		b.WriteString(" ")
		b.WriteString(n.Data)
		b.WriteString("?>\n")
	case ElementNode:
		b.WriteString("<")
		switch n.Namespace {
		case SVGNamespace:
			b.WriteString("svg ")
		case MathMLNamespace:
			b.WriteString("math ")
		}
		b.WriteString(n.Name)
		b.WriteString(">\n")
		if len(n.Attrs) > 0 {
			names := make([]string, len(n.Attrs))
			order := make([]int, len(n.Attrs))
			for i, a := range n.Attrs {
				names[i] = dumpAttrName(a)
				order[i] = i
			}
			// html5lib sorts attributes by displayed name (byte order).
			// Display names are unique per element, so stability does not
			// matter; any sort gives the same output.
			sort.SliceStable(order, func(x, y int) bool { return names[order[x]] < names[order[y]] })
			for _, i := range order {
				dumpIndent(b, depth+1)
				b.WriteString(names[i])
				b.WriteString(`="`)
				b.WriteString(n.Attrs[i].Value)
				b.WriteString("\"\n")
			}
		}
		if n.Content != None {
			dumpIndent(b, depth+1)
			b.WriteString("content\n")
			for c := range doc.Children(n.Content) {
				dumpNode(b, doc, c, depth+2)
			}
		}
	default:
		b.WriteString("#" + n.Kind.String() + "\n")
	}
	for c := range doc.Children(id) {
		dumpNode(b, doc, c, depth+1)
	}
}

// dumpAttrName is the html5lib display name: "xlink href", "xml lang",
// "xmlns xlink", or the bare local name.
func dumpAttrName(a Attr) string {
	switch a.Namespace {
	case XLinkNamespace:
		return "xlink " + a.Name
	case XMLNamespace:
		return "xml " + a.Name
	case XMLNSNamespace:
		return "xmlns " + a.Name
	}
	return a.Name
}
