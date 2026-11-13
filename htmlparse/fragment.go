package htmlparse

// The HTML fragment parsing algorithm (HTML §13.4), with a context
// element described by value (the arena has no document to take it from).
// The steps in order: document mode from the context, tokenizer state
// from the context's name, a new html root as the only open element, "in
// template" pushed for a template context, insertion mode reset, the
// form element pointer, then parsing; the result is the root's children.

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
	// is, a form element (it sets the form element pointer). The pointer
	// then refers to the detached context copy, which is enough: the
	// rules only test it for None and scope.
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
	// The context element is a detached copy: the algorithm only reads
	// its name, namespace and start tag attributes.
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
	// Step 4: the tokenizer state for HTML contexts. No last start tag is
	// set, so in RCDATA/RAWTEXT no end tag is appropriate and the whole
	// input is text.
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
	// run updates the CDATA flag only after each token; the first token
	// sees the context as the adjusted current node.
	tz.SetAllowCDATA(ns != HTMLNamespace)
	b.run()

	frag := doc.NewNode(DocumentFragmentNode)
	doc.reparentChildren(root, frag)
	doc.Root = frag
	return doc, collectErrors(tz, b)
}
