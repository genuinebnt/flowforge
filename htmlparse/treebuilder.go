package htmlparse

import (
	"bytes"
	"slices"
)

// insertionMode is a tree construction insertion mode (HTML §13.2.4.1),
// one constant per mode in the order of §13.2.6.4, whose subsections are
// the modes' rules: §13.2.6.4.1 "initial" ... §13.2.6.4.15 "in cell",
// then §13.2.6.4.16 "in template" ... §13.2.6.4.21 "after after
// frameset". The current standard has no "in select" / "in select in
// table" modes (formerly §13.2.6.4.16-17): they were removed with the
// customizable <select> parser changes (2025) and select is now handled
// by the "in body" rules, which renumbered the modes after "in cell".
type insertionMode uint8

const (
	modeInitial insertionMode = iota
	modeBeforeHTML
	modeBeforeHead
	modeInHead
	modeInHeadNoscript
	modeAfterHead
	modeInBody
	modeText
	modeInTable
	modeInTableText
	modeInCaption
	modeInColumnGroup
	modeInTableBody
	modeInRow
	modeInCell
	modeInTemplate
	modeAfterBody
	modeInFrameset
	modeAfterFrameset
	modeAfterAfterBody
	modeAfterAfterFrameset
	numModes
)

var modeNames = [numModes]string{
	"initial", "before html", "before head", "in head", "in head noscript", "after head",
	"in body", "text", "in table", "in table text", "in caption", "in column group",
	"in table body", "in row", "in cell", "in template", "after body", "in frameset",
	"after frameset", "after after body", "after after frameset",
}

// String returns the spec name of the mode ("in table body", ...).
func (m insertionMode) String() string {
	if m < numModes {
		return modeNames[m]
	}
	return "mode?"
}

// stackEntry is one entry of the stack of open elements (§13.2.4.2). The
// tag, namespace and integration-point flag are cached so the scope
// checks do not touch the arena. The annotation-xml flag must be cached:
// whether it is an HTML integration point depends on the start tag's
// encoding attribute, which the spec ties to the token, not the element.
type stackEntry struct {
	id     NodeID
	tag    tagID
	ns     Namespace
	htmlIP bool // MathML annotation-xml whose encoding makes it an HTML integration point
}

func (e stackEntry) is(tag tagID) bool { return e.ns == HTMLNamespace && e.tag == tag }

// afeEntry is one entry of the list of active formatting elements
// (§13.2.4.3); a marker has id == None. The element's token is not kept:
// reconstruction and the adoption agency clone the existing element's
// name and attributes, which are the token's (formatting elements are
// never adjusted).
type afeEntry struct {
	id  NodeID
	tag tagID
}

// maxReprocess bounds the reprocess loop for one token. The rules never
// need more than a handful of passes; the bound makes totality
// (HTML-INV-02) independent of that argument.
const maxReprocess = 64

// treeBuilder is the tree construction stage (HTML §13.2.6).
type treeBuilder struct {
	doc *Document
	tz  *Tokenizer
	tok *Token // the token being processed
	seg Token  // scratch token for character-token segments

	mode          insertionMode
	origMode      insertionMode
	templateModes []insertionMode
	stack         []stackEntry
	afe           []afeEntry
	head, form    NodeID
	framesetOK    bool
	foster        bool // the foster parenting flag (§13.2.6.1)
	scripting     bool
	ack           bool // self-closing flag acknowledged

	fragment bool
	ctx      stackEntry // fragment context element (when fragment)

	pendingTable      []byte // pending table character tokens, concatenated
	pendingTableSpan  Span
	pendingTableNonWS bool // pendingTable has a character other than ASCII whitespace
	ignoreLF          bool // drop a leading LF of the next token (pre, listing, textarea)
	textPushed        bool // the element that started "text" mode was opened

	// textBuf holds the data of Text nodes that received more than one
	// piece; run copies it into Node.Data at the end. Appending to a Go
	// string would copy it each time (strings are immutable), making
	// long text runs built from many tokens quadratic.
	textBuf map[NodeID][]byte

	// templateHost maps a template contents fragment to its template
	// element plus one (0: not a template contents fragment).
	templateHost map[NodeID]NodeID

	maxDepth    int
	errs        []ParseError
	metaCharset string // encoding declared by the first <meta> the "in head" rules saw that declares one

	sawSelectedContent bool // an HTML selectedcontent element was created
	maxStack           int  // deepest stack of open elements seen (HTML-INV-06)
}

// nodeCapacityHint estimates the arena size for input: about one element
// per '<' plus one text node for every other tag. Sizing Nodes up front
// avoids repeatedly copying the (large) Node structs while the slice
// grows; a wrong guess only costs one extra growth.
func nodeCapacityHint(input []byte) int {
	lt := bytes.Count(input, []byte{'<'})
	return lt + lt/2 + 16
}

