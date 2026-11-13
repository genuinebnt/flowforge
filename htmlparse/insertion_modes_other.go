package htmlparse

// Remaining insertion modes of HTML §13.2.6.4 ("in template", "after
// body", "in frameset", "after frameset", "after after body", "after
// after frameset").

// inTemplateMode is the "in template" insertion mode.
func (b *treeBuilder) inTemplateMode(tok *Token) bool {
	switch tok.Kind {
	case CharacterToken, CommentToken, ProcessingInstructionToken, DoctypeToken:
		return b.inBodyMode(tok)
	case StartTagToken:
		var m insertionMode
		switch tok.atom {
		case tagBase, tagBasefont, tagBgsound, tagLink, tagMeta, tagNoframes, tagScript, tagStyle, tagTemplate, tagTitle:
			return b.inHeadMode(tok)
		case tagCaption, tagColgroup, tagTbody, tagTfoot, tagThead:
			m = modeInTable
		case tagCol:
			m = modeInColumnGroup
		case tagTr:
			m = modeInTableBody
		case tagTd, tagTh:
			m = modeInRow
		default:
			m = modeInBody
		}
		b.popTemplateMode()
		b.pushTemplateMode(m)
		b.mode = m
		return true
	case EndTagToken:
		if tok.atom == tagTemplate {
			return b.inHeadMode(tok)
		}
		b.parseError(ErrUnexpectedEndTag)
		return false
	case EOFToken:
		if !b.templateOnStack() {
			return false // stop parsing (fragment case)
		}
		b.parseError(ErrEOFInElement)
		b.popUntil(tagTemplate)
		b.clearToLastMarker()
		b.popTemplateMode()
		b.resetInsertionMode()
		return true
	}
	return false
}

// afterBodyMode is the "after body" insertion mode.
func (b *treeBuilder) afterBodyMode(tok *Token) bool {
	switch tok.Kind {
	case CharacterToken:
		if isAllSpace(tok.Data) {
			return b.inBodyMode(tok)
		}
	case CommentToken:
		b.insertComment(tok, b.stack[0].id)
		return false
	case ProcessingInstructionToken:
		b.insertPI(tok, b.stack[0].id)
		return false
	case DoctypeToken:
		b.parseError(ErrUnexpectedDoctype)
		return false
	case StartTagToken:
		if tok.atom == tagHtml {
			return b.inBodyMode(tok)
		}
	case EndTagToken:
		if tok.atom == tagHtml {
			if b.fragment {
				b.parseError(ErrUnexpectedEndTag)
				return false
			}
			b.mode = modeAfterAfterBody
			return false
		}
	case EOFToken:
		return false
	}
	b.parseError(ErrUnexpectedStartTag)
	b.mode = modeInBody
	return true
}

// inFramesetMode is the "in frameset" insertion mode.
func (b *treeBuilder) inFramesetMode(tok *Token) bool {
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
		case tagFrameset:
			b.insertHTML(tok)
			return false
		case tagFrame:
			b.insertVoid(tok)
			return false
		case tagNoframes:
			return b.inHeadMode(tok)
		}
	case EndTagToken:
		if tok.atom == tagFrameset {
			if len(b.stack) <= 1 {
				b.parseError(ErrUnexpectedEndTag)
				return false
			}
			b.pop()
			if !b.fragment && !b.current().is(tagFrameset) {
				b.mode = modeAfterFrameset
			}
			return false
		}
	case EOFToken:
		if len(b.stack) > 1 {
			b.parseError(ErrEOFInElement)
		}
		return false
	}
	b.parseError(ErrUnexpectedCharacter)
	return false
}

// afterFramesetMode is the "after frameset" insertion mode.
func (b *treeBuilder) afterFramesetMode(tok *Token) bool {
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
		case tagNoframes:
			return b.inHeadMode(tok)
		}
	case EndTagToken:
		if tok.atom == tagHtml {
			b.mode = modeAfterAfterFrameset
			return false
		}
	case EOFToken:
		return false
	}
	b.parseError(ErrUnexpectedCharacter)
	return false
}

// afterAfterBodyMode is the "after after body" insertion mode.
func (b *treeBuilder) afterAfterBodyMode(tok *Token) bool {
	switch tok.Kind {
	case CommentToken:
		b.insertComment(tok, b.doc.Root)
		return false
	case ProcessingInstructionToken:
		b.insertPI(tok, b.doc.Root)
		return false
	case DoctypeToken:
		return b.inBodyMode(tok)
	case CharacterToken:
		if isAllSpace(tok.Data) {
			return b.inBodyMode(tok)
		}
	case StartTagToken:
		if tok.atom == tagHtml {
			return b.inBodyMode(tok)
		}
	case EOFToken:
		return false
	}
	b.parseError(ErrUnexpectedStartTag)
	b.mode = modeInBody
	return true
}

// afterAfterFramesetMode is the "after after frameset" insertion mode.
func (b *treeBuilder) afterAfterFramesetMode(tok *Token) bool {
	switch tok.Kind {
	case CommentToken:
		b.insertComment(tok, b.doc.Root)
		return false
	case ProcessingInstructionToken:
		b.insertPI(tok, b.doc.Root)
		return false
	case DoctypeToken:
		return b.inBodyMode(tok)
	case CharacterToken:
		if isAllSpace(tok.Data) {
			return b.inBodyMode(tok)
		}
	case StartTagToken:
		switch tok.atom {
		case tagHtml:
			return b.inBodyMode(tok)
		case tagNoframes:
			return b.inHeadMode(tok)
		}
	case EOFToken:
		return false
	}
	b.parseError(ErrUnexpectedCharacter)
	return false
}
