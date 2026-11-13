package htmlparse

// Text states: HTML §13.2.5.1-5 (data, RCDATA, RAWTEXT, script data,
// PLAINTEXT), the RCDATA/RAWTEXT/script-data less-than-sign and end-tag
// states (§13.2.5.9-17, 24-25) and the script data escape and
// double-escape states (§13.2.5.18-31).

// scanPlain copies a run of "plain" ASCII bytes (see asciiPlain) into the
// pending text. When amp is false '&' also counts as plain. It is a pure
// fast path: the bytes it copies would each have been "emit the current
// input character" in the calling state.
func (t *Tokenizer) scanPlain(amp bool) {
	i := t.pos
	in := t.in
	for i < len(in) {
		c := in[i]
		if !asciiPlain[c] && (amp || c != '&') {
			break
		}
		i++
	}
	if i > t.pos {
		t.text = append(t.text, in[t.pos:i]...)
		t.pos = i
		t.textEnd = i
		if i > t.hwm {
			t.hwm = i
		}
	}
}

// dataState is the data state (§13.2.5.1).
func (t *Tokenizer) dataState() {
	for {
		t.scanPlain(true)
		c := t.next()
		switch c {
		case '&':
			t.returnState = stateData
			t.state = stateCharacterReference
			return
		case '<':
			t.state = stateTagOpen
			return
		case 0:
			t.err(ErrUnexpectedNullCharacter)
			t.emitRune(0)
		case eofRune:
			t.emitEOF()
			return
		default:
			t.emitRune(c)
		}
	}
}

// rcdataState is the RCDATA state (§13.2.5.2).
func (t *Tokenizer) rcdataState() {
	for {
		t.scanPlain(true)
		c := t.next()
		switch c {
		case '&':
			t.returnState = stateRCDATA
			t.state = stateCharacterReference
			return
		case '<':
			t.state = stateRCDATALessThanSign
			return
		case 0:
			t.err(ErrUnexpectedNullCharacter)
			t.emitRune(0xFFFD)
		case eofRune:
			t.emitEOF()
			return
		default:
			t.emitRune(c)
		}
	}
}

// rawtextState is the RAWTEXT state (§13.2.5.3).
func (t *Tokenizer) rawtextState() {
	for {
		t.scanPlain(false)
		c := t.next()
		switch c {
		case '<':
			t.state = stateRAWTEXTLessThanSign
			return
		case 0:
			t.err(ErrUnexpectedNullCharacter)
			t.emitRune(0xFFFD)
		case eofRune:
			t.emitEOF()
			return
		default:
			t.emitRune(c)
		}
	}
}

// scriptDataState is the script data state (§13.2.5.4).
func (t *Tokenizer) scriptDataState() {
	for {
		t.scanPlain(false)
		c := t.next()
		switch c {
		case '<':
			t.state = stateScriptDataLessThanSign
			return
		case 0:
			t.err(ErrUnexpectedNullCharacter)
			t.emitRune(0xFFFD)
		case eofRune:
			t.emitEOF()
			return
		default:
			t.emitRune(c)
		}
	}
}

// plaintextState is the PLAINTEXT state (§13.2.5.5).
func (t *Tokenizer) plaintextState() {
	for {
		t.scanPlain(false)
		c := t.next()
		switch c {
		case '<':
			t.emitRune('<')
		case 0:
			t.err(ErrUnexpectedNullCharacter)
			t.emitRune(0xFFFD)
		case eofRune:
			t.emitEOF()
			return
		default:
			t.emitRune(c)
		}
	}
}

// textLessThanSignState is the RCDATA less-than sign state (§13.2.5.9)
// and the RAWTEXT less-than sign state (§13.2.5.12): endTagOpen is the
// matching end tag open state and text the matching text state.
func (t *Tokenizer) textLessThanSignState(endTagOpen, text state) {
	if c := t.next(); c == '/' {
		t.temp = t.temp[:0]
		t.state = endTagOpen
		return
	}
	t.reconsume()
	t.emitRune('<')
	t.state = text
}

// textEndTagOpenState is the RCDATA (§13.2.5.10), RAWTEXT (§13.2.5.13),
// script data (§13.2.5.16) and script data escaped (§13.2.5.24) end tag
// open state.
func (t *Tokenizer) textEndTagOpenState(endTagName, text state) {
	c := t.next()
	t.reconsume()
	if isASCIIAlpha(c) {
		t.newTag(EndTagToken)
		t.state = endTagName
		return
	}
	t.emitString("</")
	t.state = text
}