func newTreeBuilder(doc *Document, tz *Tokenizer, opts Options) *treeBuilder {
	return &treeBuilder{
		doc: doc, tz: tz, head: None, form: None, framesetOK: true,
		scripting: opts.Scripting,
		maxDepth:  limitOrDefault(opts.MaxDepth, DefaultMaxDepth),
		ctx:       stackEntry{id: None},
	}
}

// run consumes tokens until EOF. Between tokens it updates the
// tokenizer's CDATA flag from the adjusted current node, the only
// tokenizer input that depends on the tree but is not set by a rule.
func (b *treeBuilder) run() {
	for {
		tok := b.tz.Next()
		b.processToken(tok)
		if tok.Kind == EOFToken {
			break
		}
		acn := b.adjustedCurrent()
		b.tz.SetAllowCDATA(acn.id != None && acn.ns != HTMLNamespace)
	}
	for id, buf := range b.textBuf {
		// PORT: Go map iteration order is random; each entry writes only
		// its own node, so the result does not depend on it.
		b.doc.Nodes[id].Data = string(buf)
	}
	b.textBuf = nil
	if b.sawSelectedContent {
		b.mirrorSelectedContent()
	}
}

// parseError records a tree-construction error at the start of the
// token being processed.
func (b *treeBuilder) parseError(code ErrorCode) {
	off := 0
	if b.tok != nil {
		off = b.tok.Span.Start
	}
	b.errs = append(b.errs, ParseError{Code: code, Offset: off})
}

// processToken is the tree construction dispatcher (§13.2.6) plus the
// "ignore a leading LF after pre/listing/textarea" and self-closing
// acknowledgement bookkeeping. The spec ignores the next token if it is
// an LF character token; tokens here are coalesced runs, so the first
// character of the run is dropped instead. Any other token kind clears
// the flag. An unacknowledged self-closing start tag is the
// non-void-html-element-start-tag-with-trailing-solidus error (§13.2.5).
func (b *treeBuilder) processToken(tok *Token) {
	b.tok = tok
	if b.ignoreLF {
		b.ignoreLF = false
		if tok.Kind == CharacterToken && len(tok.Data) > 0 && tok.Data[0] == '\n' {
			tok.Data = tok.Data[1:]
			if tok.Data == "" {
				return
			}
		}
	}
	if tok.Kind == CharacterToken {
		b.processCharacters(tok)
		return
	}
	b.ack = false
	b.dispatch(tok)
	if tok.Kind == StartTagToken && tok.SelfClosing && !b.ack {
		b.parseError(ErrNonVoidHTMLElementStartTagWithTrailingSolidus)
	}
}

// dispatch runs the reprocess loop for tok: each pass picks the
// insertion mode or the foreign-content rules afresh, as "reprocess the
// token" requires.
func (b *treeBuilder) dispatch(tok *Token) {
	for range maxReprocess {
		var again bool
		if b.useForeignRules(tok) {
			again = b.foreignContent(tok)
		} else {
			again = b.processInMode(b.mode, tok)
		}
		if !again {
			return
		}
	}
}

// useForeignRules implements the dispatcher's choice between the current
// insertion mode and the rules for parsing tokens in foreign content
// (§13.2.6): the conditions are tested in the spec's order, and EOF
// always goes to the insertion mode.
func (b *treeBuilder) useForeignRules(tok *Token) bool {
	if len(b.stack) == 0 {
		return false
	}
	acn := b.adjustedCurrent()
	if acn.ns == HTMLNamespace {
		return false
	}
	start := tok.Kind == StartTagToken
	chars := tok.Kind == CharacterToken
	if isMathMLTextIntegrationPoint(acn) {
		if chars || (start && tok.Name != "mglyph" && tok.Name != "malignmark") {
			return false
		}
	}
	if acn.ns == MathMLNamespace && acn.tag == tagAnnotationXml && start && tok.Name == "svg" {
		return false
	}
	if isHTMLIntegrationPoint(acn) && (start || chars) {
		return false
	}
	return tok.Kind != EOFToken
}

