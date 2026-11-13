package jsparse

// lexState is the state of the input-element machine. Each state names
// the ECMA-262 production it recognises.
type lexState uint8

const (
	// stateInputElement dispatches on the first code point of the next
	// input element (12, InputElementDiv / RegExp / TemplateTail goals).
	stateInputElement lexState = iota
	// stateHashbang recognises HashbangComment (12.5) at offset 0.
	stateHashbang
	// stateWhiteSpace consumes a run of WhiteSpace (12.2).
	stateWhiteSpace
	// stateLineTerminator consumes one LineTerminatorSequence (12.3).
	stateLineTerminator
	// stateSingleLineComment consumes SingleLineComment (12.4).
	stateSingleLineComment
	// stateMultiLineComment consumes MultiLineComment (12.4).
	stateMultiLineComment
	// stateHTMLOpenComment consumes SingleLineHTMLOpenComment (B.1.1).
	stateHTMLOpenComment
	// stateHTMLCloseComment consumes SingleLineHTMLCloseComment (B.1.1).
	stateHTMLCloseComment
	// stateIdentifierName recognises IdentifierName (12.7).
	stateIdentifierName
	// statePrivateIdentifier recognises PrivateIdentifier (12.7).
	statePrivateIdentifier
	// statePunctuator recognises Punctuator / DivPunctuator /
	// RightBracePunctuator by longest match (12.8).
	statePunctuator
	// stateNumericLiteral recognises NumericLiteral (12.9.3, numbers.go).
	stateNumericLiteral
	// stateStringLiteral recognises StringLiteral (12.9.4, strings.go).
	stateStringLiteral
	// stateTemplate recognises NoSubstitutionTemplate / TemplateHead
	// (12.9.6, templates.go).
	stateTemplate
	// stateTemplateSubstitutionTail recognises TemplateMiddle /
	// TemplateTail (12.9.6), only under the TemplateTail goals.
	stateTemplateSubstitutionTail
	// stateRegularExpression recognises RegularExpressionLiteral (12.9.5,
	// regexp.go), only under the RegExp goals.
	stateRegularExpression
	// stateInvalid produces an error token for one code point.
	stateInvalid
)

