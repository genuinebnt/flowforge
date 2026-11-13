package content

import (
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/genuinebnt/flowforge/htmlparse"
)

// htmlRules is the html@1 rule table.
var htmlRules = []string{
	"parse:whatwg-html13.2 (htmlparse); valid utf-8 parsed as is, other bytes decoded (bom, transport charset, meta prescan)",
	"emit:one line per start tag, end tag and text run; no indentation; void elements without end tag",
	"fixpoint:output re-parsed and re-emitted until stable (at most 4 rounds; trees that do not round trip)",
	"drop:comments,processing-instructions",
	"text:merge adjacent runs across dropped nodes; collapse ascii whitespace; trim; drop empty",
	"text:escape & < >; attributes: escape & \"",
	"attrs:sort by qualified name (xlink:/xml:/xmlns: prefixes)",
	"attr:class:sort and dedupe tokens",
	"doctype:keep name and ids; quirks without ids gets an extra token to stay quirks",
	"derived:inline script → #script-n (js, json for json/ld+json/importmap/speculationrules, text otherwise); body emptied",
	"derived:inline style → #style-n (css); body emptied",
	"derived:on* attributes and javascript: urls → #handlers (js); value → handler placeholder",
	"element:plaintext → pre data-ff-plaintext",
	"mask:nonce attribute → nonce",
	"mask:input[name in csrf names].value, meta[name in csrf names].content → csrf",
	"mask:input[type=hidden].value unless short numeric/boolean → hidden_value",
	"mask:integrity attribute → build_hash",
	"mask:url attributes (src href action data poster formaction srcset) → cache_bust query values, build_hash asset names",
	"mask:data-* values that are timestamps/uuids/build ids, or whose name says time/nonce/token/request-id → timestamp/nonce/random_id",
	"mask:text iso-8601 date-times → timestamp",
}

// csrfNames are input and meta names that carry anti-CSRF tokens.
var csrfNames = map[string]bool{"csrf": true, "_csrf": true, "csrf_token": true, "csrf-token": true, "csrftoken": true,
	"csrfmiddlewaretoken": true, "xsrf": true, "_xsrf": true, "xsrf-token": true, "xsrf_token": true,
	"authenticity_token": true, "__requestverificationtoken": true, "_token": true, "anticsrf": true,
	"__eventvalidation": true, "__viewstate": true, "__viewstategenerator": true}

// htmlFixpointRounds bounds the re-normalization rounds; real pages
// converge in one.
const htmlFixpointRounds = 4

// voidElements have no end tag (WHATWG §13.1.2).
var voidElements = map[string]bool{"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true,
	"img": true, "input": true, "link": true, "meta": true, "source": true, "track": true, "wbr": true,
	"basefont": true, "bgsound": true, "frame": true, "keygen": true, "param": true}

// rawTextKeep are raw-text elements whose content is re-emitted verbatim
// (collapsed); escaping it would not round trip because raw text has no
// character references.
var rawTextKeep = map[string]bool{"xmp": true, "iframe": true, "noembed": true, "noframes": true}

var urlAttrs = map[string]bool{"src": true, "href": true, "action": true, "data": true, "poster": true,
	"formaction": true, "srcset": true}

// htmlNorm is the state of one HTML normalization.
type htmlNorm struct {
	doc      *htmlparse.Document
	w        lineWriter
	m        *masker
	learned  VolatilitySet
	lim      Limits
	text     strings.Builder // pending text run
	scripts  int
	styles   int
	handlers []string
	derived  []Derived
	dropped  int // derived documents over the limit
}

