package jsparse

// escapeResult is the outcome of decoding one escape sequence.
type escapeResult uint8

const (
	escOK           escapeResult = iota
	escContinuation              // LineContinuation: contributes nothing
	escLegacy                    // LegacyOctalEscapeSequence or NonOctalDecimalEscapeSequence (12.9.4; a strict-mode error, 12.9.4.1)
	escInvalid                   // malformed \x / \u (or NotEscapeSequence in a template)
)

// scanEscape decodes the EscapeSequence after a backslash at l.pos-1
// (12.9.4 / 12.9.6). l.pos is at the character after the backslash. For
// templates (tmpl), legacy octal and \8 \9 are NotEscapeSequences, and on
// failure l.pos stays right after the escape letter so that scanning
// resumes with ordinary TemplateCharacters.
func (l *Lexer) scanEscape(buf []byte, tmpl bool) ([]byte, escapeResult) {
	src := l.src
	if l.pos >= len(src) {
		return buf, escInvalid
	}
	c := src[l.pos]
	switch c {
	case '\n':
		l.pos++
		return buf, escContinuation
	case '\r':
		l.pos++
		if l.pos < len(src) && src[l.pos] == '\n' {
			l.pos++
		}
		return buf, escContinuation
	case 'b':
		l.pos++
		return append(buf, '\b'), escOK
	case 't':
		l.pos++
		return append(buf, '\t'), escOK
	case 'n':
		l.pos++
		return append(buf, '\n'), escOK
	case 'v':
		l.pos++
		return append(buf, '\v'), escOK
	case 'f':
		l.pos++
		return append(buf, '\f'), escOK
	case 'r':
		l.pos++
		return append(buf, '\r'), escOK
	case 'x':
		l.pos++
		if l.pos+2 <= len(src) && isHexDigit(src[l.pos]) && isHexDigit(src[l.pos+1]) {
			v := hexVal(src[l.pos])*16 + hexVal(src[l.pos+1])
			l.pos += 2
			return appendWTF8(buf, rune(v)), escOK
		}
		return buf, escInvalid
	case 'u':
		l.pos++
		save := l.pos
		r, ok := l.scanUnicodeEscapeBody()
		if !ok {
			if tmpl {
				l.pos = save
			}
			return buf, escInvalid
		}
		// Combine an escaped surrogate pair into one code point. The SV of
		// a string is a sequence of UTF-16 code units, so "\uD83D\uDE00",
		// "\uD83D\u{DE00}" and "\u{D83D}\u{DE00}" are the same string and
		// must get the same Value (one four-byte UTF-8 sequence), or the
		// canonical JSON of equal programs would differ. Only a lone
		// surrogate stays a three-byte WTF-8 sequence.
		if r >= 0xD800 && r <= 0xDBFF && l.pos+6 <= len(src) && src[l.pos] == '\\' && src[l.pos+1] == 'u' {
			save2 := l.pos
			l.pos += 2
			if lo, ok := l.scanUnicodeEscapeBody(); ok && lo >= 0xDC00 && lo <= 0xDFFF {
				r = 0x10000 + (r-0xD800)<<10 + (lo - 0xDC00)
			} else {
				l.pos = save2
			}
		}
		return appendWTF8(buf, r), escOK
	case '0', '1', '2', '3', '4', '5', '6', '7':
		next := l.peekByte(1)
		if c == '0' && !isDecimalDigit(next) {
			l.pos++
			return append(buf, 0), escOK
		}
		if tmpl {
			l.pos++
			return buf, escInvalid
		}
		// LegacyOctalEscapeSequence: up to three digits for 0-3, two for
		// 4-7 (so the value stays below 256). "\08" lands here too: 0
		// followed by a decimal digit is legacy, not the NUL escape.
		v := int(c - '0')
		l.pos++
		max := 2
		if c >= '4' {
			max = 1
		}
		for k := 0; k < max && l.pos < len(src) && src[l.pos] >= '0' && src[l.pos] <= '7'; k++ {
			v = v*8 + int(src[l.pos]-'0')
			l.pos++
		}
		return appendWTF8(buf, rune(v)), escLegacy
	case '8', '9':
		l.pos++
		if tmpl {
			return buf, escInvalid
		}
		return append(buf, c), escLegacy
	}
	// LS and PS form a LineContinuation; anything else is a
	// NonEscapeCharacter standing for itself (bytes copied as they are, so
	// invalid UTF-8 survives unchanged).
	r, n := decodeRune(src, l.pos)
	l.pos += n
	if r == cpLS || r == cpPS {
		return buf, escContinuation
	}
	return append(buf, src[l.pos-n:l.pos]...), escOK
}

// scanStringLiteral recognises StringLiteral at start (12.9.4). LS and PS
// may appear unescaped (ES2019); CR and LF may not. Escape problems are
// recorded on the token (the first one wins) and scanning goes on to the
// closing quote, so only an unterminated string loses its end.
func (l *Lexer) scanStringLiteral(start int) Token {
	src := l.src
	quote := src[start]
	l.pos = start + 1
	t := Token{Kind: TString, Start: int32(start)}
	// Fast path: no escapes.
	i := l.pos
	for i < len(src) {
		c := src[i]
		if c == quote || c == '\\' || c == '\n' || c == '\r' {
			break
		}
		i++
	}
	if i < len(src) && src[i] == quote {
		l.pos = i + 1
		t.End = int32(l.pos)
		t.Value = src[start+1 : i]
		return t
	}
	buf := append(l.buf[:0], src[start+1:i]...)
	l.pos = i
	for {
		if l.pos >= len(src) {
			t.Err, t.ErrAt = CodeUnterminatedString, int32(start)
			break
		}
		c := src[l.pos]
		if c == quote {
			l.pos++
			break
		}
		if c == '\n' || c == '\r' {
			t.Err, t.ErrAt = CodeUnterminatedString, int32(start)
			break
		}
		if c == '\\' {
			escAt := l.pos
			l.pos++
			var res escapeResult
			buf, res = l.scanEscape(buf, false)
			switch res {
			case escLegacy:
				t.Flags |= FlagLegacyOctal
			case escInvalid:
				if t.Err == CodeNone {
					t.Err, t.ErrAt = CodeInvalidEscape, int32(escAt)
				}
			}
			continue
		}
		buf = append(buf, c)
		l.pos++
	}
	l.buf = buf
	t.End = int32(l.pos)
	t.Value = string(buf)
	return t
}
