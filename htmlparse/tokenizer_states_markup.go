package htmlparse

// Markup declaration states: HTML §13.2.5.41-71 (bogus comment, markup
// declaration open, the comment states, the DOCTYPE states, the CDATA
// section states) and the five processing instruction states of the 2025
// standard, which follow the CDATA states and are cited by name (see the
// state type).

// bogusCommentState is the bogus comment state (§13.2.5.41).
func (t *Tokenizer) bogusCommentState() {
	for {
		c := t.next()
		switch c {
		case '>':
			t.state = stateData
			t.emitComment()
			return
		case eofRune:
			t.emitComment()
			t.emitEOF()
			return
		case 0:
			t.err(ErrUnexpectedNullCharacter)
			t.data = append(t.data, "�"...)
		default:
			t.data = appendRune(t.data, c)
		}
	}
}

// markupDeclarationOpenState is the markup declaration open state
// (§13.2.5.42).
func (t *Tokenizer) markupDeclarationOpenState() {
	t.data = t.data[:0]
	switch {
	case t.peekFold(t.pos, "--"):
		t.skipASCII(2)
		t.state = stateCommentStart
	case t.peekFold(t.pos, "doctype"):
		t.skipASCII(7)
		t.state = stateDOCTYPE
	// "[CDATA[" is matched case-sensitively, unlike "--" and "DOCTYPE".
	case len(t.in)-t.pos >= 7 && string(t.in[t.pos:t.pos+7]) == "[CDATA[":
		t.skipASCII(7)
		if t.allowCDATA {
			t.state = stateCDATASection
			return
		}
		t.err(ErrCDATAInHTMLContent) // at the last '[' consumed
		t.data = append(t.data, "[CDATA["...)
		t.state = stateBogusComment
	default:
		// Read (and un-read) the next character first so its
		// input-stream errors are reported before this one, as
		// preprocessing happens before tokenization.
		t.next()
		t.reconsume()
		t.errAt(ErrIncorrectlyOpenedComment, t.pos)
		t.state = stateBogusComment
	}
}

// commentStartState is the comment start state (§13.2.5.43).
func (t *Tokenizer) commentStartState() {
	switch c := t.next(); c {
	case '-':
		t.state = stateCommentStartDash
	case '>':
		t.err(ErrAbruptClosingOfEmptyComment)
		t.state = stateData
		t.emitComment()
	default:
		t.reconsume()
		t.state = stateComment
	}
}

// commentStartDashState is the comment start dash state (§13.2.5.44).
func (t *Tokenizer) commentStartDashState() {
	switch c := t.next(); c {
	case '-':
		t.state = stateCommentEnd
	case '>':
		t.err(ErrAbruptClosingOfEmptyComment)
		t.state = stateData
		t.emitComment()
	case eofRune:
		t.err(ErrEOFInComment)
		t.emitComment()
		t.emitEOF()
	default:
		t.data = append(t.data, '-')
		t.reconsume()
		t.state = stateComment
	}
}

// commentState is the comment state (§13.2.5.45).
func (t *Tokenizer) commentState() {
	for {
		// Fast path: plain ASCII other than '<' and '-' is appended
		// unchanged ('&' is plain here too, but asciiPlain excludes it and
		// the slow path appends it the same way).
		i := t.pos
		for i < len(t.in) {
			b := t.in[i]
			if b == '<' || b == '-' || !asciiPlain[b] {
				break
			}
			i++
		}
		if i > t.pos {
			t.data = append(t.data, t.in[t.pos:i]...)
			t.pos = i
			if i > t.hwm {
				t.hwm = i
			}
		}
		c := t.next()
		switch c {
		case '<':
			t.data = append(t.data, '<')
			t.state = stateCommentLessThanSign
			return
		case '-':
			t.state = stateCommentEndDash
			return
		case 0:
			t.err(ErrUnexpectedNullCharacter)
			t.data = append(t.data, "�"...)
		case eofRune:
			t.err(ErrEOFInComment)
			t.emitComment()
			t.emitEOF()
			return
		default:
			t.data = appendRune(t.data, c)
		}
	}
}