// Next returns the next token under goal. At the end of input it returns
// TEOF (repeatedly). It never fails: malformed input yields a token with
// Err set (TInvalid for a code point that starts nothing).
func (l *Lexer) Next(goal Goal) Token {
	st := stateInputElement
	if l.pos == 0 && len(l.src) >= 2 && l.src[0] == '#' && l.src[1] == '!' {
		st = stateHashbang
	}
	start := l.pos
	for {
		switch st {
		case stateInputElement:
			start = l.pos
			if l.pos >= len(l.src) {
				return l.finish(Token{Kind: TEOF, Start: int32(l.pos), End: int32(l.pos)}, goal)
			}
			st = l.dispatch(goal)

		case stateHashbang:
			l.pos = 2
			l.skipToLineEnd()
			l.addTrivia(THashbang, 0)
			st = stateInputElement

		case stateWhiteSpace:
			for l.pos < len(l.src) {
				c := l.src[l.pos]
				if c == ' ' || c == '\t' || c == '\v' || c == '\f' {
					l.pos++
					continue
				}
				if c >= 0x80 {
					r, n := decodeRune(l.src, l.pos)
					if isWhiteSpace(r) {
						l.pos += n
						continue
					}
				}
				break
			}
			l.addTrivia(TWhiteSpace, start)
			st = stateInputElement

		case stateLineTerminator:
			if l.src[l.pos] == '\r' && l.peekByte(1) == '\n' {
				l.pos += 2
			} else if l.src[l.pos] == '\n' || l.src[l.pos] == '\r' {
				l.pos++
			} else {
				l.pos += 3 // LS or PS
			}
			l.newline = true
			l.addTrivia(TLineTerminator, start)
			st = stateInputElement

		case stateSingleLineComment:
			l.pos += 2
			l.skipToLineEnd()
			l.addTrivia(TLineComment, start)
			st = stateInputElement

		case stateMultiLineComment:
			l.pos += 2
			closed := false
			for l.pos < len(l.src) {
				c := l.src[l.pos]
				if c == '*' && l.peekByte(1) == '/' {
					l.pos += 2
					closed = true
					break
				}
				if c == '\n' || c == '\r' {
					l.newline = true
				} else if c == 0xE2 {
					if r, _ := decodeRune(l.src, l.pos); r == cpLS || r == cpPS {
						l.newline = true
					}
				}
				l.pos++
			}
			if !closed {
				// The error token covers the comment; it is not trivia too.
				l.comments = append(l.comments, Comment{Kind: TBlockComment, Span: Span{int32(start), int32(l.pos)}})
				return l.finish(Token{Kind: TInvalid, Start: int32(start), End: int32(l.pos), Err: CodeUnterminatedComment, ErrAt: int32(start)}, goal)
			}
			l.addTrivia(TBlockComment, start)
			st = stateInputElement

		case stateHTMLOpenComment:
			l.pos += 4
			l.skipToLineEnd()
			l.addTrivia(THTMLOpenComment, start)
			st = stateInputElement

		case stateHTMLCloseComment:
			l.pos += 3
			l.skipToLineEnd()
			l.addTrivia(THTMLCloseComment, start)
			st = stateInputElement

		case stateIdentifierName:
			return l.finish(l.scanIdentifierName(start), goal)

		case statePrivateIdentifier:
			l.pos++
			if l.pos < len(l.src) {
				r, _ := decodeRune(l.src, l.pos)
				if isIDStart(r) || l.src[l.pos] == '\\' {
					t := l.scanIdentifierName(l.pos)
					t.Kind = TPrivateName
					t.Kw = KwNone
					t.Start = int32(start)
					return l.finish(t, goal)
				}
			}
			return l.finish(Token{Kind: TInvalid, Start: int32(start), End: int32(l.pos), Err: CodeUnexpectedCharacter, ErrAt: int32(start)}, goal)

		case statePunctuator:
			return l.finish(l.scanPunctuator(start), goal)

		case stateNumericLiteral:
			return l.finish(l.scanNumericLiteral(start), goal)

		case stateStringLiteral:
			return l.finish(l.scanStringLiteral(start), goal)

		case stateTemplate:
			return l.finish(l.scanTemplate(start, false), goal)

		case stateTemplateSubstitutionTail:
			return l.finish(l.scanTemplate(start, true), goal)

		case stateRegularExpression:
			return l.finish(l.scanRegularExpression(start), goal)

		case stateInvalid:
			_, n := decodeRune(l.src, l.pos)
			l.pos += n
			return l.finish(Token{Kind: TInvalid, Start: int32(start), End: int32(l.pos), Err: CodeUnexpectedCharacter, ErrAt: int32(start)}, goal)
		}
	}
}

// dispatch chooses the state for the input element starting at l.pos.
func (l *Lexer) dispatch(goal Goal) lexState {
	c := l.src[l.pos]
	switch c {
	case ' ', '\t', '\v', '\f':
		return stateWhiteSpace
	case '\n', '\r':
		return stateLineTerminator
	case '/':
		switch l.peekByte(1) {
		case '/':
			return stateSingleLineComment
		case '*':
			return stateMultiLineComment
		}
		if goal.allowsRegExp() {
			return stateRegularExpression
		}
		return statePunctuator
	case '<':
		if !l.module && l.peekByte(1) == '!' && l.peekByte(2) == '-' && l.peekByte(3) == '-' {
			return stateHTMLOpenComment
		}
		return statePunctuator
	case '-':
		if !l.module && (l.newline || !l.sawToken) && l.peekByte(1) == '-' && l.peekByte(2) == '>' {
			return stateHTMLCloseComment
		}
		return statePunctuator
	case '}':
		if goal.allowsTemplateTail() {
			return stateTemplateSubstitutionTail
		}
		return statePunctuator
	case '`':
		return stateTemplate
	case '"', '\'':
		return stateStringLiteral
	case '.':
		if isDecimalDigit(l.peekByte(1)) {
			return stateNumericLiteral
		}
		return statePunctuator
	case '#':
		return statePrivateIdentifier
	case '\\':
		return stateIdentifierName
	}
	if isDecimalDigit(c) {
		return stateNumericLiteral
	}
	if c < 0x80 {
		if l.alt.asciiIDStart(c) {
			return stateIdentifierName
		}
		if punctStart[c] {
			return statePunctuator
		}
		return stateInvalid
	}
	r, _ := decodeRune(l.src, l.pos)
	switch {
	case r == cpLS || r == cpPS:
		return stateLineTerminator
	case isWhiteSpace(r):
		return stateWhiteSpace
	case l.alt.idStart(r):
		return stateIdentifierName
	}
	return stateInvalid
}

