package cssparse

import "sort"

// URLForm is how a URL reference is spelled.
type URLForm uint8

const (
	// URLFormToken is an unquoted url(...) (<url-token>).
	URLFormToken URLForm = iota
	// URLFormFunction is url("...") or src("...") (a <function-token>
	// whose first argument is a string).
	URLFormFunction
	// URLFormString is a bare string that the context makes a URL
	// (@import "x", @namespace "x", image-set("x" 1x)).
	URLFormString
)

var urlFormNames = [...]string{"token", "function", "string"}

// String returns the stable name of f.
func (f URLForm) String() string {
	if int(f) < len(urlFormNames) {
		return urlFormNames[f]
	}
	return "form?"
}

// URLContext is where a URL reference appears.
type URLContext uint8

const (
	// URLContextValue: any other component value (backgrounds, masks,
	// cursors, ...).
	URLContextValue URLContext = iota
	// URLContextImport: the target of @import.
	URLContextImport
	// URLContextNamespace: the namespace URL of @namespace (an identifier,
	// not fetched by browsers).
	URLContextNamespace
	// URLContextImageSet: a candidate of image-set() / -webkit-image-set().
	URLContextImageSet
	// URLContextFontFaceSrc: the src descriptor of @font-face.
	URLContextFontFaceSrc
)

var urlContextNames = [...]string{"value", "import", "namespace", "image-set", "font-face-src"}

// String returns the stable name of c.
func (c URLContext) String() string {
	if int(c) < len(urlContextNames) {
		return urlContextNames[c]
	}
	return "context?"
}

// URLRef is one URL reference found in a stylesheet.
type URLRef struct {
	// URL is the decoded reference, unresolved.
	URL     string
	Form    URLForm
	Context URLContext
	// Start and End are the byte span of the token, string or function.
	Start, End int32
}

// URLResult is the outcome of URL extraction.
type URLResult struct {
	Refs  []URLRef
	Grade Grade
	// Diags holds the document's diagnostics plus DiagLimitExceeded when
	// Options.MaxURLs cut the list.
	Diags []Diagnostic
}

// ExtractURLs parses src as a stylesheet and returns every URL reference
// in source order.
func ExtractURLs(src []byte, opts Options) URLResult {
	return ParseStylesheet(src, opts).URLs()
}

type urlFrame struct {
	id      NodeID
	ctx     URLContext
	strings bool // strings directly here are URLs
}