func normalizeHTML(body []byte, charset string, learned VolatilitySet, lim Limits, m *masker) normResult {
	opts := htmlparse.Options{MaxDepth: MaxHTMLDepth, MaxInputSize: lim.MaxResponseBytes}
	var doc *htmlparse.Document
	var errs []htmlparse.ParseError
	// Valid UTF-8 is parsed as is, so the UTF-8 output re-normalizes the
	// same way whatever charset the original declared (idempotence).
	if utf8.Valid(body) {
		doc, errs = htmlparse.Parse(body, opts)
	} else {
		doc, _, errs = htmlparse.ParseRaw(body, charset, opts)
	}
	h := &htmlNorm{doc: doc, m: m, learned: learned, lim: lim}
	h.node(doc.Root)
	h.flushText()
	r := normResult{text: h.w.bytes(), grade: GradeComplete}
	// Some trees the parser builds do not survive serialize-then-reparse
	// (misnested formatting and list elements, foreign content; htmlparse
	// INV-3 lists them). Re-normalize the output until it is a fixed
	// point, so normalization is idempotent. Masks, handlers and derived
	// documents come from the first round: later rounds only see
	// placeholders and emptied scripts.
	for round := 0; ; round++ {
		if round == htmlFixpointRounds {
			r.diags = append(r.diags, "html_not_converged")
			break
		}
		d2, _ := htmlparse.Parse(r.text, opts)
		h2 := &htmlNorm{doc: d2, m: newMasker(), learned: learned, lim: lim}
		h2.node(d2.Root)
		h2.flushText()
		next := h2.w.bytes()
		if string(next) == string(r.text) {
			break
		}
		r.text = next
	}
	for _, e := range errs {
		switch e.Code {
		case htmlparse.ErrNestingDepthLimit, htmlparse.ErrAttributeLimit, htmlparse.ErrInputSizeLimit:
			r.grade = GradePartial
			r.diags = appendOnce(r.diags, "html_"+strings.ReplaceAll(e.Code.String(), "-", "_"))
		}
	}
	if len(h.handlers) > 0 {
		h.addDerived("#handlers", KindJS, []byte(strings.Join(h.handlers, "")))
	}
	if h.dropped > 0 {
		r.grade = GradePartial
		r.diags = append(r.diags, "inner_document_limit")
	}
	r.derived = h.derived
	return r
}

func appendOnce(s []string, v string) []string {
	if slices.Contains(s, v) {
		return s
	}
	return append(s, v)
}

func (h *htmlNorm) addDerived(suffix string, k Kind, body []byte) {
	if len(h.derived) >= h.lim.MaxInnerDocuments {
		h.dropped++
		return
	}
	h.derived = append(h.derived, Derived{Suffix: suffix, Kind: k, Body: body})
}

// node emits one node and its subtree. Recursion depth is bounded by the
// parser's MaxDepth (open elements), so the call stack is bounded too.
func (h *htmlNorm) node(id htmlparse.NodeID) {
	n := h.doc.Node(id)
	if n == nil {
		return
	}
	switch n.Kind {
	case htmlparse.DocumentNode, htmlparse.DocumentFragmentNode:
		h.children(id)
	case htmlparse.DoctypeNode:
		h.flushText()
		h.w.line(h.doctype(n))
	case htmlparse.TextNode:
		h.text.WriteString(n.Data)
	case htmlparse.ElementNode:
		h.element(id, n)
	}
	// Comments and processing instructions are dropped; the pending text
	// run continues across them, so "a<!--x-->b" and "ab" agree.
}

func (h *htmlNorm) children(id htmlparse.NodeID) {
	for c := range h.doc.Children(id) {
		h.node(c)
	}
}

func (h *htmlNorm) flushText() {
	if h.text.Len() == 0 {
		return
	}
	t := collapseSpace(h.text.String())
	h.text.Reset()
	if t == "" {
		return
	}
	h.w.line(escapeHTMLText(maskText(t, h.m)))
}

func (h *htmlNorm) doctype(n *htmlparse.Node) string {
	var b strings.Builder
	b.WriteString("<!DOCTYPE")
	if n.Name != "" {
		b.WriteString(" " + n.Name)
	}
	quote := func(s string) string {
		if strings.Contains(s, `"`) {
			return "'" + s + "'"
		}
		return `"` + s + `"`
	}
	switch {
	case n.PublicID != "":
		b.WriteString(" PUBLIC " + quote(n.PublicID))
		if n.SystemID != "" {
			b.WriteString(" " + quote(n.SystemID))
		}
	case n.SystemID != "":
		b.WriteString(" SYSTEM " + quote(n.SystemID))
	case h.doc.Mode == htmlparse.Quirks && n.Name != "":
		// The original was forced into quirks mode by a malformed doctype
		// (WHATWG §13.2.5.56: anything after the name that is not PUBLIC
		// or SYSTEM sets force-quirks). Keep it quirks on re-parse, or
		// table and paragraph handling would change.
		b.WriteString(" x")
	}
	b.WriteByte('>')
	return b.String()
}