// commentLessThanSignState is the comment less-than sign state
// (§13.2.5.46).
func (t *Tokenizer) commentLessThanSignState() {
	for {
		switch c := t.next(); c {
		case '!':
			t.data = append(t.data, '!')
			t.state = stateCommentLessThanSignBang
			return
		case '<':
			t.data = append(t.data, '<')
		default:
			t.reconsume()
			t.state = stateComment
			return
		}
	}
}

// commentLessThanSignBangState is the comment less-than sign bang state
// (§13.2.5.47).
func (t *Tokenizer) commentLessThanSignBangState() {
	if c := t.next(); c == '-' {
		t.state = stateCommentLessThanSignBangDash
		return
	}
	t.reconsume()
	t.state = stateComment
}

// commentLessThanSignBangDashState is the comment less-than sign bang
// dash state (§13.2.5.48).
func (t *Tokenizer) commentLessThanSignBangDashState() {
	if c := t.next(); c == '-' {
		t.state = stateCommentLessThanSignBangDashDash
		return
	}
	t.reconsume()
	t.state = stateCommentEndDash
}

// commentLessThanSignBangDashDashState is the comment less-than sign
// bang dash dash state (§13.2.5.49).
func (t *Tokenizer) commentLessThanSignBangDashDashState() {
	c := t.next()
	if c != '>' && c != eofRune {
		t.err(ErrNestedComment)
	}
	t.reconsume()
	t.state = stateCommentEnd
}

// commentEndDashState is the comment end dash state (§13.2.5.50).
func (t *Tokenizer) commentEndDashState() {
	switch c := t.next(); c {
	case '-':
		t.state = stateCommentEnd
	case eofRune:
		t.err(ErrEOFInComment)
		t.emitComment()
		t.emitEOF()
	default:
		t.data = append(t.data, '-')
		t.reconsume()
		t.state = stateComment
	}
}

// commentEndState is the comment end state (§13.2.5.51).
func (t *Tokenizer) commentEndState() {
	for {
		switch c := t.next(); c {
		case '>':
			t.state = stateData
			t.emitComment()
		case '!':
			t.state = stateCommentEndBang
		case '-':
			t.data = append(t.data, '-')
			continue
		case eofRune:
			t.err(ErrEOFInComment)
			t.emitComment()
			t.emitEOF()
		default:
			t.data = append(t.data, "--"...)
			t.reconsume()
			t.state = stateComment
		}
		return
	}
}

// commentEndBangState is the comment end bang state (§13.2.5.52).
func (t *Tokenizer) commentEndBangState() {
	switch c := t.next(); c {
	case '-':
		t.data = append(t.data, "--!"...)
		t.state = stateCommentEndDash
	case '>':
		t.err(ErrIncorrectlyClosedComment)
		t.state = stateData
		t.emitComment()
	case eofRune:
		t.err(ErrEOFInComment)
		t.emitComment()
		t.emitEOF()
	default:
		t.data = append(t.data, "--!"...)
		t.reconsume()
		t.state = stateComment
	}
}

// doctypeState is the DOCTYPE state (§13.2.5.53).
func (t *Tokenizer) doctypeState() {
	c := t.next()
	switch {
	case isHTMLSpace(c):
		t.state = stateBeforeDOCTYPEName
	case c == '>':
		t.reconsume()
		t.state = stateBeforeDOCTYPEName
	case c == eofRune:
		t.err(ErrEOFInDoctype)
		t.newDoctype()
		t.forceQuirks = true
		t.emitDoctype()
		t.emitEOF()
	default:
		t.err(ErrMissingWhitespaceBeforeDoctypeName)
		t.reconsume()
		t.state = stateBeforeDOCTYPEName
	}
}