// processInMode applies the rules of mode to tok. It returns true when
// the token must be reprocessed (the rules have already switched b.mode).
func (b *treeBuilder) processInMode(mode insertionMode, tok *Token) bool {
	switch mode {
	case modeInitial:
		return b.initialMode(tok)
	case modeBeforeHTML:
		return b.beforeHTMLMode(tok)
	case modeBeforeHead:
		return b.beforeHeadMode(tok)
	case modeInHead:
		return b.inHeadMode(tok)
	case modeInHeadNoscript:
		return b.inHeadNoscriptMode(tok)
	case modeAfterHead:
		return b.afterHeadMode(tok)
	case modeInBody:
		return b.inBodyMode(tok)
	case modeText:
		return b.textMode(tok)
	case modeInTable:
		return b.inTableMode(tok)
	case modeInTableText:
		return b.inTableTextMode(tok)
	case modeInCaption:
		return b.inCaptionMode(tok)
	case modeInColumnGroup:
		return b.inColumnGroupMode(tok)
	case modeInTableBody:
		return b.inTableBodyMode(tok)
	case modeInRow:
		return b.inRowMode(tok)
	case modeInCell:
		return b.inCellMode(tok)
	case modeInTemplate:
		return b.inTemplateMode(tok)
	case modeAfterBody:
		return b.afterBodyMode(tok)
	case modeInFrameset:
		return b.inFramesetMode(tok)
	case modeAfterFrameset:
		return b.afterFramesetMode(tok)
	case modeAfterAfterBody:
		return b.afterAfterBodyMode(tok)
	case modeAfterAfterFrameset:
		return b.afterAfterFramesetMode(tok)
	}
	return false
}

// charClass classifies bytes for splitting character tokens: the rules
// treat whitespace, NUL and other characters differently, so a run is
// processed as homogeneous segments (like html5ever's SplitWhitespace).
func charClass(c byte) uint8 {
	switch c {
	case '\t', '\n', '\f', '\r', ' ':
		return 1
	case 0:
		return 2
	}
	return 0
}

// processCharacters processes a character token. Modes whose rules
// handle any mix of characters in one step (in body, text, in table
// text, foreign content) get the whole run; other modes get homogeneous
// segments.
func (b *treeBuilder) processCharacters(tok *Token) {
	if b.useForeignRules(tok) {
		b.foreignCharacters(tok)
		return
	}
	switch b.mode {
	case modeInBody, modeText, modeInTableText:
		b.dispatch(tok)
		return
	}
	data := tok.Data
	for i := 0; i < len(data); {
		cls := charClass(data[i])
		j := i + 1
		for j < len(data) && charClass(data[j]) == cls {
			j++
		}
		// b.seg is reused for every segment: a local copy would escape to
		// the heap through b.tok.
		b.seg = *tok
		b.seg.Data = data[i:j]
		b.tok = &b.seg
		if b.useForeignRules(&b.seg) {
			b.foreignCharacters(&b.seg)
		} else {
			b.dispatch(&b.seg)
		}
		i = j
	}
	b.tok = tok
}

func isAllSpace(s string) bool {
	for i := 0; i < len(s); i++ {
		if charClass(s[i]) != 1 {
			return false
		}
	}
	return true
}

// --- the stack of open elements (§13.2.4.2) ---

func (b *treeBuilder) current() stackEntry {
	if len(b.stack) == 0 {
		return stackEntry{id: None}
	}
	return b.stack[len(b.stack)-1]
}

// adjustedCurrent is the adjusted current node (§13.2.4.2): the context
// element when parsing a fragment and only the root html element is
// open, otherwise the current node.
func (b *treeBuilder) adjustedCurrent() stackEntry {
	if b.fragment && len(b.stack) == 1 {
		return b.ctx
	}
	return b.current()
}

func (b *treeBuilder) push(e stackEntry) {
	b.stack = append(b.stack, e)
	b.maxStack = max(b.maxStack, len(b.stack))
}

// pop pops the current node. The root html element is never popped: no
// rule pops it while tokens remain (the frameset rules check for it), so
// refusing here only keeps the stack non-empty whatever the input. The
// other stack helpers (removeFromStack, popUntil, popUntilNode) likewise
// never touch index 0.
func (b *treeBuilder) pop() {
	if len(b.stack) > 1 {
		b.stack = b.stack[:len(b.stack)-1]
	}
}

func (b *treeBuilder) stackIndex(id NodeID) int {
	for i := len(b.stack) - 1; i >= 0; i-- {
		if b.stack[i].id == id {
			return i
		}
	}
	return -1
}

func (b *treeBuilder) onStack(id NodeID) bool { return b.stackIndex(id) >= 0 }

func (b *treeBuilder) removeFromStack(id NodeID) {
	if i := b.stackIndex(id); i > 0 {
		b.stack = slices.Delete(b.stack, i, i+1)
	}
}

// popUntil pops elements until an HTML element with one of tags has been
// popped. If there is none above the root it pops nothing.
func (b *treeBuilder) popUntil(tags ...tagID) {
	for i := len(b.stack) - 1; i > 0; i-- {
		e := b.stack[i]
		if e.ns == HTMLNamespace && slices.Contains(tags, e.tag) {
			b.stack = b.stack[:i]
			return
		}
	}
}

