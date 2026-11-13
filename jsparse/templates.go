package jsparse

// scanTemplate recognises a template part (12.9.6). With cont false it
// starts at a backquote and yields NoSubstitutionTemplate or TemplateHead;
// with cont true it starts at the `}` that closes a substitution and
// yields TemplateMiddle or TemplateTail.
//
// Value is the template value (TV, "cooked"); when the part contains a
// NotEscapeSequence the cooked value is undefined, which is signalled by
// FlagInvalidEscape (Value is then empty). Raw is the template raw value
// (TRV) with CR and CRLF normalized to LF.
func (l *Lexer) scanTemplate(start int, cont bool) Token {
	src := l.src
	l.pos = start + 1
	bodyStart := l.pos
	t := Token{Start: int32(start)}
	var cooked, raw []byte
	slow := false // cooked/raw differ from the source slice
	var bodyEnd int
	terminated := false
	head := false
	for l.pos < len(src) {
		c := src[l.pos]
		if c == '`' {
			bodyEnd = l.pos
			l.pos++
			terminated = true
			break
		}
		if c == '$' && l.peekByte(1) == '{' {
			bodyEnd = l.pos
			l.pos += 2
			terminated = true
			head = true
			break
		}
		if c == '\\' {
			if !slow {
				cooked = append(cooked[:0], src[bodyStart:l.pos]...)
				raw = append(raw[:0], src[bodyStart:l.pos]...)
				slow = true
			}
			escStart := l.pos
			l.pos++
			var res escapeResult
			cooked, res = l.scanEscape(cooked, true)
			if res == escInvalid {
				// Not a lexical error: a tagged template may contain a
				// NotEscapeSequence (its cooked value is undefined). ErrAt
				// keeps the first one for the parser's untagged-template
				// error; 0 can mean "unset" because an escape never starts
				// at offset 0 (the backquote or brace comes first).
				t.Flags |= FlagInvalidEscape
				if t.ErrAt == 0 {
					t.ErrAt = int32(escStart)
				}
			}
			raw = appendRawTemplate(raw, src[escStart:l.pos])
			continue
		}
		if c == '\r' {
			if !slow {
				cooked = append(cooked[:0], src[bodyStart:l.pos]...)
				raw = append(raw[:0], src[bodyStart:l.pos]...)
				slow = true
			}
			l.pos++
			if l.pos < len(src) && src[l.pos] == '\n' {
				l.pos++
			}
			cooked = append(cooked, '\n')
			raw = append(raw, '\n')
			continue
		}
		if slow {
			cooked = append(cooked, c)
			raw = append(raw, c)
		}
		l.pos++
	}
	t.End = int32(l.pos)
	if !terminated {
		// An unterminated part is a tail (head is false); its body runs
		// to the end of the input.
		t.Err, t.ErrAt = CodeUnterminatedTemplate, int32(start)
		bodyEnd = l.pos
	}
	switch {
	case cont && head:
		t.Kind = TTemplateMiddle
	case cont:
		t.Kind = TTemplateTail
	case head:
		t.Kind = TTemplateHead
	default:
		t.Kind = TNoSubstitutionTemplate
	}
	if !slow {
		t.Value = src[bodyStart:bodyEnd]
		t.Raw = t.Value
	} else {
		t.Raw = string(raw)
		t.Value = string(cooked)
	}
	if t.Flags&FlagInvalidEscape != 0 {
		t.Value = ""
	}
	return t
}

// appendRawTemplate appends the raw text of an escape, normalizing CR and
// CRLF in a LineContinuation to LF (TRV).
func appendRawTemplate(raw []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\r' {
			raw = append(raw, '\n')
			if i+1 < len(s) && s[i+1] == '\n' {
				i++
			}
			continue
		}
		raw = append(raw, c)
	}
	return raw
}