var punctStart = func() (t [128]bool) {
	for _, c := range "{}()[];,<>=!+-*/%&|^~?:." {
		t[c] = true
	}
	return t
}()

// finish stamps the newline flag and goal on a completed token.
func (l *Lexer) finish(t Token, goal Goal) Token {
	if l.newline {
		t.Flags |= FlagNewlineBefore
	}
	t.Goal = goal
	l.newline = false
	if t.Kind != TEOF {
		l.sawToken = true
	}
	return t
}

// skipToLineEnd advances to the next LineTerminator (not consumed).
func (l *Lexer) skipToLineEnd() {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == '\n' || c == '\r' {
			return
		}
		if c == 0xE2 {
			if r, _ := decodeRune(l.src, l.pos); r == cpLS || r == cpPS {
				return
			}
		}
		l.pos++
	}
}

// scanIdentifierName recognises IdentifierName starting at start
// (12.7). Escapes are decoded into Value and flagged; the code point an
// escape denotes must itself be ID_Start / ID_Continue.
func (l *Lexer) scanIdentifierName(start int) Token {
	i := start
	src := l.src
	if l.alt.ASCIIClass == ASCIIBranches {
		for i < len(src) && src[i] < 0x80 && l.alt.asciiIDPart(src[i]) {
			i++
		}
	} else {
		// The default's loop stays free of the candidate test: it is the
		// lexer's hottest loop.
		for i < len(src) {
			c := src[i]
			if c < 0x80 && asciiIdent[c]&2 != 0 {
				i++
				continue
			}
			break
		}
	}
	if i >= len(src) || (src[i] != '\\' && src[i] < 0x80) {
		l.pos = i
		v := src[start:i]
		return Token{Kind: TIdentifier, Kw: l.alt.keyword(v), Start: int32(start), End: int32(i), Value: v}
	}
	// Slow path: escapes or non-ASCII.
	l.pos = start
	buf := l.buf[:0]
	var flags TokenFlags
	var err Code
	errAt := int32(-1)
	first := true
	for l.pos < len(src) {
		c := src[l.pos]
		if c == '\\' {
			escStart := l.pos
			flags |= FlagEscaped
			r, ok := l.scanUnicodeEscapeInIdent()
			valid := ok && ((first && l.alt.idStart(r)) || (!first && l.alt.idContinue(r)))
			if !valid && err == CodeNone {
				err = CodeInvalidIdentifierEscape
				errAt = int32(escStart)
			}
			if ok {
				buf = appendWTF8(buf, r)
			}
			first = false
			continue
		}
		r, n := decodeRune(src, l.pos)
		if (first && !l.alt.idStart(r)) || (!first && !l.alt.idContinue(r)) {
			break
		}
		buf = append(buf, src[l.pos:l.pos+n]...)
		l.pos += n
		first = false
	}
	l.buf = buf
	v := string(buf)
	if flags&FlagEscaped == 0 {
		v = src[start:l.pos]
	}
	return Token{Kind: TIdentifier, Kw: l.alt.keyword(v), Flags: flags, Start: int32(start), End: int32(l.pos), Value: v, Err: err, ErrAt: errAt}
}

// scanUnicodeEscapeInIdent consumes `\u` HexDigits or `\u{...}` at l.pos.
func (l *Lexer) scanUnicodeEscapeInIdent() (rune, bool) {
	l.pos++ // backslash
	if l.pos >= len(l.src) || l.src[l.pos] != 'u' {
		return 0, false
	}
	l.pos++
	return l.scanUnicodeEscapeBody()
}

// scanUnicodeEscapeBody consumes the part of a UnicodeEscapeSequence after
// `\u`: four hex digits or {CodePoint}. On failure it leaves l.pos at the
// first byte that does not fit.
func (l *Lexer) scanUnicodeEscapeBody() (rune, bool) {
	src := l.src
	if l.pos < len(src) && src[l.pos] == '{' {
		l.pos++
		var v rune
		digits := 0
		for l.pos < len(src) && isHexDigit(src[l.pos]) {
			v = v*16 + rune(hexVal(src[l.pos]))
			if v > 0x10FFFF {
				v = 0x110000
			}
			l.pos++
			digits++
		}
		if digits == 0 || l.pos >= len(src) || src[l.pos] != '}' || v > 0x10FFFF {
			return 0, false
		}
		l.pos++
		return v, true
	}
	if l.pos+4 > len(src) {
		return 0, false
	}
	var v rune
	for k := 0; k < 4; k++ {
		c := src[l.pos]
		if !isHexDigit(c) {
			return 0, false
		}
		v = v*16 + rune(hexVal(c))
		l.pos++
	}
	return v, true
}