func qualifiedAttrName(a htmlparse.Attr) string {
	switch a.Namespace {
	case htmlparse.XLinkNamespace:
		return "xlink:" + a.Name
	case htmlparse.XMLNamespace:
		return "xml:" + a.Name
	case htmlparse.XMLNSNamespace:
		if a.Name == "xmlns" {
			return "xmlns"
		}
		return "xmlns:" + a.Name
	}
	return a.Name
}

func (h *htmlNorm) element(id htmlparse.NodeID, n *htmlparse.Node) {
	h.flushText()
	name := n.Name
	tag := name
	if n.Namespace == htmlparse.HTMLNamespace && name == "plaintext" {
		// PLAINTEXT swallows the rest of the input, end tags included, so
		// it cannot round trip; emit it as a pre that says what it was.
		tag = "pre"
	}
	isScript := name == "script"
	isStyle := name == "style"
	_, hasSrc := h.doc.Attr(id, "src")
	inline := ""
	if (isScript && !hasSrc) || isStyle {
		inline = h.doc.TextContent(id)
	}
	h.w.line(h.startTag(tag, name, n, id))
	switch {
	case isScript:
		if !hasSrc && strings.TrimSpace(inline) != "" {
			h.scripts++
			typ, _ := h.doc.Attr(id, "type")
			h.addDerived("#script-"+strconv.Itoa(h.scripts), scriptKind(typ), []byte(inline))
		}
	case isStyle:
		if strings.TrimSpace(inline) != "" {
			h.styles++
			h.addDerived("#style-"+strconv.Itoa(h.styles), KindCSS, []byte(inline))
		}
	case n.Namespace == htmlparse.HTMLNamespace && rawTextKeep[name]:
		if t := collapseSpace(h.doc.TextContent(id)); t != "" {
			h.w.line(t)
		}
	case n.Namespace == htmlparse.HTMLNamespace && name == "template" && n.Content != htmlparse.None:
		h.children(n.Content)
		h.flushText()
	default:
		h.children(id)
		h.flushText()
	}
	if !(n.Namespace == htmlparse.HTMLNamespace && voidElements[name]) {
		h.w.line("</" + tag + ">")
	}
}

// scriptKind maps a script type attribute to the derived document kind
// (WHATWG §4.12.1: a JavaScript MIME type or "module" is script; data
// blocks are anything else).
func scriptKind(typ string) Kind {
	t := strings.ToLower(strings.TrimSpace(typ))
	switch t {
	case "", "module", "text/javascript", "application/javascript", "text/ecmascript", "application/ecmascript",
		"application/x-javascript", "text/jscript", "text/babel":
		return KindJS
	case "application/json", "application/ld+json", "importmap", "speculationrules", "text/json":
		return KindJSON
	}
	return KindText
}

type outAttr struct{ name, value string }

func (h *htmlNorm) startTag(tag, name string, n *htmlparse.Node, id htmlparse.NodeID) string {
	inputName, _ := h.doc.Attr(id, "name")
	inputName = strings.ToLower(inputName)
	typ, _ := h.doc.Attr(id, "type")
	typ = strings.ToLower(strings.TrimSpace(typ))
	attrs := make([]outAttr, 0, len(n.Attrs)+1)
	for _, a := range n.Attrs {
		q := qualifiedAttrName(a)
		attrs = append(attrs, outAttr{q, h.attrValue(name, q, a.Value, inputName, typ)})
	}
	if tag != name {
		attrs = append(attrs, outAttr{"data-ff-" + name, ""})
	}
	// PORT: names are unique (the parser keeps the first of duplicate
	// attributes) except when a renamed element already carries the
	// data-ff-<name> marker. That tie is ordered by Go's unstable
	// slices.SortFunc, which is insertion sort (input order) up to 12
	// attributes; a stable sort reproduces it for real inputs.
	slices.SortFunc(attrs, func(a, b outAttr) int { return strings.Compare(a.name, b.name) })
	var b strings.Builder
	b.WriteString("<" + tag)
	for _, a := range attrs {
		b.WriteString(" " + a.name + `="` + escapeHTMLAttr(a.value) + `"`)
	}
	b.WriteByte('>')
	return b.String()
}