// popUntilNode pops elements until id has been popped.
func (b *treeBuilder) popUntilNode(id NodeID) {
	if i := b.stackIndex(id); i > 0 {
		b.stack = b.stack[:i]
	}
}

// templateOnStack reports whether a template element is on the stack.
func (b *treeBuilder) templateOnStack() bool {
	for i := len(b.stack) - 1; i >= 0; i-- {
		if b.stack[i].is(tagTemplate) {
			return true
		}
	}
	return false
}

// parsingTemplateContents: a template element is on the stack, or the
// fragment context element is a template. The form rules use it for the
// spec's "a template element on the stack" test: the expected trees for
// a template context (template.dat, "<form><form>" and
// "<table><form></table>" with context template) require the context to
// count.
func (b *treeBuilder) parsingTemplateContents() bool {
	return b.templateOnStack() || (b.fragment && b.ctx.is(tagTemplate))
}

// scopeKind selects one of the "has an element in ... scope" lists
// (§13.2.4.2).
type scopeKind uint8

const (
	scopeDefault scopeKind = iota
	scopeListItem
	scopeButton
	scopeTable
)

// isScopeBoundary reports whether e terminates the "has an element in
// the specific scope" search for the given scope. The default scope list
// is applet, caption, html, table, td, th, marquee, object, template,
// select (added by the 2025 select changes, which removed select scope),
// MathML mi/mo/mn/ms/mtext/annotation-xml and SVG
// foreignObject/desc/title; list item scope adds HTML ol and ul, button
// scope adds HTML button, and table scope is only html, table and
// template.
func isScopeBoundary(e stackEntry, sc scopeKind) bool {
	switch e.ns {
	case HTMLNamespace:
		switch e.tag {
		case tagHtml, tagTable, tagTemplate:
			return true
		}
		if sc == scopeTable {
			return false
		}
		switch e.tag {
		case tagApplet, tagCaption, tagTd, tagTh, tagMarquee, tagObject, tagSelect:
			return true
		case tagOl, tagUl:
			return sc == scopeListItem
		case tagButton:
			return sc == scopeButton
		}
	case MathMLNamespace:
		if sc == scopeTable {
			return false
		}
		switch e.tag {
		case tagMi, tagMo, tagMn, tagMs, tagMtext, tagAnnotationXml:
			return true
		}
	case SVGNamespace:
		if sc == scopeTable {
			return false
		}
		switch e.tag {
		case tagForeignObject, tagDesc, tagTitle:
			return true
		}
	}
	return false
}

// inScope reports whether the stack has an HTML element with one of tags
// in the given scope. The target test comes before the boundary test, as
// in the spec, so "table in table scope" finds a table.
func (b *treeBuilder) inScope(sc scopeKind, tags ...tagID) bool {
	for i := len(b.stack) - 1; i >= 0; i-- {
		e := b.stack[i]
		if e.ns == HTMLNamespace && slices.Contains(tags, e.tag) {
			return true
		}
		if isScopeBoundary(e, sc) {
			return false
		}
	}
	return false
}

// inScopeName is inScope for an element name that is not an atom.
func (b *treeBuilder) inScopeName(sc scopeKind, tag tagID, name string) bool {
	if tag != tagUnknown {
		return b.inScope(sc, tag)
	}
	for i := len(b.stack) - 1; i >= 0; i-- {
		e := b.stack[i]
		if e.ns == HTMLNamespace && e.tag == tagUnknown && b.doc.Nodes[e.id].Name == name {
			return true
		}
		if isScopeBoundary(e, sc) {
			return false
		}
	}
	return false
}

// nodeInScope reports whether the specific node id is in (default) scope.
func (b *treeBuilder) nodeInScope(id NodeID) bool {
	for i := len(b.stack) - 1; i >= 0; i-- {
		e := b.stack[i]
		if e.id == id {
			return true
		}
		if isScopeBoundary(e, scopeDefault) {
			return false
		}
	}
	return false
}