// scanPunctuator recognises the longest Punctuator at start (12.8).
func (l *Lexer) scanPunctuator(start int) Token {
	p1, p2, p3 := l.peekByte(1), l.peekByte(2), l.peekByte(3)
	k, n := TInvalid, 1
	switch l.src[start] {
	case '{':
		k = TLBrace
	case '}':
		k = TRBrace
	case '(':
		k = TLParen
	case ')':
		k = TRParen
	case '[':
		k = TLBracket
	case ']':
		k = TRBracket
	case ';':
		k = TSemicolon
	case ',':
		k = TComma
	case ':':
		k = TColon
	case '~':
		k = TTilde
	case '.':
		k = TDot
		if p1 == '.' && p2 == '.' {
			k, n = TEllipsis, 3
		}
	case '<':
		k = TLt
		switch {
		case p1 == '<' && p2 == '=':
			k, n = TShlEq, 3
		case p1 == '<':
			k, n = TShl, 2
		case p1 == '=':
			k, n = TLtEq, 2
		}
	case '>':
		k = TGt
		switch {
		case p1 == '>' && p2 == '>' && p3 == '=':
			k, n = TUShrEq, 4
		case p1 == '>' && p2 == '>':
			k, n = TUShr, 3
		case p1 == '>' && p2 == '=':
			k, n = TShrEq, 3
		case p1 == '>':
			k, n = TShr, 2
		case p1 == '=':
			k, n = TGtEq, 2
		}
	case '=':
		k = TEq
		switch {
		case p1 == '=' && p2 == '=':
			k, n = TEqEqEq, 3
		case p1 == '=':
			k, n = TEqEq, 2
		case p1 == '>':
			k, n = TArrow, 2
		}
	case '!':
		k = TBang
		switch {
		case p1 == '=' && p2 == '=':
			k, n = TNotEqEq, 3
		case p1 == '=':
			k, n = TNotEq, 2
		}
	case '+':
		k = TPlus
		switch p1 {
		case '+':
			k, n = TPlusPlus, 2
		case '=':
			k, n = TPlusEq, 2
		}
	case '-':
		k = TMinus
		switch p1 {
		case '-':
			k, n = TMinusMinus, 2
		case '=':
			k, n = TMinusEq, 2
		}
	case '*':
		k = TStar
		switch {
		case p1 == '*' && p2 == '=':
			k, n = TStarStarEq, 3
		case p1 == '*':
			k, n = TStarStar, 2
		case p1 == '=':
			k, n = TStarEq, 2
		}
	case '/':
		k = TSlash
		if p1 == '=' {
			k, n = TSlashEq, 2
		}
	case '%':
		k = TPercent
		if p1 == '=' {
			k, n = TPercentEq, 2
		}
	case '&':
		k = TAmp
		switch {
		case p1 == '&' && p2 == '=':
			k, n = TAmpAmpEq, 3
		case p1 == '&':
			k, n = TAmpAmp, 2
		case p1 == '=':
			k, n = TAmpEq, 2
		}
	case '|':
		k = TPipe
		switch {
		case p1 == '|' && p2 == '=':
			k, n = TPipePipeEq, 3
		case p1 == '|':
			k, n = TPipePipe, 2
		case p1 == '=':
			k, n = TPipeEq, 2
		}
	case '^':
		k = TCaret
		if p1 == '=' {
			k, n = TCaretEq, 2
		}
	case '?':
		k = TQuestion
		switch {
		case p1 == '?' && p2 == '=':
			k, n = TQuestionQuestionEq, 3
		case p1 == '?':
			k, n = TQuestionQuestion, 2
		case p1 == '.' && !isDecimalDigit(p2):
			// OptionalChainingPunctuator :: ?. [lookahead ∉ DecimalDigit]
			k, n = TQuestionDot, 2
		}
	}
	l.pos = start + n
	t := Token{Kind: k, Start: int32(start), End: int32(l.pos)}
	if k == TInvalid {
		t.Err, t.ErrAt = CodeUnexpectedCharacter, int32(start)
	}
	return t
}