// textEndTagNameState is the RCDATA (§13.2.5.11), RAWTEXT (§13.2.5.14),
// script data (§13.2.5.17) and script data escaped (§13.2.5.25) end tag
// name state. text is the state to reconsume in when the tag is not an
// appropriate end tag.
func (t *Tokenizer) textEndTagNameState(text state) {
	for {
		c := t.next()
		switch {
		case isHTMLSpace(c):
			if t.appropriateEndTag() {
				t.state = stateBeforeAttributeName
				return
			}
		case c == '/':
			if t.appropriateEndTag() {
				t.state = stateSelfClosingStartTag
				return
			}
		case c == '>':
			if t.appropriateEndTag() {
				t.state = stateData
				t.emitTag()
				return
			}
		case isASCIIUpper(c):
			t.name = append(t.name, byte(c+0x20))
			t.temp = append(t.temp, byte(c))
			continue
		case c >= 'a' && c <= 'z':
			t.name = append(t.name, byte(c))
			t.temp = append(t.temp, byte(c))
			continue
		}
		// Anything else.
		t.reconsume()
		t.emitString("</")
		t.emitBytes(t.temp)
		t.state = text
		return
	}
}

// scriptDataLessThanSignState is the script data less-than sign state
// (§13.2.5.15).
func (t *Tokenizer) scriptDataLessThanSignState() {
	switch c := t.next(); c {
	case '/':
		t.temp = t.temp[:0]
		t.state = stateScriptDataEndTagOpen
	case '!':
		t.state = stateScriptDataEscapeStart
		t.emitString("<!")
	default:
		t.reconsume()
		t.emitRune('<')
		t.state = stateScriptData
	}
}

// scriptDataEscapeStartState is the script data escape start state
// (§13.2.5.18).
func (t *Tokenizer) scriptDataEscapeStartState() {
	if c := t.next(); c == '-' {
		t.state = stateScriptDataEscapeStartDash
		t.emitRune('-')
		return
	}
	t.reconsume()
	t.state = stateScriptData
}

// scriptDataEscapeStartDashState is the script data escape start dash
// state (§13.2.5.19).
func (t *Tokenizer) scriptDataEscapeStartDashState() {
	if c := t.next(); c == '-' {
		t.state = stateScriptDataEscapedDashDash
		t.emitRune('-')
		return
	}
	t.reconsume()
	t.state = stateScriptData
}

// scriptDataEscapedState is the script data escaped state (§13.2.5.20).
func (t *Tokenizer) scriptDataEscapedState() {
	for {
		c := t.next()
		switch c {
		case '-':
			t.state = stateScriptDataEscapedDash
			t.emitRune('-')
			return
		case '<':
			t.state = stateScriptDataEscapedLessThanSign
			return
		case 0:
			t.err(ErrUnexpectedNullCharacter)
			t.emitRune(0xFFFD)
		case eofRune:
			t.err(ErrEOFInScriptHTMLCommentLikeText)
			t.emitEOF()
			return
		default:
			t.emitRune(c)
		}
	}
}

// scriptDataEscapedDashState is the script data escaped dash state
// (§13.2.5.21).
func (t *Tokenizer) scriptDataEscapedDashState() {
	switch c := t.next(); c {
	case '-':
		t.state = stateScriptDataEscapedDashDash
		t.emitRune('-')
	case '<':
		t.state = stateScriptDataEscapedLessThanSign
	case 0:
		t.err(ErrUnexpectedNullCharacter)
		t.state = stateScriptDataEscaped
		t.emitRune(0xFFFD)
	case eofRune:
		t.err(ErrEOFInScriptHTMLCommentLikeText)
		t.emitEOF()
	default:
		t.state = stateScriptDataEscaped
		t.emitRune(c)
	}
}

// scriptDataEscapedDashDashState is the script data escaped dash dash
// state (§13.2.5.22).
func (t *Tokenizer) scriptDataEscapedDashDashState() {
	switch c := t.next(); c {
	case '-':
		t.emitRune('-')
	case '<':
		t.state = stateScriptDataEscapedLessThanSign
	case '>':
		t.state = stateScriptData
		t.emitRune('>')
	case 0:
		t.err(ErrUnexpectedNullCharacter)
		t.state = stateScriptDataEscaped
		t.emitRune(0xFFFD)
	case eofRune:
		t.err(ErrEOFInScriptHTMLCommentLikeText)
		t.emitEOF()
	default:
		t.state = stateScriptDataEscaped
		t.emitRune(c)
	}
}