// isSpecial reports the "special" category (§13.2.4.2).
func isSpecial(e stackEntry) bool {
	switch e.ns {
	case HTMLNamespace:
		switch e.tag {
		case tagAddress, tagApplet, tagArea, tagArticle, tagAside, tagBase, tagBasefont,
			tagBgsound, tagBlockquote, tagBody, tagBr, tagButton, tagCaption, tagCenter,
			tagCol, tagColgroup, tagDd, tagDetails, tagDir, tagDiv, tagDl, tagDt, tagEmbed,
			tagFieldset, tagFigcaption, tagFigure, tagFooter, tagForm, tagFrame, tagFrameset,
			tagH1, tagH2, tagH3, tagH4, tagH5, tagH6, tagHead, tagHeader, tagHgroup, tagHr,
			tagHtml, tagIframe, tagImg, tagInput, tagKeygen, tagLi, tagLink, tagListing,
			tagMain, tagMarquee, tagMenu, tagMeta, tagNav, tagNoembed, tagNoframes,
			tagNoscript, tagObject, tagOl, tagP, tagParam, tagPlaintext, tagPre, tagScript,
			tagSearch, tagSection, tagSelect, tagSource, tagStyle, tagSummary, tagTable,
			tagTbody, tagTd, tagTemplate, tagTextarea, tagTfoot, tagTh, tagThead, tagTitle,
			tagTr, tagTrack, tagUl, tagWbr, tagXmp:
			return true
		}
	case MathMLNamespace:
		switch e.tag {
		case tagMi, tagMo, tagMn, tagMs, tagMtext, tagAnnotationXml:
			return true
		}
	case SVGNamespace:
		switch e.tag {
		case tagForeignObject, tagDesc, tagTitle:
			return true
		}
	}
	return false
}

// isMathMLTextIntegrationPoint reports a MathML mi, mo, mn, ms or mtext
// element (§13.2.6 "MathML text integration point").
func isMathMLTextIntegrationPoint(e stackEntry) bool {
	if e.ns != MathMLNamespace {
		return false
	}
	switch e.tag {
	case tagMi, tagMo, tagMn, tagMs, tagMtext:
		return true
	}
	return false
}

// isHTMLIntegrationPoint reports an HTML integration point (§13.2.6): a
// MathML annotation-xml whose start tag had encoding text/html or
// application/xhtml+xml, or an SVG foreignObject, desc or title.
func isHTMLIntegrationPoint(e stackEntry) bool {
	switch e.ns {
	case MathMLNamespace:
		return e.tag == tagAnnotationXml && e.htmlIP
	case SVGNamespace:
		return e.tag == tagForeignObject || e.tag == tagDesc || e.tag == tagTitle
	}
	return false
}

// impliedEndTag reports the elements "generate implied end tags" pops
// (§13.2.6.3): dd, dt, li, optgroup, option, p, rb, rp, rt, rtc.
func impliedEndTag(tag tagID) bool {
	switch tag {
	case tagDd, tagDt, tagLi, tagOptgroup, tagOption, tagP, tagRb, tagRp, tagRt, tagRtc:
		return true
	}
	return false
}

// generateImpliedEndTags pops implied-end-tag elements except those named
// in except (§13.2.6.3). Only HTML elements qualify: a foreign <p> or
// <option> stops the loop.
func (b *treeBuilder) generateImpliedEndTags(except ...tagID) {
	for {
		c := b.current()
		if c.ns != HTMLNamespace || !impliedEndTag(c.tag) || slices.Contains(except, c.tag) || len(b.stack) <= 1 {
			return
		}
		b.pop()
	}
}

// generateImpliedEndTagsExceptName is generateImpliedEndTags excluding
// HTML elements whose name is name (used for "any other end tag").
func (b *treeBuilder) generateImpliedEndTagsExceptName(name string) {
	for {
		c := b.current()
		if c.ns != HTMLNamespace || !impliedEndTag(c.tag) || tagNames[c.tag] == name || len(b.stack) <= 1 {
			return
		}
		b.pop()
	}
}

// generateAllImpliedEndTagsThoroughly is "generate all implied end tags
// thoroughly" (§13.2.6.3): the implied-end-tag elements plus caption,
// colgroup, tbody, td, tfoot, th, thead and tr.
func (b *treeBuilder) generateAllImpliedEndTagsThoroughly() {
	for {
		c := b.current()
		if c.ns != HTMLNamespace || len(b.stack) <= 1 {
			return
		}
		switch c.tag {
		case tagCaption, tagColgroup, tagTbody, tagTd, tagTfoot, tagTh, tagThead, tagTr:
		default:
			if !impliedEndTag(c.tag) {
				return
			}
		}
		b.pop()
	}
}

// --- the list of active formatting elements (§13.2.4.3) ---

func (b *treeBuilder) afeIndex(id NodeID) int {
	for i := len(b.afe) - 1; i >= 0; i-- {
		if b.afe[i].id == id {
			return i
		}
	}
	return -1
}

func (b *treeBuilder) insertMarker() { b.afe = append(b.afe, afeEntry{id: None}) }