// beforeDOCTYPENameState is the before DOCTYPE name state (§13.2.5.54).
func (t *Tokenizer) beforeDOCTYPENameState() {
	for {
		c := t.next()
		switch {
		case isHTMLSpace(c):
			continue
		case isASCIIUpper(c):
			t.newDoctype()
			t.hasName = true
			t.name = append(t.name, byte(c+0x20))
			t.state = stateDOCTYPEName
		case c == 0:
			t.err(ErrUnexpectedNullCharacter)
			t.newDoctype()
			t.hasName = true
			t.name = append(t.name, "�"...)
			t.state = stateDOCTYPEName
		case c == '>':
			t.err(ErrMissingDoctypeName)
			t.newDoctype()
			t.forceQuirks = true
			t.state = stateData
			t.emitDoctype()
		case c == eofRune:
			t.err(ErrEOFInDoctype)
			t.newDoctype()
			t.forceQuirks = true
			t.emitDoctype()
			t.emitEOF()
		default:
			t.newDoctype()
			t.hasName = true
			t.name = appendRune(t.name, c)
			t.state = stateDOCTYPEName
		}
		return
	}
}

// doctypeNameState is the DOCTYPE name state (§13.2.5.55).
func (t *Tokenizer) doctypeNameState() {
	for {
		c := t.next()
		switch {
		case isHTMLSpace(c):
			t.state = stateAfterDOCTYPEName
			return
		case c == '>':
			t.state = stateData
			t.emitDoctype()
			return
		case isASCIIUpper(c):
			t.name = append(t.name, byte(c+0x20))
		case c == 0:
			t.err(ErrUnexpectedNullCharacter)
			t.name = append(t.name, "�"...)
		case c == eofRune:
			t.err(ErrEOFInDoctype)
			t.forceQuirks = true
			t.emitDoctype()
			t.emitEOF()
			return
		default:
			t.name = appendRune(t.name, c)
		}
	}
}

// afterDOCTYPENameState is the after DOCTYPE name state (§13.2.5.56).
func (t *Tokenizer) afterDOCTYPENameState() {
	for {
		c := t.next()
		switch {
		case isHTMLSpace(c):
			continue
		case c == '>':
			t.state = stateData
			t.emitDoctype()
		case c == eofRune:
			t.err(ErrEOFInDoctype)
			t.forceQuirks = true
			t.emitDoctype()
			t.emitEOF()
		// "If the six characters starting from the current input
		// character are PUBLIC / SYSTEM" (ASCII case-insensitive): rewind
		// to the current character and consume all six.
		case t.peekFold(t.curStart, "public"):
			t.pos = t.curStart
			t.skipASCII(6)
			t.state = stateAfterDOCTYPEPublicKeyword
		case t.peekFold(t.curStart, "system"):
			t.pos = t.curStart
			t.skipASCII(6)
			t.state = stateAfterDOCTYPESystemKeyword
		default:
			t.err(ErrInvalidCharacterSequenceAfterDoctypeName)
			t.forceQuirks = true
			t.reconsume()
			t.state = stateBogusDOCTYPE
		}
		return
	}
}

// afterDOCTYPEKeywordState is the after DOCTYPE public keyword state
// (§13.2.5.57) when public is true and the after DOCTYPE system keyword
// state (§13.2.5.63) otherwise.
func (t *Tokenizer) afterDOCTYPEKeywordState(public bool) {
	c := t.next()
	switch {
	case isHTMLSpace(c):
		if public {
			t.state = stateBeforeDOCTYPEPublicIdentifier
		} else {
			t.state = stateBeforeDOCTYPESystemIdentifier
		}
	case c == '"' || c == '\'':
		if public {
			t.err(ErrMissingWhitespaceAfterDoctypePublicKeyword)
		} else {
			t.err(ErrMissingWhitespaceAfterDoctypeSystemKeyword)
		}
		t.openIdentifier(public, c)
	case c == '>':
		if public {
			t.err(ErrMissingDoctypePublicIdentifier)
		} else {
			t.err(ErrMissingDoctypeSystemIdentifier)
		}
		t.forceQuirks = true
		t.state = stateData
		t.emitDoctype()
	case c == eofRune:
		t.err(ErrEOFInDoctype)
		t.forceQuirks = true
		t.emitDoctype()
		t.emitEOF()
	default:
		if public {
			t.err(ErrMissingQuoteBeforeDoctypePublicIdentifier)
		} else {
			t.err(ErrMissingQuoteBeforeDoctypeSystemIdentifier)
		}
		t.forceQuirks = true
		t.reconsume()
		t.state = stateBogusDOCTYPE
	}
}

