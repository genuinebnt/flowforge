package htmlparse

// Table insertion modes: HTML §13.2.6.4.9-15 ("in table", "in table
// text", "in caption", "in column group", "in table body", "in row",
// "in cell"), including foster parenting.

// clearStackBack pops until the current node is an HTML element with one
// of tags, or the root html element: the "clear the stack back to a
// table / table body / table row context" steps (each list includes
// template and html).
func (b *treeBuilder) clearStackBack(tags ...tagID) {
	for len(b.stack) > 1 {
		c := b.current()
		if c.ns == HTMLNamespace {
			found := c.tag == tagHtml
			for _, t := range tags {
				if c.tag == t {
					found = true
				}
			}
			if found {
				return
			}
		}
		b.pop()
	}
}

func (b *treeBuilder) clearToTableContext() { b.clearStackBack(tagTable, tagTemplate) }
func (b *treeBuilder) clearToTableBodyContext() {
	b.clearStackBack(tagTbody, tagTfoot, tagThead, tagTemplate)
}
func (b *treeBuilder) clearToTableRowContext() { b.clearStackBack(tagTr, tagTemplate) }

// inTableMode is the "in table" insertion mode (§13.2.6.4.9).
func (b *treeBuilder) inTableMode(tok *Token) bool {
	switch tok.Kind {
	case CharacterToken:
		// Text directly in a table is collected by "in table text",
		// which decides between inserting it (all whitespace) and foster
		// parenting it. Elsewhere it is "anything else".
		if c := b.current(); c.ns == HTMLNamespace {
			switch c.tag {
			case tagTable, tagTbody, tagTemplate, tagTfoot, tagThead, tagTr:
				b.pendingTable = b.pendingTable[:0]
				b.pendingTableNonWS = false
				b.pendingTableSpan = tok.Span
				b.origMode = b.mode
				b.mode = modeInTableText
				return true
			}
		}
	case CommentToken:
		b.insertComment(tok, None)
		return false
	case ProcessingInstructionToken:
		b.insertPI(tok, None)
		return false
	case DoctypeToken:
		b.parseError(ErrUnexpectedDoctype)
		return false
	case StartTagToken:
		switch tok.atom {
		case tagCaption:
			b.clearToTableContext()
			b.insertMarker()
			b.insertHTML(tok)
			b.mode = modeInCaption
			return false
		case tagColgroup:
			b.clearToTableContext()
			b.insertHTML(tok)
			b.mode = modeInColumnGroup
			return false
		case tagCol:
			b.clearToTableContext()
			b.insertImplied(tagColgroup)
			b.mode = modeInColumnGroup
			return true
		case tagTbody, tagTfoot, tagThead:
			b.clearToTableContext()
			b.insertHTML(tok)
			b.mode = modeInTableBody
			return false
		case tagTd, tagTh, tagTr:
			b.clearToTableContext()
			b.insertImplied(tagTbody)
			b.mode = modeInTableBody
			return true
		case tagTable:
			b.parseError(ErrUnexpectedStartTag)
			if !b.inScope(scopeTable, tagTable) {
				return false
			}
			b.popUntil(tagTable)
			b.resetInsertionMode()
			return true
		case tagStyle, tagScript, tagTemplate:
			return b.inHeadMode(tok)
		case tagInput:
			if isHiddenInput(tok) {
				b.parseError(ErrUnexpectedStartTag)
				b.insertVoid(tok)
				return false
			}
		case tagForm:
			// A form in a table becomes an empty element (inserted and
			// popped at once); its content goes elsewhere. Inside
			// template contents it neither consults nor sets the form
			// element pointer (template.dat, "<template><table><form>").
			b.parseError(ErrUnexpectedStartTag)
			parsingTemplate := b.parsingTemplateContents()
			if b.form != None && !parsingTemplate {
				return false
			}
			id, pushed := b.insertHTML(tok)
			if !parsingTemplate {
				b.form = id
			}
			if pushed {
				b.pop()
			}
			return false
		}
	case EndTagToken:
		switch tok.atom {
		case tagTable:
			if !b.inScope(scopeTable, tagTable) {
				b.parseError(ErrUnexpectedEndTag)
				return false
			}
			b.popUntil(tagTable)
			b.resetInsertionMode()
			return false
		case tagBody, tagCaption, tagCol, tagColgroup, tagHtml, tagTbody, tagTd, tagTfoot, tagTh, tagThead, tagTr:
			b.parseError(ErrUnexpectedEndTag)
			return false
		case tagTemplate:
			return b.inHeadMode(tok)
		}
	case EOFToken:
		return b.inBodyMode(tok)
	}
	// Anything else: foster parenting.
	b.parseError(ErrUnexpectedStartTag)
	return b.fosterInBody(tok)
}