// pushFormatting pushes id onto the list, applying the Noah's Ark clause
// (§13.2.4.3): if three entries after the last marker already have the
// same tag name, namespace and attributes, the earliest of them is
// removed. The clause runs on every push, so there are never more than
// three to count.
func (b *treeBuilder) pushFormatting(id NodeID, tag tagID) {
	n := &b.doc.Nodes[id]
	count, earliest := 0, -1
	for i := len(b.afe) - 1; i >= 0; i-- {
		e := b.afe[i]
		if e.id == None {
			break
		}
		m := &b.doc.Nodes[e.id]
		if e.tag == tag && m.Name == n.Name && m.Namespace == n.Namespace && sameAttrs(m.Attrs, n.Attrs) {
			count++
			earliest = i
		}
	}
	if count >= 3 {
		b.afe = slices.Delete(b.afe, earliest, earliest+1)
	}
	b.afe = append(b.afe, afeEntry{id: id, tag: tag})
}

// sameAttrs compares attribute lists as sets of (namespace, name, value),
// the Noah's Ark notion of "same attributes" (order does not matter).
// Lists are duplicate-free (the tokenizer drops duplicates), so equal
// length plus inclusion is set equality.
func sameAttrs(a, c []Attr) bool {
	if len(a) != len(c) {
		return false
	}
outer:
	for _, x := range a {
		for _, y := range c {
			if x == y {
				continue outer
			}
		}
		return false
	}
	return true
}

// clearToLastMarker clears the list up to the last marker.
func (b *treeBuilder) clearToLastMarker() {
	for len(b.afe) > 0 {
		e := b.afe[len(b.afe)-1]
		b.afe = b.afe[:len(b.afe)-1]
		if e.id == None {
			return
		}
	}
}

// reconstructFormatting is "reconstruct the active formatting elements"
// (§13.2.4.3): rewind from the last entry to the first one after a marker
// or an open element, then for each entry from there to the end create
// a new element for the entry's token (here: a clone of the entry's
// element, carrying its span), insert it, push it and replace the entry.
// When the depth limit is reached the reconstruction stops; the remaining
// entries stay in the list pointing at their old elements and are tried
// again at the next reconstruction.
func (b *treeBuilder) reconstructFormatting() {
	if len(b.afe) == 0 {
		return
	}
	last := b.afe[len(b.afe)-1]
	if last.id == None || b.onStack(last.id) {
		return
	}
	i := len(b.afe) - 1
	for i > 0 {
		e := b.afe[i-1]
		if e.id == None || b.onStack(e.id) {
			break
		}
		i--
	}
	for ; i < len(b.afe); i++ {
		if len(b.stack) >= b.maxDepth {
			return
		}
		old := b.afe[i]
		on := &b.doc.Nodes[old.id]
		id := b.createElement(on.Name, old.tag, HTMLNamespace, slices.Clone(on.Attrs), on.Span)
		b.insertAtLocation(id)
		b.push(stackEntry{id: id, tag: old.tag, ns: HTMLNamespace})
		b.afe[i].id = id
	}
}

// --- creating and inserting nodes (§13.2.6.1) ---

// insertionLocation is "the appropriate place for inserting a node"
// (§13.2.6.1), with an optional override target (id None: the current
// node). It returns (parent, reference child); a None reference means
// "after the parent's last child".
//
// With foster parenting on and a table, tbody, tfoot, thead or tr target,
// the steps are: inside the last template's contents if it is lower on
// the stack than the last table (or there is no table); inside the root
// html element if there is no table (fragment case); before the last
// table if it has a parent; otherwise after the last child of the element
// just above the last table on the stack. Finally a template element as
// the location is replaced by its template contents.
func (b *treeBuilder) insertionLocation(override stackEntry) (NodeID, NodeID) {
	target := override
	if target.id == None {
		target = b.current()
	}
	parent, ref := target.id, None
	if b.foster && target.ns == HTMLNamespace {
		switch target.tag {
		case tagTable, tagTbody, tagTfoot, tagThead, tagTr:
			lastTemplate, lastTable := -1, -1
			for i := len(b.stack) - 1; i >= 0; i-- {
				if lastTemplate < 0 && b.stack[i].is(tagTemplate) {
					lastTemplate = i
				}
				if lastTable < 0 && b.stack[i].is(tagTable) {
					lastTable = i
				}
			}
			switch {
			case lastTemplate >= 0 && lastTemplate > lastTable:
				parent = b.stack[lastTemplate].id
				target = b.stack[lastTemplate]
			case lastTable < 0:
				return b.stack[0].id, None
			case b.doc.Nodes[b.stack[lastTable].id].Parent != None:
				return b.doc.Nodes[b.stack[lastTable].id].Parent, b.stack[lastTable].id
			case lastTable > 0:
				target = b.stack[lastTable-1]
				parent = target.id
			}
		}
	}
	if target.is(tagTemplate) {
		if c := b.doc.Nodes[target.id].Content; c != None {
			return c, None
		}
	}
	return parent, ref
}

