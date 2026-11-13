package htmlparse

// The HTML fragment parsing algorithm (HTML §13.4), with a context
// element described by value (the arena has no document to take it from).

// ContextElement describes the context element of a fragment parse.
type ContextElement struct {
	// Namespace of the context element; NoNamespace is taken as HTML.
	Namespace Namespace
	// Name is the local name ("td", "svg", "foreignObject", ...).
	Name string
	// Attrs are the context element's attributes (they matter for
	// MathML annotation-xml, whose encoding decides whether it is an HTML
	// integration point).
	Attrs []Attr
	// Mode is the document mode of the context element's document.
	Mode QuirksMode
	// InForm reports that the context element is, or has an ancestor that
	// is, a form element (it sets the form element pointer).
	InForm bool
}

// ParseFragment parses input as the children of context (innerHTML-style
// parsing). The returned document's Root is a DocumentFragment holding
// the result; Nodes also contains the document node, the html element the
// algorithm uses as its root, and a detached copy of the context element.
func ParseFragment(input []byte, context ContextElement, opts Options) (*Document, []ParseError) {
	ns := context.Namespace
	if ns == NoNamespace {
		ns = HTMLNamespace
	}
	doc := &Document{Mode: context.Mode, Nodes: make([]Node, 0, nodeCapacityHint(input))}
	docID := doc.NewNode(DocumentNode)
	doc.Root = docID
	tz := NewTokenizer(input, opts.tokenizeOptions())
	b := newTreeBuilder(doc, tz, opts)
	b.fragment = true

	tag := lookupTagString(context.Name)
	ctxID := b.createElement(context.Name, tag, ns, append([]Attr(nil), context.Attrs...), Span{})
	b.ctx = stackEntry{id: ctxID, tag: tag, ns: ns}
	if ns == MathMLNamespace && tag == tagAnnotationXml {
		for _, a := range context.Attrs {
			if a.Namespace == NoNamespace && a.Name == "encoding" &&
				(equalFoldASCII(a.Value, "text/html") || equalFoldASCII(a.Value, "application/xhtml+xml")) {
				b.ctx.htmlIP = true
			}
		}
	}
	if ns == HTMLNamespace {
		switch tag {
		case tagTitle, tagTextarea:
			tz.SetState(RCDATAState)
		case tagStyle, tagXmp, tagIframe, tagNoembed, tagNoframes:
			tz.SetState(RAWTEXTState)
		case tagScript:
			tz.SetState(ScriptDataState)
		case tagNoscript:
			if opts.Scripting {
				tz.SetState(RAWTEXTState)
			}
		case tagPlaintext:
			tz.SetState(PLAINTEXTState)
		}
	}
	root := b.createElement("html", tagHtml, HTMLNamespace, nil, Span{})
	doc.insertBefore(docID, root, None)
	b.push(stackEntry{id: root, tag: tagHtml, ns: HTMLNamespace})
	if b.ctx.is(tagTemplate) {
		b.pushTemplateMode(modeInTemplate)
	}
	b.resetInsertionMode()
	if context.InForm || b.ctx.is(tagForm) {
		b.form = ctxID
	}
	tz.SetAllowCDATA(ns != HTMLNamespace)
	b.run()

	frag := doc.NewNode(DocumentFragmentNode)
	doc.reparentChildren(root, frag)
	doc.Root = frag
	return doc, collectErrors(tz, b)
}
