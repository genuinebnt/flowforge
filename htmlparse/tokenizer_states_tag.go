package htmlparse

import "unicode/utf8"

// Tag states: HTML §13.2.5.6-8 (tag open, end tag open, tag name) and
// §13.2.5.32-40 (attribute states and the self-closing start tag state).

// tagOpenState is the tag open state (§13.2.5.6).
func (t *Tokenizer) tagOpenState() {
	c := t.next()
	switch {
	case c == '!':
		t.state = stateMarkupDeclarationOpen
	case c == '/':
		t.state = stateEndTagOpen
	case isASCIIAlpha(c):
		t.newTag(StartTagToken)
		t.reconsume()
		t.state = stateTagName
	case c == '?':
		t.temp = t.temp[:0]
		t.state = stateProcessingInstructionOpen
	case c == eofRune:
		t.err(ErrEOFBeforeTagName)
		t.emitRune('<')
		t.emitEOF()
	default:
		t.err(ErrInvalidFirstCharacterOfTagName)
		t.reconsume()
		t.emitRune('<')
		t.state = stateData
	}
}

// endTagOpenState is the end tag open state (§13.2.5.7).
func (t *Tokenizer) endTagOpenState() {
	c := t.next()
	switch {
	case isASCIIAlpha(c):
		t.newTag(EndTagToken)
		t.reconsume()
		t.state = stateTagName
	case c == '>':
		t.err(ErrMissingEndTagName)
		t.state = stateData
	case c == eofRune:
		t.err(ErrEOFBeforeTagName)
		t.emitString("</")
		t.emitEOF()
	default:
		t.err(ErrInvalidFirstCharacterOfTagName)
		t.data = t.data[:0]
		t.reconsume()
		t.state = stateBogusComment
	}
}

