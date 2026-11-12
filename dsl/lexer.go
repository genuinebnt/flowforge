package dsl

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxInterpDepth bounds the nesting of string interpolations inside
// string interpolations.
const maxInterpDepth = 32

// Duration units in nanoseconds (spec 7.2).
var durationUnits = map[string]int64{
	"ms": 1_000_000,
	"s":  1_000_000_000,
	"m":  60 * 1_000_000_000,
	"h":  3600 * 1_000_000_000,
	"d":  86400 * 1_000_000_000,
}

// Lex splits src into tokens. The result always ends with a TokEOF token.
// Lexical errors are reported as diagnostics; lexing continues after each
// error so that later errors are reported too.
func Lex(file string, src []byte) ([]Token, []Diagnostic) {
	sink := newDiagSink(file)
	l := &lexer{src: src, sink: sink, line: 1, col: 1}
	toks := l.run(0)
	return toks, sink.diags
}

type lexer struct {
	src  []byte
	off  int
	line int
	col  int
	sink *diagSink
}

func (l *lexer) pos() Pos { return Pos{Line: l.line, Col: l.col, Offset: l.off} }

// peekByte returns the byte at off+i, or 0 past the end.
func (l *lexer) peekByte(i int) byte {
	if l.off+i < len(l.src) {
		return l.src[l.off+i]
	}
	return 0
}

// advance consumes one code point (or one invalid byte).
func (l *lexer) advance() rune {
	if l.off >= len(l.src) {
		return 0
	}
	r, size := utf8.DecodeRune(l.src[l.off:])
	l.off += size
	if r == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return r
}

func (l *lexer) spanFrom(start Pos) Span { return Span{Start: start, End: l.pos()} }

// run lexes tokens until EOF, or, when depth > 0, until the `}` that closes
// a string interpolation (which is consumed but not emitted).
func (l *lexer) run(depth int) []Token {
	var toks []Token
	braces := 0
	for {
		l.skipTrivia()
		if l.off >= len(l.src) {
			toks = append(toks, Token{Kind: TokEOF, Span: l.spanFrom(l.pos())})
			return toks
		}
		if depth > 0 {
			c := l.src[l.off]
			if c == '}' && braces == 0 {
				p := l.pos()
				toks = append(toks, Token{Kind: TokEOF, Span: Span{Start: p, End: p}})
				l.advance()
				return toks
			}
		}
		tok, ok := l.next(depth)
		if !ok {
			continue
		}
		switch tok.Kind {
		case TokLBrace:
			braces++
		case TokRBrace:
			braces--
		}
		toks = append(toks, tok)
	}
}

// skipTrivia skips whitespace and comments.
func (l *lexer) skipTrivia() {
	for l.off < len(l.src) {
		c := l.src[l.off]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			l.advance()
		case c == '/' && l.peekByte(1) == '/':
			for l.off < len(l.src) && l.src[l.off] != '\n' {
				l.advance()
			}
		case c == '/' && l.peekByte(1) == '*':
			l.blockComment()
		default:
			return
		}
	}
}