// createElement makes a detached element node ("create an element for a
// token" without the side effects, which the callers apply) and records
// a template's contents fragment in templateHost.
func (b *treeBuilder) createElement(name string, tag tagID, ns Namespace, attrs []Attr, span Span) NodeID {
	id := b.doc.NewElement(ns, name, attrs)
	b.doc.Nodes[id].Span = span
	if c := b.doc.Nodes[id].Content; c != None {
		if b.templateHost == nil {
			b.templateHost = map[NodeID]NodeID{}
		}
		b.templateHost[c] = id + 1
	}
	return id
}

// insertAtLocation inserts a created node at the appropriate place.
// Of the DOM's pre-insert validity checks only "a Document has at most
// one element child" is kept (defensive: the insertion modes never put a
// second element there).
func (b *treeBuilder) insertAtLocation(id NodeID) {
	parent, ref := b.insertionLocation(stackEntry{id: None})
	if parent == None {
		return
	}
	if b.doc.Nodes[parent].Kind == DocumentNode && b.doc.Nodes[id].Kind == ElementNode && b.doc.DocumentElement() != None {
		return // a Document has at most one element child
	}
	b.doc.insertBefore(parent, id, ref)
}

// insertElement inserts an element for tok in namespace ns at the
// appropriate place and pushes it. When the stack is at the depth limit
// the element is inserted but not opened (fail closed: it stays empty and
// its would-be content goes to the current node); pushed reports which.
func (b *treeBuilder) insertElement(tok *Token, ns Namespace) (id NodeID, pushed bool) {
	tag := tok.atom
	name := tok.Name
	if ns != HTMLNamespace || tag == tagUnknown {
		tag = lookupTagString(name)
	}
	id = b.createElement(name, tag, ns, tok.Attrs, tok.Span)
	e := stackEntry{id: id, tag: tag, ns: ns}
	if tag == tagSelectedcontent && ns == HTMLNamespace {
		b.sawSelectedContent = true
	}
	if ns == MathMLNamespace && tag == tagAnnotationXml {
		for _, a := range tok.Attrs {
			if a.Namespace == NoNamespace && a.Name == "encoding" &&
				(equalFoldASCII(a.Value, "text/html") || equalFoldASCII(a.Value, "application/xhtml+xml")) {
				e.htmlIP = true
			}
		}
	}
	if ns != HTMLNamespace {
		b.checkXMLNS(id, ns)
	}
	b.insertAtLocation(id)
	if len(b.stack) >= b.maxDepth {
		b.parseError(ErrNestingDepthLimit)
		return id, false
	}
	b.push(e)
	return id, true
}

// insertHTML inserts an HTML element for tok.
func (b *treeBuilder) insertHTML(tok *Token) (NodeID, bool) {
	return b.insertElement(tok, HTMLNamespace)
}

// insertVoid inserts an HTML element for tok and immediately pops it,
// acknowledging the self-closing flag.
func (b *treeBuilder) insertVoid(tok *Token) NodeID {
	id, pushed := b.insertHTML(tok)
	if pushed {
		b.pop()
	}
	b.ack = true
	return id
}

// insertImplied inserts an HTML element for a start tag token with no
// attributes that the parser makes up (html, head, body, tbody, ...).
func (b *treeBuilder) insertImplied(tag tagID) (NodeID, bool) {
	at := 0
	if b.tok != nil {
		at = b.tok.Span.Start
	}
	tok := Token{Kind: StartTagToken, Name: tagNames[tag], atom: tag, Span: Span{at, at}}
	return b.insertHTML(&tok)
}

// insertCharacters is "insert a character" (§13.2.6.1) for a run of
// characters: text at a Document location is dropped, and text right
// after a Text node is appended to it (HTML-INV-10), including when the
// location comes from foster parenting.
func (b *treeBuilder) insertCharacters(s string) {
	if s == "" {
		return
	}
	parent, ref := b.insertionLocation(stackEntry{id: None})
	if parent == None || b.doc.Nodes[parent].Kind == DocumentNode {
		return
	}
	prev := b.doc.Nodes[parent].LastChild
	if ref != None {
		prev = b.doc.Nodes[ref].PrevSibling
	}
	if prev != None && b.doc.Nodes[prev].Kind == TextNode {
		b.appendText(prev, s)
		return
	}
	id := b.doc.NewText(s)
	if b.tok != nil {
		b.doc.Nodes[id].Span = b.tok.Span
	}
	b.doc.insertBefore(parent, id, ref)
}