// fosterInBody processes tok with the "in body" rules with foster
// parenting enabled: elements and text whose insertion location would be
// a table, tbody, tfoot, thead or tr go before the table instead
// (insertionLocation).
func (b *treeBuilder) fosterInBody(tok *Token) bool {
	b.foster = true
	again := b.inBodyMode(tok)
	b.foster = false
	return again
}

// inTableTextMode is the "in table text" insertion mode (§13.2.6.4.10).
// Character tokens accumulate (NULs dropped with an error); the first
// other token flushes them, with one error and foster parenting if any
// is not whitespace, and is reprocessed in the original mode. The
// pending run keeps one span from its first to its last character token.
func (b *treeBuilder) inTableTextMode(tok *Token) bool {
	if tok.Kind == CharacterToken {
		s := tok.Data
		for i := 0; i < len(s); i++ {
			switch charClass(s[i]) {
			case 2:
				b.parseError(ErrUnexpectedNullCharacter)
				continue
			case 0:
				b.pendingTableNonWS = true
			}
			b.pendingTable = append(b.pendingTable, s[i])
		}
		if tok.Span.End > b.pendingTableSpan.End {
			b.pendingTableSpan.End = tok.Span.End
		}
		return false
	}
	if len(b.pendingTable) > 0 {
		pending := Token{Kind: CharacterToken, Data: string(b.pendingTable), Span: b.pendingTableSpan}
		cur := b.tok
		b.tok = &pending
		if b.pendingTableNonWS {
			// Reprocess with the "anything else" rules of "in table".
			b.parseError(ErrUnexpectedCharacter)
			b.fosterInBody(&pending)
		} else {
			b.insertCharacters(pending.Data)
		}
		b.tok = cur
	}
	b.pendingTable = b.pendingTable[:0]
	b.pendingTableNonWS = false
	b.mode = b.origMode
	return true
}

// inCaptionMode is the "in caption" insertion mode (§13.2.6.4.11). Table
// structure tags close the caption (and are reprocessed, except
// </caption>); everything else uses "in body".
func (b *treeBuilder) inCaptionMode(tok *Token) bool {
	switch {
	case isEnd(tok, tagCaption), isEnd(tok, tagTable),
		isStart(tok, tagCaption, tagCol, tagColgroup, tagTbody, tagTd, tagTfoot, tagTh, tagThead, tagTr):
		if !b.inScope(scopeTable, tagCaption) {
			b.parseError(ErrUnexpectedEndTag)
			return false
		}
		b.generateImpliedEndTags()
		if !b.current().is(tagCaption) {
			b.parseError(ErrUnclosedElement)
		}
		b.popUntil(tagCaption)
		b.clearToLastMarker()
		b.mode = modeInTable
		return !isEnd(tok, tagCaption)
	case isEnd(tok, tagBody, tagCol, tagColgroup, tagHtml, tagTbody, tagTd, tagTfoot, tagTh, tagThead, tagTr):
		b.parseError(ErrUnexpectedEndTag)
		return false
	}
	return b.inBodyMode(tok)
}

// inColumnGroupMode is the "in column group" insertion mode
// (§13.2.6.4.12). Character tokens arrive as homogeneous segments, so
// leading whitespace is inserted and the rest closes the colgroup.
func (b *treeBuilder) inColumnGroupMode(tok *Token) bool {
	switch tok.Kind {
	case CharacterToken:
		if isAllSpace(tok.Data) {
			b.insertCharacters(tok.Data)
			return false
		}
	case CommentToken:
		b.insertComment(tok, None)
		return false
	case ProcessingInstructionToken:
		b.insertPI(tok, None)
		return false
	case DoctypeToken:
		b.parseError(ErrUnexpectedDoctype)
		return false
	case StartTagToken:
		switch tok.atom {
		case tagHtml:
			return b.inBodyMode(tok)
		case tagCol:
			b.insertVoid(tok)
			return false
		case tagTemplate:
			return b.inHeadMode(tok)
		}
	case EndTagToken:
		switch tok.atom {
		case tagColgroup:
			if !b.current().is(tagColgroup) {
				b.parseError(ErrUnexpectedEndTag)
				return false
			}
			b.pop()
			b.mode = modeInTable
			return false
		case tagCol:
			b.parseError(ErrUnexpectedEndTag)
			return false
		case tagTemplate:
			return b.inHeadMode(tok)
		}
	case EOFToken:
		return b.inBodyMode(tok)
	}
	if !b.current().is(tagColgroup) {
		b.parseError(ErrUnexpectedStartTag)
		return false
	}
	b.pop()
	b.mode = modeInTable
	return true
}