// openIdentifier sets the public (or system) identifier to the empty
// string and switches to its quoted state for quote.
func (t *Tokenizer) openIdentifier(public bool, quote rune) {
	if public {
		t.hasPub = true
		t.pubID = t.pubID[:0]
		if quote == '"' {
			t.state = stateDOCTYPEPublicIdentifierDoubleQuoted
		} else {
			t.state = stateDOCTYPEPublicIdentifierSingleQuoted
		}
		return
	}
	t.hasSys = true
	t.sysID = t.sysID[:0]
	if quote == '"' {
		t.state = stateDOCTYPESystemIdentifierDoubleQuoted
	} else {
		t.state = stateDOCTYPESystemIdentifierSingleQuoted
	}
}

// beforeDOCTYPEIdentifierState is the before DOCTYPE public identifier
// state (§13.2.5.58) when public is true and the before DOCTYPE system
// identifier state (§13.2.5.64) otherwise.
func (t *Tokenizer) beforeDOCTYPEIdentifierState(public bool) {
	for {
		c := t.next()
		switch {
		case isHTMLSpace(c):
			continue
		case c == '"' || c == '\'':
			t.openIdentifier(public, c)
		case c == '>':
			if public {
				t.err(ErrMissingDoctypePublicIdentifier)
			} else {
				t.err(ErrMissingDoctypeSystemIdentifier)
			}
			t.forceQuirks = true
			t.state = stateData
			t.emitDoctype()
		case c == eofRune:
			t.err(ErrEOFInDoctype)
			t.forceQuirks = true
			t.emitDoctype()
			t.emitEOF()
		default:
			if public {
				t.err(ErrMissingQuoteBeforeDoctypePublicIdentifier)
			} else {
				t.err(ErrMissingQuoteBeforeDoctypeSystemIdentifier)
			}
			t.forceQuirks = true
			t.reconsume()
			t.state = stateBogusDOCTYPE
		}
		return
	}
}

// doctypeIdentifierQuotedState is the DOCTYPE public identifier
// (double-quoted / single-quoted) state (§13.2.5.59-60) when public is
// true, and the DOCTYPE system identifier (double-quoted /
// single-quoted) state (§13.2.5.65-66) otherwise.
func (t *Tokenizer) doctypeIdentifierQuotedState(public bool, quote rune) {
	buf := &t.sysID
	if public {
		buf = &t.pubID
	}
	for {
		c := t.next()
		switch c {
		case quote:
			if public {
				t.state = stateAfterDOCTYPEPublicIdentifier
			} else {
				t.state = stateAfterDOCTYPESystemIdentifier
			}
			return
		case 0:
			t.err(ErrUnexpectedNullCharacter)
			*buf = append(*buf, "�"...)
		case '>':
			if public {
				t.err(ErrAbruptDoctypePublicIdentifier)
			} else {
				t.err(ErrAbruptDoctypeSystemIdentifier)
			}
			t.forceQuirks = true
			t.state = stateData
			t.emitDoctype()
			return
		case eofRune:
			t.err(ErrEOFInDoctype)
			t.forceQuirks = true
			t.emitDoctype()
			t.emitEOF()
			return
		default:
			*buf = appendRune(*buf, c)
		}
	}
}

// afterDOCTYPEPublicIdentifierState is the after DOCTYPE public
// identifier state (§13.2.5.61).
func (t *Tokenizer) afterDOCTYPEPublicIdentifierState() {
	c := t.next()
	switch {
	case isHTMLSpace(c):
		t.state = stateBetweenDOCTYPEPublicAndSystemIdentifiers
	case c == '>':
		t.state = stateData
		t.emitDoctype()
	case c == '"' || c == '\'':
		t.err(ErrMissingWhitespaceBetweenDoctypePublicAndSystemIdentifiers)
		t.openIdentifier(false, c)
	case c == eofRune:
		t.err(ErrEOFInDoctype)
		t.forceQuirks = true
		t.emitDoctype()
		t.emitEOF()
	default:
		t.err(ErrMissingQuoteBeforeDoctypeSystemIdentifier)
		t.forceQuirks = true
		t.reconsume()
		t.state = stateBogusDOCTYPE
	}
}