// scriptDataEscapedLessThanSignState is the script data escaped
// less-than sign state (§13.2.5.23).
func (t *Tokenizer) scriptDataEscapedLessThanSignState() {
	c := t.next()
	switch {
	case c == '/':
		t.temp = t.temp[:0]
		t.state = stateScriptDataEscapedEndTagOpen
	case isASCIIAlpha(c):
		t.temp = t.temp[:0]
		t.reconsume()
		t.emitRune('<')
		t.state = stateScriptDataDoubleEscapeStart
	default:
		t.reconsume()
		t.emitRune('<')
		t.state = stateScriptDataEscaped
	}
}

// scriptDataDoubleEscapeStartState is the script data double escape
// start state (§13.2.5.26).
func (t *Tokenizer) scriptDataDoubleEscapeStartState() {
	t.doubleEscapeBoundary(stateScriptDataDoubleEscaped, stateScriptDataEscaped)
}

// scriptDataDoubleEscapeEndState is the script data double escape end
// state (§13.2.5.31).
func (t *Tokenizer) scriptDataDoubleEscapeEndState() {
	t.doubleEscapeBoundary(stateScriptDataEscaped, stateScriptDataDoubleEscaped)
}

// doubleEscapeBoundary implements the shared body of the double escape
// start/end states: on a delimiter it switches to ifScript when the
// temporary buffer is "script" and to otherwise if not; letters are
// buffered (lower-cased) and emitted; anything else reconsumes in
// otherwise.
func (t *Tokenizer) doubleEscapeBoundary(ifScript, otherwise state) {
	for {
		c := t.next()
		switch {
		case isHTMLSpace(c) || c == '/' || c == '>':
			if string(t.temp) == "script" {
				t.state = ifScript
			} else {
				t.state = otherwise
			}
			t.emitRune(c)
			return
		case isASCIIUpper(c):
			t.temp = append(t.temp, byte(c+0x20))
			t.emitRune(c)
		case c >= 'a' && c <= 'z':
			t.temp = append(t.temp, byte(c))
			t.emitRune(c)
		default:
			t.reconsume()
			t.state = otherwise
			return
		}
	}
}

// scriptDataDoubleEscapedState is the script data double escaped state
// (§13.2.5.27).
func (t *Tokenizer) scriptDataDoubleEscapedState() {
	for {
		c := t.next()
		switch c {
		case '-':
			t.state = stateScriptDataDoubleEscapedDash
			t.emitRune('-')
			return
		case '<':
			t.state = stateScriptDataDoubleEscapedLessThanSign
			t.emitRune('<')
			return
		case 0:
			t.err(ErrUnexpectedNullCharacter)
			t.emitRune(0xFFFD)
		case eofRune:
			t.err(ErrEOFInScriptHTMLCommentLikeText)
			t.emitEOF()
			return
		default:
			t.emitRune(c)
		}
	}
}

// scriptDataDoubleEscapedDashState is the script data double escaped
// dash state (§13.2.5.28).
func (t *Tokenizer) scriptDataDoubleEscapedDashState() {
	switch c := t.next(); c {
	case '-':
		t.state = stateScriptDataDoubleEscapedDashDash
		t.emitRune('-')
	case '<':
		t.state = stateScriptDataDoubleEscapedLessThanSign
		t.emitRune('<')
	case 0:
		t.err(ErrUnexpectedNullCharacter)
		t.state = stateScriptDataDoubleEscaped
		t.emitRune(0xFFFD)
	case eofRune:
		t.err(ErrEOFInScriptHTMLCommentLikeText)
		t.emitEOF()
	default:
		t.state = stateScriptDataDoubleEscaped
		t.emitRune(c)
	}
}

// scriptDataDoubleEscapedDashDashState is the script data double
// escaped dash dash state (§13.2.5.29).
func (t *Tokenizer) scriptDataDoubleEscapedDashDashState() {
	switch c := t.next(); c {
	case '-':
		t.emitRune('-')
	case '<':
		t.state = stateScriptDataDoubleEscapedLessThanSign
		t.emitRune('<')
	case '>':
		t.state = stateScriptData
		t.emitRune('>')
	case 0:
		t.err(ErrUnexpectedNullCharacter)
		t.state = stateScriptDataDoubleEscaped
		t.emitRune(0xFFFD)
	case eofRune:
		t.err(ErrEOFInScriptHTMLCommentLikeText)
		t.emitEOF()
	default:
		t.state = stateScriptDataDoubleEscaped
		t.emitRune(c)
	}
}

// scriptDataDoubleEscapedLessThanSignState is the script data double
// escaped less-than sign state (§13.2.5.30).
func (t *Tokenizer) scriptDataDoubleEscapedLessThanSignState() {
	if c := t.next(); c == '/' {
		t.temp = t.temp[:0]
		t.state = stateScriptDataDoubleEscapeEnd
		t.emitRune('/')
		return
	}
	t.reconsume()
	t.state = stateScriptDataDoubleEscaped
}