// inTableBodyMode is the "in table body" insertion mode (§13.2.6.4.13).
func (b *treeBuilder) inTableBodyMode(tok *Token) bool {
	switch {
	case isStart(tok, tagTr):
		b.clearToTableBodyContext()
		b.insertHTML(tok)
		b.mode = modeInRow
		return false
	case isStart(tok, tagTh, tagTd):
		b.parseError(ErrUnexpectedStartTag)
		b.clearToTableBodyContext()
		b.insertImplied(tagTr)
		b.mode = modeInRow
		return true
	case isEnd(tok, tagTbody, tagTfoot, tagThead):
		if !b.inScope(scopeTable, tok.atom) {
			b.parseError(ErrUnexpectedEndTag)
			return false
		}
		b.clearToTableBodyContext()
		b.pop()
		b.mode = modeInTable
		return false
	case isStart(tok, tagCaption, tagCol, tagColgroup, tagTbody, tagTfoot, tagThead), isEnd(tok, tagTable):
		if !b.inScope(scopeTable, tagTbody, tagThead, tagTfoot) {
			b.parseError(ErrUnexpectedEndTag)
			return false
		}
		b.clearToTableBodyContext()
		b.pop()
		b.mode = modeInTable
		return true
	case isEnd(tok, tagBody, tagCaption, tagCol, tagColgroup, tagHtml, tagTd, tagTh, tagTr):
		b.parseError(ErrUnexpectedEndTag)
		return false
	}
	return b.inTableMode(tok)
}

// inRowMode is the "in row" insertion mode (§13.2.6.4.14).
func (b *treeBuilder) inRowMode(tok *Token) bool {
	switch {
	case isStart(tok, tagTh, tagTd):
		b.clearToTableRowContext()
		b.insertHTML(tok)
		b.mode = modeInCell
		b.insertMarker()
		return false
	case isEnd(tok, tagTr):
		if !b.inScope(scopeTable, tagTr) {
			b.parseError(ErrUnexpectedEndTag)
			return false
		}
		b.clearToTableRowContext()
		b.pop()
		b.mode = modeInTableBody
		return false
	case isStart(tok, tagCaption, tagCol, tagColgroup, tagTbody, tagTfoot, tagThead, tagTr), isEnd(tok, tagTable):
		if !b.inScope(scopeTable, tagTr) {
			b.parseError(ErrUnexpectedEndTag)
			return false
		}
		b.clearToTableRowContext()
		b.pop()
		b.mode = modeInTableBody
		return true
	case isEnd(tok, tagTbody, tagTfoot, tagThead):
		if !b.inScope(scopeTable, tok.atom) {
			b.parseError(ErrUnexpectedEndTag)
			return false
		}
		if !b.inScope(scopeTable, tagTr) {
			return false // no tr in table scope: ignored without an error
		}
		b.clearToTableRowContext()
		b.pop()
		b.mode = modeInTableBody
		return true
	case isEnd(tok, tagBody, tagCaption, tagCol, tagColgroup, tagHtml, tagTd, tagTh):
		b.parseError(ErrUnexpectedEndTag)
		return false
	}
	return b.inTableMode(tok)
}

// inCellMode is the "in cell" insertion mode (§13.2.6.4.15).
func (b *treeBuilder) inCellMode(tok *Token) bool {
	switch {
	case isEnd(tok, tagTd, tagTh):
		if !b.inScope(scopeTable, tok.atom) {
			b.parseError(ErrUnexpectedEndTag)
			return false
		}
		b.generateImpliedEndTags()
		if !b.current().is(tok.atom) {
			b.parseError(ErrUnclosedElement)
		}
		b.popUntil(tok.atom)
		b.clearToLastMarker()
		b.mode = modeInRow
		return false
	case isStart(tok, tagCaption, tagCol, tagColgroup, tagTbody, tagTd, tagTfoot, tagTh, tagThead, tagTr):
		if !b.inScope(scopeTable, tagTd, tagTh) {
			// Only the fragment case gets here (a td or th context
			// element, so no cell is on the stack): ignore the token.
			b.parseError(ErrUnexpectedStartTag)
			return false
		}
		b.closeCell()
		return true
	case isEnd(tok, tagBody, tagCaption, tagCol, tagColgroup, tagHtml):
		b.parseError(ErrUnexpectedEndTag)
		return false
	case isEnd(tok, tagTable, tagTbody, tagTfoot, tagThead, tagTr):
		if !b.inScope(scopeTable, tok.atom) {
			b.parseError(ErrUnexpectedEndTag)
			return false
		}
		b.closeCell()
		return true
	}
	return b.inBodyMode(tok)
}

// closeCell is "close the cell" (§13.2.6.4.15).
func (b *treeBuilder) closeCell() {
	b.generateImpliedEndTags()
	if c := b.current(); !c.is(tagTd) && !c.is(tagTh) {
		b.parseError(ErrUnclosedElement)
	}
	b.popUntil(tagTd, tagTh)
	b.clearToLastMarker()
	b.mode = modeInRow
}