// betweenDOCTYPEPublicAndSystemIdentifiersState is the between DOCTYPE
// public and system identifiers state (§13.2.5.62).
func (t *Tokenizer) betweenDOCTYPEPublicAndSystemIdentifiersState() {
	for {
		c := t.next()
		switch {
		case isHTMLSpace(c):
			continue
		case c == '>':
			t.state = stateData
			t.emitDoctype()
		case c == '"' || c == '\'':
			t.openIdentifier(false, c)
		case c == eofRune:
			t.err(ErrEOFInDoctype)
			t.forceQuirks = true
			t.emitDoctype()
			t.emitEOF()
		default:
			t.err(ErrMissingQuoteBeforeDoctypeSystemIdentifier)
			t.forceQuirks = true
			t.reconsume()
			t.state = stateBogusDOCTYPE
		}
		return
	}
}

// afterDOCTYPESystemIdentifierState is the after DOCTYPE system
// identifier state (§13.2.5.67).
func (t *Tokenizer) afterDOCTYPESystemIdentifierState() {
	for {
		c := t.next()
		switch {
		case isHTMLSpace(c):
			continue
		case c == '>':
			t.state = stateData
			t.emitDoctype()
		case c == eofRune:
			t.err(ErrEOFInDoctype)
			t.forceQuirks = true
			t.emitDoctype()
			t.emitEOF()
		default:
			t.err(ErrUnexpectedCharacterAfterDoctypeSystemIdentifier)
			t.reconsume()
			t.state = stateBogusDOCTYPE
		}
		return
	}
}

// bogusDOCTYPEState is the bogus DOCTYPE state (§13.2.5.68).
func (t *Tokenizer) bogusDOCTYPEState() {
	for {
		switch c := t.next(); c {
		case '>':
			t.state = stateData
			t.emitDoctype()
			return
		case 0:
			t.err(ErrUnexpectedNullCharacter)
		case eofRune:
			t.emitDoctype()
			t.emitEOF()
			return
		}
	}
}

// cdataSectionState is the CDATA section state (§13.2.5.69). NULs are
// passed through (the state has no NUL rule); the foreign-content rules
// for character tokens replace them with U+FFFD.
func (t *Tokenizer) cdataSectionState() {
	for {
		i := t.pos
		for i < len(t.in) {
			b := t.in[i]
			if b == ']' || !(asciiPlain[b] || b == '<' || b == '&') {
				break
			}
			i++
		}
		if i > t.pos {
			t.text = append(t.text, t.in[t.pos:i]...)
			t.pos = i
			t.textEnd = i
			if i > t.hwm {
				t.hwm = i
			}
		}
		c := t.next()
		switch c {
		case ']':
			t.state = stateCDATASectionBracket
			return
		case eofRune:
			t.err(ErrEOFInCDATA)
			t.emitEOF()
			return
		default:
			t.emitRune(c)
		}
	}
}

// cdataSectionBracketState is the CDATA section bracket state
// (§13.2.5.70).
func (t *Tokenizer) cdataSectionBracketState() {
	if c := t.next(); c == ']' {
		t.state = stateCDATASectionEnd
		return
	}
	t.reconsume()
	t.emitRune(']')
	t.state = stateCDATASection
}

// cdataSectionEndState is the CDATA section end state (§13.2.5.71).
func (t *Tokenizer) cdataSectionEndState() {
	for {
		switch c := t.next(); c {
		case ']':
			t.emitRune(']')
			continue
		case '>':
			// The section ends; its text stays pending and coalesces with
			// any character data that follows.
			t.state = stateData
		default:
			t.reconsume()
			t.emitString("]]")
			t.state = stateCDATASection
		}
		return
	}
}