// blockComment skips a nestable /* ... */ comment.
func (l *lexer) blockComment() {
	start := l.pos()
	l.advance()
	l.advance()
	depth := 1
	for l.off < len(l.src) {
		if l.src[l.off] == '/' && l.peekByte(1) == '*' {
			l.advance()
			l.advance()
			depth++
			continue
		}
		if l.src[l.off] == '*' && l.peekByte(1) == '/' {
			l.advance()
			l.advance()
			depth--
			if depth == 0 {
				return
			}
			continue
		}
		l.advance()
	}
	l.sink.errorf(CodeUnterminatedComment, Span{Start: start, End: Pos{Line: start.Line, Col: start.Col + 2, Offset: start.Offset + 2}},
		"comment starts here", "unterminated block comment")
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentChar(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHexDigit(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// next lexes one token. ok is false when the input produced no token (an
// invalid character, already reported).
func (l *lexer) next(depth int) (Token, bool) {
	start := l.pos()
	c := l.src[l.off]
	switch {
	case isIdentStart(c):
		for l.off < len(l.src) && isIdentChar(l.src[l.off]) {
			l.advance()
		}
		text := string(l.src[start.Offset:l.off])
		kind := TokIdent
		if k, ok := keywords[text]; ok {
			kind = k
		}
		return Token{Kind: kind, Span: l.spanFrom(start), Text: text}, true
	case isDigit(c):
		return l.number(start), true
	case c == '"':
		return l.str(start, depth), true
	}
	two := ""
	if l.off+1 < len(l.src) {
		two = string(l.src[l.off : l.off+2])
	}
	if k, ok := twoCharOps[two]; ok {
		l.advance()
		l.advance()
		return Token{Kind: k, Span: l.spanFrom(start), Text: two}, true
	}
	if k, ok := oneCharOps[c]; ok {
		l.advance()
		return Token{Kind: k, Span: l.spanFrom(start), Text: string(c)}, true
	}
	r, size := utf8.DecodeRune(l.src[l.off:])
	l.advance()
	if r == utf8.RuneError && size <= 1 {
		l.sink.errorf(CodeUnexpectedChar, l.spanFrom(start), "invalid byte", "invalid UTF-8 in source")
	} else {
		l.sink.errorf(CodeUnexpectedChar, l.spanFrom(start), "unexpected character",
			"unexpected character %s", strconv.QuoteRune(r))
	}
	return Token{}, false
}

var twoCharOps = map[string]TokenKind{
	"==": TokEq, "!=": TokNe, "<=": TokLe, ">=": TokGe, "&&": TokAndAnd,
	"||": TokOrOr, "|>": TokPipe, "=>": TokFatArrow, "->": TokArrow,
	"..": TokDotDot, "::": TokColons,
}

var oneCharOps = map[byte]TokenKind{
	'+': TokPlus, '-': TokMinus, '*': TokStar, '/': TokSlash, '%': TokPercent,
	'<': TokLt, '>': TokGt, '!': TokBang, '?': TokQuestion, '.': TokDot,
	'=': TokAssign, ':': TokColon, ',': TokComma, ';': TokSemi, '(': TokLParen,
	')': TokRParen, '[': TokLBracket, ']': TokRBracket, '{': TokLBrace,
	'}': TokRBrace, '#': TokHash,
}

// number lexes an integer, float or duration literal.
func (l *lexer) number(start Pos) Token {
	// Hexadecimal integer.
	if l.src[l.off] == '0' && (l.peekByte(1) == 'x' || l.peekByte(1) == 'X') {
		l.advance()
		l.advance()
		digStart := l.off
		for l.off < len(l.src) && (isHexDigit(l.src[l.off]) || l.src[l.off] == '_') {
			l.advance()
		}
		digits := strings.ReplaceAll(string(l.src[digStart:l.off]), "_", "")
		// Trailing identifier characters make the literal malformed.
		bad := false
		for l.off < len(l.src) && isIdentChar(l.src[l.off]) {
			l.advance()
			bad = true
		}
		tok := Token{Kind: TokInt, Span: l.spanFrom(start), Text: string(l.src[start.Offset:l.off])}
		if digits == "" || bad {
			l.sink.errorf(CodeMalformedNumber, tok.Span, "invalid hexadecimal literal",
				"malformed hexadecimal literal `%s`", tok.Text)
			return tok
		}
		v, err := strconv.ParseUint(digits, 16, 64)
		if err != nil || v > math.MaxInt64 {
			l.sink.errorf(CodeLiteralTooLarge, tok.Span, "does not fit in 64-bit signed integer",
				"integer literal `%s` is too large", tok.Text).
				withHelp("The largest Int is 9223372036854775807 (0x7fffffffffffffff).")
			return tok
		}
		tok.Int = int64(v)
		return tok
	}
	for l.off < len(l.src) && (isDigit(l.src[l.off]) || l.src[l.off] == '_') {
		l.advance()
	}
	isFloat := false
	if l.peekByte(0) == '.' && isDigit(l.peekByte(1)) {
		isFloat = true
		l.advance()
		for l.off < len(l.src) && (isDigit(l.src[l.off]) || l.src[l.off] == '_') {
			l.advance()
		}
		if c := l.peekByte(0); c == 'e' || c == 'E' {
			save := *l
			l.advance()
			if c := l.peekByte(0); c == '+' || c == '-' {
				l.advance()
			}
			if !isDigit(l.peekByte(0)) {
				// Not an exponent; leave the letters for the suffix check.
				*l = save
			} else {
				for l.off < len(l.src) && isDigit(l.src[l.off]) {
					l.advance()
				}
			}
		}
	}
	numEnd := l.off
	numText := strings.ReplaceAll(string(l.src[start.Offset:numEnd]), "_", "")
	// Unit suffix for durations.
	suffixStart := l.off
	for l.off < len(l.src) && isIdentChar(l.src[l.off]) {
		l.advance()
	}
	suffix := string(l.src[suffixStart:l.off])
	tok := Token{Span: l.spanFrom(start), Text: string(l.src[start.Offset:l.off])}
	if isFloat {
		tok.Kind = TokFloat
		if suffix != "" {
			l.sink.errorf(CodeUnknownDurationUnit, tok.Span, "unexpected suffix",
				"float literal `%s` cannot have the suffix `%s`", tok.Text, suffix).
				withHelp("Durations are whole numbers of a unit: write 1500ms instead of 1.5s.")
			return tok
		}
		f, err := strconv.ParseFloat(numText, 64)
		if err != nil || math.IsInf(f, 0) {
			l.sink.errorf(CodeLiteralTooLarge, tok.Span, "out of range for Float",
				"float literal `%s` is out of range", tok.Text)
			return tok
		}
		tok.Float = f
		return tok
	}
	v, err := strconv.ParseUint(numText, 10, 64)
	if err != nil || v > math.MaxInt64 {
		tok.Kind = TokInt
		if suffix != "" {
			tok.Kind = TokDuration
		}
		l.sink.errorf(CodeLiteralTooLarge, tok.Span, "does not fit in 64-bit signed integer",
			"integer literal `%s` is too large", tok.Text).
			withHelp("The largest Int is 9223372036854775807.")
		return tok
	}
	if suffix == "" {
		tok.Kind = TokInt
		tok.Int = int64(v)
		return tok
	}
	tok.Kind = TokDuration
	unit, ok := durationUnits[suffix]
	if !ok {
		l.sink.errorf(CodeUnknownDurationUnit, Span{Start: Pos{Line: start.Line, Col: start.Col + (suffixStart - start.Offset), Offset: suffixStart}, End: tok.Span.End},
			"unknown unit", "unknown duration unit `%s`", suffix).
			withHelp("Duration units are ms, s, m, h and d.")
		return tok
	}
	if v > uint64(math.MaxInt64/unit) {
		l.sink.errorf(CodeLiteralTooLarge, tok.Span, "does not fit in 64-bit nanoseconds",
			"duration literal `%s` is too large", tok.Text)
		return tok
	}
	tok.Int = int64(v) * unit
	return tok
}

// str lexes a string literal with escapes and interpolations.
func (l *lexer) str(start Pos, depth int) Token {
	l.advance() // opening quote
	var parts []StrPartToken
	var lit strings.Builder
	litStart := l.pos()
	flush := func() {
		if lit.Len() > 0 {
			parts = append(parts, StrPartToken{Lit: lit.String(), Span: l.spanFrom(litStart)})
			lit.Reset()
		}
	}
	for {
		if l.off >= len(l.src) {
			l.sink.errorf(CodeUnterminatedString, Span{Start: start, End: Pos{Line: start.Line, Col: start.Col + 1, Offset: start.Offset + 1}},
				"string starts here", "unterminated string literal").
				withHelp("Close the string with a double quote.")
			flush()
			return Token{Kind: TokString, Span: l.spanFrom(start), Text: string(l.src[start.Offset:l.off]), Parts: parts}
		}
		c := l.src[l.off]
		switch c {
		case '"':
			flush()
			l.advance()
			return Token{Kind: TokString, Span: l.spanFrom(start), Text: string(l.src[start.Offset:l.off]), Parts: parts}
		case '\\':
			l.escape(&lit)
		case '{':
			if l.peekByte(1) == '{' {
				l.advance()
				l.advance()
				lit.WriteByte('{')
				continue
			}
			flush()
			open := l.pos()
			l.advance()
			exprStart := l.pos()
			if depth+1 > maxInterpDepth {
				l.sink.errorf(CodeBadInterpolation, l.spanFrom(open), "nested too deeply",
					"string interpolation nested more than %d levels", maxInterpDepth)
				// Skip to the end of the input; nothing sensible remains.
				for l.off < len(l.src) {
					l.advance()
				}
				continue
			}
			inner := l.run(depth + 1)
			exprSpan := Span{Start: exprStart, End: inner[len(inner)-1].Span.Start}
			if len(inner) == 1 {
				l.sink.errorf(CodeBadInterpolation, l.spanFrom(open), "empty interpolation",
					"empty string interpolation `{}`").
					withHelp("Write {{}} for literal braces.")
			}
			parts = append(parts, StrPartToken{Interp: true, Tokens: inner, Span: exprSpan})
			litStart = l.pos()
		case '}':
			sp := l.pos()
			l.advance()
			if l.peekByte(0) == '}' {
				l.advance()
				lit.WriteByte('}')
				continue
			}
			l.sink.errorf(CodeBadInterpolation, l.spanFrom(sp), "unmatched `}`",
				"unmatched `}` in string literal").withHelp("Write }} for a literal brace.")
			lit.WriteByte('}')
		default:
			r, size := utf8.DecodeRune(l.src[l.off:])
			sp := l.pos()
			l.advance()
			if r == utf8.RuneError && size <= 1 {
				l.sink.errorf(CodeUnexpectedChar, l.spanFrom(sp), "invalid byte", "invalid UTF-8 in string literal")
				continue
			}
			lit.WriteRune(r)
		}
	}
}

// escape decodes one backslash escape into b.
func (l *lexer) escape(b *strings.Builder) {
	start := l.pos()
	l.advance() // backslash
	c := l.peekByte(0)
	switch c {
	case 'n':
		l.advance()
		b.WriteByte('\n')
		return
	case 't':
		l.advance()
		b.WriteByte('\t')
		return
	case '"':
		l.advance()
		b.WriteByte('"')
		return
	case '\\':
		l.advance()
		b.WriteByte('\\')
		return
	case 'u':
		l.advance()
		if l.peekByte(0) != '{' {
			l.sink.errorf(CodeInvalidEscape, l.spanFrom(start), "expected `{`",
				"invalid unicode escape").withHelp("Write \\u{1F600}.")
			return
		}
		l.advance()
		hexStart := l.off
		for l.off < len(l.src) && isHexDigit(l.src[l.off]) {
			l.advance()
		}
		hex := string(l.src[hexStart:l.off])
		if l.peekByte(0) != '}' {
			l.sink.errorf(CodeInvalidEscape, l.spanFrom(start), "expected `}`",
				"invalid unicode escape").withHelp("Write \\u{1F600}.")
			return
		}
		l.advance()
		v, err := strconv.ParseUint(hex, 16, 32)
		if hex == "" || len(hex) > 6 || err != nil || v > utf8.MaxRune || (v >= 0xD800 && v <= 0xDFFF) {
			l.sink.errorf(CodeInvalidEscape, l.spanFrom(start), "not a Unicode scalar value",
				"invalid unicode escape `\\u{%s}`", hex)
			return
		}
		b.WriteRune(rune(v))
		return
	}
	if l.off < len(l.src) {
		l.advance()
	}
	l.sink.errorf(CodeInvalidEscape, l.spanFrom(start), "unknown escape",
		"invalid escape sequence `%s`", string(l.src[start.Offset:l.off])).
		withHelp(`Valid escapes are \n \t \" \\ and \u{HEX}; write {{ and }} for literal braces.`)
}