// URLs returns every URL reference in the tree under d.Root, in source
// order. @font-face blocks and the blocks of other at-rules are re-parsed
// with ParseBlockContents (appending nodes to d); style-rule blocks are
// walked as component values.
func (d *Document) URLs() URLResult {
	max := d.opts.maxURLs()
	var refs []URLRef
	diags := append([]Diagnostic(nil), d.Diags...)
	stack := []urlFrame{{id: d.Root}}
	push := func(first NodeID, ctx URLContext, strings bool) {
		mark := len(stack)
		for c := first; c != NoNode; c = d.Nodes[c].NextSibling {
			stack = append(stack, urlFrame{id: c, ctx: ctx, strings: strings})
		}
		for i, j := mark, len(stack)-1; i < j; i, j = i+1, j-1 {
			stack[i], stack[j] = stack[j], stack[i]
		}
	}
	add := func(r URLRef) bool {
		if len(refs) >= max {
			diags = append(diags, Diagnostic{Code: DiagLimitExceeded, Start: r.Start, End: r.End})
			return false
		}
		refs = append(refs, r)
		return true
	}
walk:
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		n := &d.Nodes[f.id]
		switch n.Kind {
		case NodeAtRule:
			name := lowerASCII(d.Name(f.id))
			block := n.Block
			switch name {
			case "import":
				first := true
				var rest []urlFrame
				for c := n.FirstChild; c != NoNode; c = d.Nodes[c].NextSibling {
					fr := urlFrame{id: c, ctx: URLContextValue}
					if first && !d.isWhitespace(c) {
						fr = urlFrame{id: c, ctx: URLContextImport, strings: true}
						first = false
					}
					rest = append(rest, fr)
				}
				if block != NoNode {
					// @import takes no block; walk a stray one raw.
					stack = append(stack, urlFrame{id: block, ctx: URLContextValue})
					block = NoNode
				}
				for i := len(rest) - 1; i >= 0; i-- {
					stack = append(stack, rest[i])
				}
			case "namespace":
				if block != NoNode {
					stack = append(stack, urlFrame{id: block, ctx: URLContextValue})
					block = NoNode
				}
				push(n.FirstChild, URLContextNamespace, true)
			default:
				push(n.FirstChild, URLContextValue, false)
			}
			if block != NoNode {
				bc := d.ParseBlockContents(block)
				if name == "font-face" {
					mark := len(stack)
					for c := d.Nodes[bc].FirstChild; c != NoNode; c = d.Nodes[c].NextSibling {
						ctx := URLContextValue
						if d.Nodes[c].Kind == NodeDeclaration && asciiEqualFold(d.Name(c), "src") {
							ctx = URLContextFontFaceSrc
						}
						stack = append(stack, urlFrame{id: c, ctx: ctx})
					}
					for i, j := mark, len(stack)-1; i < j; i, j = i+1, j-1 {
						stack[i], stack[j] = stack[j], stack[i]
					}
				} else {
					stack = append(stack, urlFrame{id: bc, ctx: URLContextValue})
				}
			}
			// The prelude was pushed first, so the block (pushed later) is
			// visited first; refs are sorted at the end.
		case NodeQualifiedRule:
			if n.Block != NoNode {
				stack = append(stack, urlFrame{id: n.Block, ctx: f.ctx})
			}
			push(n.FirstChild, URLContextValue, false)
		case NodeFunction:
			name := lowerASCII(d.Name(f.id))
			switch name {
			case "url", "src":
				if s := d.firstNonWhitespace(n.FirstChild); s != NoNode && d.preservedIs(s, TokenString) {
					if !add(URLRef{URL: d.TokenOf(s).Value, Form: URLFormFunction, Context: f.ctx, Start: n.Start, End: n.End}) {
						break walk
					}
				}
				push(n.FirstChild, f.ctx, false)
			case "image-set", "-webkit-image-set":
				push(n.FirstChild, URLContextImageSet, true)
			default:
				push(n.FirstChild, f.ctx, false)
			}
		case NodePreserved:
			t := &d.Tokens[n.Token]
			switch {
			case t.Kind == TokenURL:
				if !add(URLRef{URL: t.Value, Form: URLFormToken, Context: f.ctx, Start: t.Start, End: t.End}) {
					break walk
				}
			case t.Kind == TokenString && f.strings:
				if !add(URLRef{URL: t.Value, Form: URLFormString, Context: f.ctx, Start: t.Start, End: t.End}) {
					break walk
				}
			}
		case NodeError:
			push(n.FirstChild, URLContextValue, false)
		default:
			// Roots, declarations, blocks: walk the children with the
			// inherited context.
			push(n.FirstChild, f.ctx, false)
		}
	}
	sort.SliceStable(refs, func(i, j int) bool { return refs[i].Start < refs[j].Start })
	return URLResult{Refs: refs, Grade: gradeOf(diags), Diags: diags}
}

func (d *Document) isWhitespace(id NodeID) bool { return d.preservedIs(id, TokenWhitespace) }

func (d *Document) preservedIs(id NodeID, k TokenKind) bool {
	n := &d.Nodes[id]
	return n.Kind == NodePreserved && d.Tokens[n.Token].Kind == k
}

func (d *Document) firstNonWhitespace(c NodeID) NodeID {
	for c != NoNode && d.isWhitespace(c) {
		c = d.Nodes[c].NextSibling
	}
	return c
}

func lowerASCII(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'A' && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}