// appendText appends to a Text node's data in amortized O(len(s)) and
// extends the node's span to the end of the current token.
func (b *treeBuilder) appendText(id NodeID, s string) {
	if b.textBuf == nil {
		b.textBuf = map[NodeID][]byte{}
	}
	buf, ok := b.textBuf[id]
	if !ok {
		buf = make([]byte, 0, 2*(len(b.doc.Nodes[id].Data)+len(s)))
		buf = append(buf, b.doc.Nodes[id].Data...)
	}
	b.textBuf[id] = append(buf, s...)
	if b.tok != nil && b.tok.Span.End > b.doc.Nodes[id].Span.End {
		b.doc.Nodes[id].Span.End = b.tok.Span.End
	}
}

// insertComment inserts a comment for tok at the appropriate place, or as
// the last child of parent when parent is not None.
func (b *treeBuilder) insertComment(tok *Token, parent NodeID) {
	id := b.doc.NewComment(tok.Data)
	b.doc.Nodes[id].Span = tok.Span
	b.insertNodeAt(id, parent)
}

// insertPI inserts a processing instruction for tok, at the same places
// as a comment (parent None: the appropriate place).
func (b *treeBuilder) insertPI(tok *Token, parent NodeID) {
	id := b.doc.NewNode(ProcessingInstructionNode)
	n := &b.doc.Nodes[id]
	n.Name, n.Data, n.Span = tok.Name, tok.Data, tok.Span
	b.insertNodeAt(id, parent)
}

// insertNodeAt appends id to parent, or inserts it at the appropriate
// place when parent is None.
func (b *treeBuilder) insertNodeAt(id, parent NodeID) {
	if parent != None {
		b.doc.insertBefore(parent, id, None)
		return
	}
	p, ref := b.insertionLocation(stackEntry{id: None})
	if p != None {
		b.doc.insertBefore(p, id, ref)
	}
}

// --- resetting the insertion mode (§13.2.4.1) ---

// resetInsertionMode is "reset the insertion mode appropriately": walk the
// stack from the current node down; the bottom entry stands for the
// context element in the fragment case ("last" is true there). The 2025
// standard dropped the select branch with the select modes. Foreign
// elements match no branch.
func (b *treeBuilder) resetInsertionMode() {
	for i := len(b.stack) - 1; i >= 0; i-- {
		node := b.stack[i]
		last := i == 0
		if last && b.fragment {
			node = b.ctx
		}
		if node.ns != HTMLNamespace {
			if last {
				b.mode = modeInBody
				return
			}
			continue
		}
		switch node.tag {
		case tagTd, tagTh:
			if !last {
				b.mode = modeInCell
				return
			}
		case tagTr:
			b.mode = modeInRow
			return
		case tagTbody, tagThead, tagTfoot:
			b.mode = modeInTableBody
			return
		case tagCaption:
			b.mode = modeInCaption
			return
		case tagColgroup:
			b.mode = modeInColumnGroup
			return
		case tagTable:
			b.mode = modeInTable
			return
		case tagTemplate:
			b.mode = b.currentTemplateMode()
			return
		case tagHead:
			if !last {
				b.mode = modeInHead
				return
			}
		case tagBody:
			b.mode = modeInBody
			return
		case tagFrameset:
			b.mode = modeInFrameset
			return
		case tagHtml:
			if b.head == None {
				b.mode = modeBeforeHead
			} else {
				b.mode = modeAfterHead
			}
			return
		}
		if last {
			b.mode = modeInBody
			return
		}
	}
	b.mode = modeInBody
}

// currentTemplateMode is the current template insertion mode (the top of
// the stack of template insertion modes); in body if the stack is empty
// (defensive: every template element on the stack pushed a mode).
func (b *treeBuilder) currentTemplateMode() insertionMode {
	if n := len(b.templateModes); n > 0 {
		return b.templateModes[n-1]
	}
	return modeInBody
}

func (b *treeBuilder) pushTemplateMode(m insertionMode) { b.templateModes = append(b.templateModes, m) }

func (b *treeBuilder) popTemplateMode() {
	if n := len(b.templateModes); n > 0 {
		b.templateModes = b.templateModes[:n-1]
	}
}

// startText switches the tokenizer and enters the "text" insertion mode
// (the generic raw text / RCDATA element parsing algorithms, §13.2.6.2).
// textPushed records whether the element was opened, so the text mode
// does not pop an unrelated element when the depth limit kept it closed.
func (b *treeBuilder) startText(tok *Token, st TokenizerState) {
	_, b.textPushed = b.insertHTML(tok)
	b.tz.SetState(st)
	b.origMode = b.mode
	b.mode = modeText
}