// attrValue applies the html@1 attribute rules.
func (h *htmlNorm) attrValue(tag, attr, v, inputName, typ string) string {
	switch {
	case strings.HasPrefix(attr, "on") && len(attr) > 2:
		return h.handler(tag, attr, v)
	case attr == "nonce" && v != "":
		h.m.hit(MaskNonce, v)
		return maskPlaceholder(MaskNonce)
	case attr == "integrity" && v != "":
		h.m.hit(MaskBuildHash, v)
		return maskPlaceholder(MaskBuildHash)
	case attr == "class":
		// PORT: strings.Fields splits on Unicode White_Space (NBSP and
		// U+0085 included), not only HTML's ASCII whitespace; Rust's
		// str::split_whitespace uses the same property.
		cls := strings.Fields(v)
		slices.Sort(cls)
		return strings.Join(slices.Compact(cls), " ")
	case h.learned.htmlAttrRule(tag, attr) && v != "" && v != maskPlaceholder(MaskLearned):
		h.m.hit(MaskLearned, v)
		return maskPlaceholder(MaskLearned)
	}
	if tag == "input" && attr == "value" {
		if csrfNames[inputName] {
			return h.maskValue(MaskCSRF, v)
		}
		if typ == "hidden" && !shortScalar(v) {
			return h.maskValue(MaskHidden, v)
		}
	}
	if tag == "meta" && attr == "content" && csrfNames[inputName] {
		return h.maskValue(MaskCSRF, v)
	}
	if urlAttrs[attr] {
		if rest, ok := cutPrefixFold(strings.TrimSpace(v), "javascript:"); ok {
			if rest == maskPlaceholder(MaskHandler) {
				return v
			}
			h.handler(tag, attr, rest)
			return "javascript:" + maskPlaceholder(MaskHandler)
		}
		if attr == "srcset" {
			parts := strings.Split(v, ",")
			for i, p := range parts {
				fs := strings.Fields(p)
				if len(fs) > 0 {
					fs[0] = maskURL(fs[0], h.m)
				}
				parts[i] = strings.Join(fs, " ")
			}
			return strings.Join(parts, ", ")
		}
		return maskURL(v, h.m)
	}
	if strings.HasPrefix(attr, "data-") && v != "" && !isPlaceholder(v) {
		if c, ok := volatileScalar(v); ok {
			return h.maskValue(c, v)
		}
		for _, w := range []struct {
			word string
			c    MaskClass
		}{{"timestamp", MaskTimestamp}, {"time", MaskTimestamp}, {"nonce", MaskNonce}, {"csrf", MaskCSRF},
			{"token", MaskCSRF}, {"request-id", MaskRandomID}, {"requestid", MaskRandomID}, {"trace", MaskRandomID}} {
			if strings.Contains(attr, w.word) {
				return h.maskValue(w.c, v)
			}
		}
	}
	return v
}

func (h *htmlNorm) maskValue(c MaskClass, v string) string {
	if v == "" || v == maskPlaceholder(c) {
		return v
	}
	h.m.hit(c, v)
	return maskPlaceholder(c)
}

// handler moves an event-handler body to the #handlers document and
// returns the placeholder that stays in the page.
func (h *htmlNorm) handler(tag, attr, v string) string {
	if strings.TrimSpace(v) == "" || v == maskPlaceholder(MaskHandler) {
		return v
	}
	h.m.hit(MaskHandler, v)
	n := len(h.handlers) + 1
	h.handlers = append(h.handlers, "function "+jsIdentPart(tag)+"_"+jsIdentPart(attr)+"_"+strconv.Itoa(n)+"(event) {\n"+v+"\n}\n")
	return maskPlaceholder(MaskHandler)
}

// jsIdentPart keeps [A-Za-z0-9_] so the handler wrapper name is an
// identifier.
func jsIdentPart(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// shortScalar is a hidden-field value that may be kept (V2 §12.11): a
// short number or a boolean word.
func shortScalar(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || len(v) <= 6 && isAllDigits(v) {
		return true
	}
	switch strings.ToLower(v) {
	case "true", "false", "yes", "no", "on", "off":
		return true
	}
	return false
}

func cutPrefixFold(s, p string) (string, bool) {
	if len(s) >= len(p) && strings.EqualFold(s[:len(p)], p) {
		return s[len(p):], true
	}
	return s, false
}

func escapeHTMLText(s string) string {
	if !strings.ContainsAny(s, "&<>") {
		return s
	}
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func escapeHTMLAttr(s string) string {
	if !strings.ContainsAny(s, `&"`) {
		return s
	}
	r := strings.NewReplacer("&", "&amp;", `"`, "&quot;")
	return r.Replace(s)
}