// commentFromTemp starts the bogus comment a rejected processing
// instruction becomes: its data is "?" followed by the temporary buffer
// (the target characters consumed so far), as the pre-2025 "<?" rule
// would have produced.
func (t *Tokenizer) commentFromTemp() {
	t.data = append(t.data[:0], '?')
	t.data = append(t.data, t.temp...)
}

// processingInstructionOpenState is the processing instruction open
// state (§13.2.5, 2025), entered after "<?": a target must start with an
// ASCII letter or '_'.
func (t *Tokenizer) processingInstructionOpenState() {
	c := t.next()
	switch {
	case isASCIIAlpha(c) || c == '_':
		t.reconsume()
		t.state = stateProcessingInstructionTarget
	case c == eofRune:
		t.err(ErrEOFInProcessingInstruction)
		t.emitEOF()
	default:
		t.err(ErrInvalidFirstCharacterOfProcessingInstructionTarget)
		t.commentFromTemp()
		t.reconsume()
		t.state = stateBogusComment
	}
}

// processingInstructionTargetState is the processing instruction target
// state (§13.2.5, 2025). The targets "xml" and "xml-stylesheet" (ASCII
// case-insensitive) and targets with other characters stay bogus
// comments, as before processing instructions existed.
func (t *Tokenizer) processingInstructionTargetState() {
	for {
		c := t.next()
		switch {
		case isHTMLSpace(c) || c == '?' || c == '>':
			if equalFoldASCII(string(t.temp), "xml") || equalFoldASCII(string(t.temp), "xml-stylesheet") {
				t.err(ErrDisallowedProcessingInstructionTarget)
				t.commentFromTemp()
				t.reconsume()
				t.state = stateBogusComment
				return
			}
			// The PI token's target is the temporary buffer; its data
			// accumulates in t.data.
			t.name = append(t.name[:0], t.temp...)
			t.data = t.data[:0]
			t.reconsume()
			t.state = stateAfterProcessingInstructionTarget
			return
		case isASCIIAlnum(c) || c == '-' || c == '_':
			t.temp = append(t.temp, byte(c))
		case c == eofRune:
			t.err(ErrEOFInProcessingInstruction)
			t.emitEOF()
			return
		default:
			t.err(ErrInvalidProcessingInstructionTarget)
			t.commentFromTemp()
			t.reconsume()
			t.state = stateBogusComment
			return
		}
	}
}

// afterProcessingInstructionTargetState is the after processing
// instruction target state (§13.2.5, 2025): whitespace between target and
// data is dropped.
func (t *Tokenizer) afterProcessingInstructionTargetState() {
	for {
		c := t.next()
		if isHTMLSpace(c) {
			continue
		}
		t.reconsume()
		t.state = stateProcessingInstructionData
		return
	}
}

// processingInstructionDataState is the processing instruction data
// state (§13.2.5, 2025). A PI ends at the first '>' ("?>" or a bare
// '>'); there are no character references and no NUL replacement here.
func (t *Tokenizer) processingInstructionDataState() {
	for {
		c := t.next()
		switch c {
		case '?':
			t.state = stateProcessingInstructionQuestionable
			return
		case '>':
			t.state = stateData
			t.emitPI()
			return
		case eofRune:
			t.err(ErrEOFInProcessingInstruction)
			t.emitEOF()
			return
		default:
			t.data = appendRune(t.data, c)
		}
	}
}

// processingInstructionQuestionableState is the processing instruction
// questionable state (§13.2.5, 2025), after a '?' in the data: '>' ends
// the PI without the '?', anything else keeps it.
func (t *Tokenizer) processingInstructionQuestionableState() {
	switch c := t.next(); c {
	case '>':
		t.state = stateData
		t.emitPI()
	case eofRune:
		t.err(ErrEOFInProcessingInstruction)
		t.emitEOF()
	default:
		t.data = append(t.data, '?')
		t.reconsume()
		t.state = stateProcessingInstructionData
	}
}

// emitPI emits the processing instruction token: target in Name, data in
// Data.
func (t *Tokenizer) emitPI() {
	t.emitToken(Token{Kind: ProcessingInstructionToken, Name: string(t.name), Data: string(t.data)})
}