// tagNameState is the tag name state (§13.2.5.8).
func (t *Tokenizer) tagNameState() {
	for {
		// Fast path: bytes that "anything else" appends unchanged and that
		// raise no input-stream error (upper case is lowered below).
		i := t.pos
		for i < len(t.in) {
			b := t.in[i]
			if (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '-' || b == ':' || b == '_' || b == '.' {
				i++
				continue
			}
			break
		}
		if i > t.pos {
			t.name = append(t.name, t.in[t.pos:i]...)
			t.pos = i
			if i > t.hwm {
				t.hwm = i
			}
		}
		c := t.next()
		switch {
		case isHTMLSpace(c):
			t.state = stateBeforeAttributeName
			return
		case c == '/':
			t.state = stateSelfClosingStartTag
			return
		case c == '>':
			t.state = stateData
			t.emitTag()
			return
		case isASCIIUpper(c):
			t.name = append(t.name, byte(c+0x20))
		case c == 0:
			t.err(ErrUnexpectedNullCharacter)
			t.name = append(t.name, "�"...)
		case c == eofRune:
			t.err(ErrEOFInTag)
			t.emitEOF()
			return
		default:
			t.name = appendRune(t.name, c)
		}
	}
}

// beforeAttributeNameState is the before attribute name state
// (§13.2.5.32).
func (t *Tokenizer) beforeAttributeNameState() {
	for {
		c := t.next()
		switch {
		case isHTMLSpace(c):
			continue
		case c == '/' || c == '>' || c == eofRune:
			t.reconsume()
			t.state = stateAfterAttributeName
		case c == '=':
			t.err(ErrUnexpectedEqualsSignBeforeAttributeName)
			t.startAttribute()
			t.attrName = append(t.attrName, '=')
			t.state = stateAttributeName
		default:
			t.startAttribute()
			t.reconsume()
			t.state = stateAttributeName
		}
		return
	}
}

// attributeNameState is the attribute name state (§13.2.5.33). Leaving
// it runs the duplicate-attribute check (leaveAttributeName).
func (t *Tokenizer) attributeNameState() {
	for {
		c := t.next()
		switch {
		case isHTMLSpace(c) || c == '/' || c == '>' || c == eofRune:
			t.reconsume()
			t.leaveAttributeName()
			t.state = stateAfterAttributeName
			return
		case c == '=':
			t.leaveAttributeName()
			t.state = stateBeforeAttributeValue
			return
		case isASCIIUpper(c):
			t.attrName = append(t.attrName, byte(c+0x20))
		case c == 0:
			t.err(ErrUnexpectedNullCharacter)
			t.attrName = append(t.attrName, "�"...)
		case c == '"' || c == '\'' || c == '<':
			t.err(ErrUnexpectedCharacterInAttributeName)
			t.attrName = append(t.attrName, byte(c))
		default:
			t.attrName = appendRune(t.attrName, c)
		}
	}
}

// afterAttributeNameState is the after attribute name state
// (§13.2.5.34).
func (t *Tokenizer) afterAttributeNameState() {
	for {
		c := t.next()
		switch {
		case isHTMLSpace(c):
			continue
		case c == '/':
			t.state = stateSelfClosingStartTag
		case c == '=':
			t.state = stateBeforeAttributeValue
		case c == '>':
			t.state = stateData
			t.emitTag()
		case c == eofRune:
			t.err(ErrEOFInTag)
			t.emitEOF()
		default:
			t.startAttribute()
			t.reconsume()
			t.state = stateAttributeName
		}
		return
	}
}

// beforeAttributeValueState is the before attribute value state
// (§13.2.5.35).
func (t *Tokenizer) beforeAttributeValueState() {
	for {
		c := t.next()
		switch {
		case isHTMLSpace(c):
			continue
		case c == '"':
			t.state = stateAttributeValueDoubleQuoted
		case c == '\'':
			t.state = stateAttributeValueSingleQuoted
		case c == '>':
			t.err(ErrMissingAttributeValue)
			t.state = stateData
			t.emitTag()
		default:
			t.reconsume()
			t.state = stateAttributeValueUnquoted
		}
		return
	}
}

// attributeValueQuotedState is the attribute value (double-quoted)
// state (§13.2.5.36) and the attribute value (single-quoted) state
// (§13.2.5.37); quote is the delimiter and self the state's own id (the
// character reference return state).
func (t *Tokenizer) attributeValueQuotedState(quote byte, self state) {
	for {
		// Fast path: copy bytes that are appended unchanged. It stops at
		// the quote, '&', CR (newline normalization), NUL and the other
		// controls (errors), and any non-ASCII byte (decoded by next for
		// its input-stream checks).
		i := t.pos
		for i < len(t.in) {
			b := t.in[i]
			if b == quote || b == '&' || b == '\r' || b >= 0x7F || (b < 0x20 && b != '\t' && b != '\n' && b != '\f') {
				break
			}
			i++
		}
		if i > t.pos {
			t.attrValue = append(t.attrValue, t.in[t.pos:i]...)
			t.pos = i
			if i > t.hwm {
				t.hwm = i
			}
		}
		c := t.next()
		switch {
		case c == rune(quote):
			t.state = stateAfterAttributeValueQuoted
			return
		case c == '&':
			t.returnState = self
			t.state = stateCharacterReference
			return
		case c == 0:
			t.err(ErrUnexpectedNullCharacter)
			t.attrValue = append(t.attrValue, "�"...)
		case c == eofRune:
			t.err(ErrEOFInTag)
			t.emitEOF()
			return
		default:
			t.attrValue = appendRune(t.attrValue, c)
		}
	}
}

// attributeValueUnquotedState is the attribute value (unquoted) state
// (§13.2.5.38).
func (t *Tokenizer) attributeValueUnquotedState() {
	for {
		c := t.next()
		switch {
		case isHTMLSpace(c):
			t.state = stateBeforeAttributeName
			return
		case c == '&':
			t.returnState = stateAttributeValueUnquoted
			t.state = stateCharacterReference
			return
		case c == '>':
			t.state = stateData
			t.emitTag()
			return
		case c == 0:
			t.err(ErrUnexpectedNullCharacter)
			t.attrValue = append(t.attrValue, "�"...)
		case c == '"' || c == '\'' || c == '<' || c == '=' || c == '`':
			t.err(ErrUnexpectedCharacterInUnquotedAttributeValue)
			t.attrValue = append(t.attrValue, byte(c))
		case c == eofRune:
			t.err(ErrEOFInTag)
			t.emitEOF()
			return
		default:
			t.attrValue = appendRune(t.attrValue, c)
		}
	}
}

// afterAttributeValueQuotedState is the after attribute value (quoted)
// state (§13.2.5.39).
func (t *Tokenizer) afterAttributeValueQuotedState() {
	c := t.next()
	switch {
	case isHTMLSpace(c):
		t.state = stateBeforeAttributeName
	case c == '/':
		t.state = stateSelfClosingStartTag
	case c == '>':
		t.state = stateData
		t.emitTag()
	case c == eofRune:
		t.err(ErrEOFInTag)
		t.emitEOF()
	default:
		t.err(ErrMissingWhitespaceBetweenAttributes)
		t.reconsume()
		t.state = stateBeforeAttributeName
	}
}

// selfClosingStartTagState is the self-closing start tag state
// (§13.2.5.40).
func (t *Tokenizer) selfClosingStartTagState() {
	c := t.next()
	switch c {
	case '>':
		t.selfClosing = true
		t.state = stateData
		t.emitTag()
	case eofRune:
		t.err(ErrEOFInTag)
		t.emitEOF()
	default:
		t.err(ErrUnexpectedSolidusInTag)
		t.reconsume()
		t.state = stateBeforeAttributeName
	}
}

// appendRune appends the UTF-8 encoding of c.
func appendRune(b []byte, c rune) []byte {
	if c < utf8.RuneSelf {
		return append(b, byte(c))
	}
	return utf8.AppendRune(b, c)
}
