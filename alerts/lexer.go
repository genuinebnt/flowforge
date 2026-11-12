package alerts

import (
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// tokKind classifies a token.
type tokKind uint8

const (
	tEOF tokKind = iota
	tIdent
	tKeyword
	tInt
	tFloat
	tDuration
	tString
	tOp
	tBad // an invalid literal, already diagnosed; parses as an expression
)

// keywords reserved by the alert language. Clause words (throttle,
// group_by, ...) and the word operators (contains, matches, ...) are
// contextual identifiers so they stay usable as function names.
func isKeyword(s string) bool {
	switch s {
	case "alert", "when", "true", "false", "in":
		return true
	}
	return false
}

// token is one lexeme. Literal payloads are decoded by the lexer.
type token struct {
	kind tokKind
	text string // identifier, keyword or operator text; raw text otherwise
	sp   span
	ival int64 // tInt; tDuration in nanoseconds
	fval float64
	sval string // decoded tString
}

func (t token) is(kind tokKind, text string) bool { return t.kind == kind && t.text == text }

// lexer turns source text into tokens, reporting lexical diagnostics to
// the sink and continuing after each error.
type lexer struct {
	src  string
	pos  int
	diag *diagSink
	// runToEOF is set when an unterminated string or comment swallowed the
	// rest of the input; the parser then stays silent about the missing
	// tail instead of piling on a second error.
	runToEOF bool
	tokStart int // start of the lexeme next returned
}

// lex tokenizes src. Invalid literals become tBad tokens so the parser
// can continue without reporting a cascade of follow-on errors.
func lex(src string, diag *diagSink) (toks []token, runToEOF bool) {
	lx := &lexer{src: src, diag: diag}
	for {
		t, ok := lx.next()
		if !ok {
			if c := src[lx.tokStart]; c == '"' || isDigit(c) {
				toks = append(toks, token{kind: tBad, text: src[lx.tokStart:lx.pos], sp: span{lx.tokStart, lx.pos}})
			}
			continue
		}
		toks = append(toks, t)
		if t.kind == tEOF {
			return toks, lx.runToEOF
		}
	}
}

// next returns the next token; ok is false when the lexeme was invalid
// (a diagnostic has been recorded) and should be skipped.
func (lx *lexer) next() (token, bool) {
	lx.skipSpaceAndComments()
	if lx.pos >= len(lx.src) {
		return token{kind: tEOF, sp: span{len(lx.src), len(lx.src)}}, true
	}
	start := lx.pos
	lx.tokStart = start
	c := lx.src[lx.pos]
	switch {
	case isIdentStart(c):
		for lx.pos < len(lx.src) && isIdentPart(lx.src[lx.pos]) {
			lx.pos++
		}
		text := lx.src[start:lx.pos]
		kind := tIdent
		if isKeyword(text) {
			kind = tKeyword
		}
		return token{kind: kind, text: text, sp: span{start, lx.pos}}, true
	case isDigit(c):
		return lx.number(start)
	case c == '"':
		return lx.str(start)
	}
	if op := matchOperator(lx.src[lx.pos:]); op != "" {
		lx.pos += len(op)
		return token{kind: tOp, text: op, sp: span{start, lx.pos}}, true
	}
	_, size := utf8.DecodeRuneInString(lx.src[lx.pos:])
	lx.pos += size
	lx.diag.add(CodeUnexpectedChar, span{start, lx.pos}, "not valid here", "unexpected character %q", lx.src[start:lx.pos])
	return token{}, false
}

func (lx *lexer) skipSpaceAndComments() {
	for lx.pos < len(lx.src) {
		switch c := lx.src[lx.pos]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			lx.pos++
		case strings.HasPrefix(lx.src[lx.pos:], "//"):
			for lx.pos < len(lx.src) && lx.src[lx.pos] != '\n' {
				lx.pos++
			}
		case strings.HasPrefix(lx.src[lx.pos:], "/*"):
			lx.blockComment()
		default:
			return
		}
	}
}

// blockComment skips a nestable /* ... */ comment.
func (lx *lexer) blockComment() {
	start := lx.pos
	depth := 0
	for lx.pos < len(lx.src) {
		switch {
		case strings.HasPrefix(lx.src[lx.pos:], "/*"):
			depth++
			lx.pos += 2
		case strings.HasPrefix(lx.src[lx.pos:], "*/"):
			depth--
			lx.pos += 2
			if depth == 0 {
				return
			}
		default:
			lx.pos++
		}
	}
	lx.runToEOF = true
	lx.diag.add(CodeUnterminatedComment, span{start, start + 2}, "comment starts here", "unterminated block comment")
}

func (lx *lexer) number(start int) (token, bool) {
	src := lx.src
	if strings.HasPrefix(src[lx.pos:], "0x") || strings.HasPrefix(src[lx.pos:], "0X") {
		lx.pos += 2
		for lx.pos < len(src) && (isHexDigit(src[lx.pos]) || src[lx.pos] == '_') {
			lx.pos++
		}
		digits := strings.ReplaceAll(src[start+2:lx.pos], "_", "")
		if lx.pos < len(src) && isIdentPart(src[lx.pos]) {
			return lx.badSuffix(start)
		}
		if digits == "" {
			lx.diag.add(CodeInvalidNumber, span{start, lx.pos}, "no digits", "hexadecimal literal has no digits")
			return token{}, false
		}
		v, err := strconv.ParseInt(digits, 16, 64)
		if err != nil {
			lx.diag.add(CodeIntegerTooLarge, span{start, lx.pos}, "does not fit in 64 bits", "integer literal too large")
			return token{}, false
		}
		return token{kind: tInt, text: src[start:lx.pos], sp: span{start, lx.pos}, ival: v}, true
	}
	lx.digits()
	isFloat := false
	if lx.pos+1 < len(src) && src[lx.pos] == '.' && isDigit(src[lx.pos+1]) {
		isFloat = true
		lx.pos++
		lx.digits()
		if lx.pos < len(src) && (src[lx.pos] == 'e' || src[lx.pos] == 'E') {
			save := lx.pos
			lx.pos++
			if lx.pos < len(src) && (src[lx.pos] == '+' || src[lx.pos] == '-') {
				lx.pos++
			}
			if lx.pos < len(src) && isDigit(src[lx.pos]) {
				for lx.pos < len(src) && isDigit(src[lx.pos]) {
					lx.pos++
				}
			} else {
				lx.pos = save
			}
		}
	}
	text := src[start:lx.pos]
	clean := strings.ReplaceAll(text, "_", "")
	if isFloat {
		if lx.pos < len(src) && isIdentPart(src[lx.pos]) {
			return lx.badSuffix(start)
		}
		f, err := strconv.ParseFloat(clean, 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			lx.diag.add(CodeInvalidNumber, span{start, lx.pos}, "not a finite float", "invalid float literal")
			return token{}, false
		}
		return token{kind: tFloat, text: text, sp: span{start, lx.pos}, fval: f}, true
	}
	v, err := strconv.ParseInt(clean, 10, 64)
	if err != nil {
		lx.skipIdent()
		lx.diag.add(CodeIntegerTooLarge, span{start, lx.pos}, "does not fit in 64 bits", "integer literal too large")
		return token{}, false
	}
	if lx.pos < len(src) && isIdentStart(src[lx.pos]) {
		unitStart := lx.pos
		lx.skipIdent()
		unit := src[unitStart:lx.pos]
		d, ok := durationUnit(unit)
		if !ok {
			lx.diag.add(CodeUnknownDurationUnit, span{unitStart, lx.pos}, "unknown unit", "unknown duration unit %q", unit).
				Help = "use one of ms, s, m, h, d"
			return token{}, false
		}
		if v > math.MaxInt64/int64(d) {
			lx.diag.add(CodeIntegerTooLarge, span{start, lx.pos}, "does not fit in 64-bit nanoseconds", "duration literal too large")
			return token{}, false
		}
		return token{kind: tDuration, text: src[start:lx.pos], sp: span{start, lx.pos}, ival: v * int64(d)}, true
	}
	return token{kind: tInt, text: text, sp: span{start, lx.pos}, ival: v}, true
}

func (lx *lexer) badSuffix(start int) (token, bool) {
	lx.skipIdent()
	lx.diag.add(CodeInvalidNumber, span{start, lx.pos}, "invalid suffix", "invalid numeric literal %q", lx.src[start:lx.pos])
	return token{}, false
}

func (lx *lexer) digits() {
	for lx.pos < len(lx.src) && (isDigit(lx.src[lx.pos]) || lx.src[lx.pos] == '_') {
		lx.pos++
	}
}

func (lx *lexer) skipIdent() {
	for lx.pos < len(lx.src) && isIdentPart(lx.src[lx.pos]) {
		lx.pos++
	}
}

func durationUnit(u string) (time.Duration, bool) {
	switch u {
	case "ms":
		return time.Millisecond, true
	case "s":
		return time.Second, true
	case "m":
		return time.Minute, true
	case "h":
		return time.Hour, true
	case "d":
		return 24 * time.Hour, true
	}
	return 0, false
}

// str lexes a string literal. Interpolation ({expr}) is part of the DSL
// lexical grammar but not of the alert language; {{ and }} still denote
// literal braces and a lone brace is diagnosed.
func (lx *lexer) str(start int) (token, bool) {
	src := lx.src
	lx.pos++ // opening quote
	var b strings.Builder
	ok := true
	for {
		if lx.pos >= len(src) {
			lx.runToEOF = true
			lx.diag.add(CodeUnterminatedString, span{start, len(src)}, "string starts here", "unterminated string literal").
				Help = `close the string with "`
			return token{}, false
		}
		c := src[lx.pos]
		switch {
		case c == '"':
			lx.pos++
			if !ok {
				return token{}, false
			}
			return token{kind: tString, text: src[start:lx.pos], sp: span{start, lx.pos}, sval: b.String()}, true
		case c == '\\':
			if !lx.escape(&b) {
				ok = false
			}
		case c == '{' || c == '}':
			if lx.pos+1 < len(src) && src[lx.pos+1] == c {
				b.WriteByte(c)
				lx.pos += 2
				continue
			}
			lx.diag.add(CodeInterpolation, span{lx.pos, lx.pos + 1}, "lone brace", "string interpolation is not supported in alert rules").
				Help = "write {{ and }} for literal braces"
			ok = false
			lx.pos++
		default:
			b.WriteByte(c)
			lx.pos++
		}
	}
}

func (lx *lexer) escape(b *strings.Builder) bool {
	src := lx.src
	start := lx.pos
	lx.pos++ // backslash
	if lx.pos >= len(src) {
		return true // reported as unterminated by the caller
	}
	c := src[lx.pos]
	lx.pos++
	switch c {
	case 'n':
		b.WriteByte('\n')
		return true
	case 't':
		b.WriteByte('\t')
		return true
	case '"':
		b.WriteByte('"')
		return true
	case '\\':
		b.WriteByte('\\')
		return true
	case 'u':
		if r, ok := lx.unicodeEscape(); ok {
			b.WriteRune(r)
			return true
		}
		lx.diag.add(CodeInvalidEscape, span{start, lx.pos}, "not a Unicode scalar value", "invalid unicode escape").
			Help = `write \u{HEX} with 1 to 6 hex digits`
		return false
	}
	_, size := utf8.DecodeRuneInString(src[lx.pos-1:])
	lx.pos += size - 1
	lx.diag.add(CodeInvalidEscape, span{start, lx.pos}, "unknown escape", "invalid escape sequence %q", src[start:lx.pos]).
		Help = `valid escapes are \n \t \" \\ and \u{HEX}`
	return false
}

// unicodeEscape decodes the {HEX} part of \u{HEX}; the position is left
// after the closing brace when one is found on success or failure.
func (lx *lexer) unicodeEscape() (rune, bool) {
	src := lx.src
	if lx.pos >= len(src) || src[lx.pos] != '{' {
		return 0, false
	}
	end := strings.IndexAny(src[lx.pos:], "}\"\n")
	if end < 0 || src[lx.pos+end] != '}' {
		return 0, false
	}
	hex := src[lx.pos+1 : lx.pos+end]
	lx.pos += end + 1
	if len(hex) == 0 || len(hex) > 6 {
		return 0, false
	}
	v, err := strconv.ParseUint(hex, 16, 32)
	if err != nil || !utf8.ValidRune(rune(v)) {
		return 0, false
	}
	return rune(v), true
}

// matchOperator returns the longest operator at the start of s, or "".
func matchOperator(s string) string {
	if len(s) >= 2 {
		switch two := s[:2]; two {
		case "..", "==", "!=", "<=", ">=", "&&", "||", "|>", "=>", "->", "::":
			return two
		}
	}
	if len(s) >= 1 && strings.IndexByte("+-*/%<>!?.=:,;()[]{}#", s[0]) >= 0 {
		return s[:1]
	}
	return ""
}

func isIdentStart(c byte) bool { return c == '_' || (c|0x20 >= 'a' && c|0x20 <= 'z') }
func isIdentPart(c byte) bool  { return isIdentStart(c) || isDigit(c) }
func isDigit(c byte) bool      { return c >= '0' && c <= '9' }
func isHexDigit(c byte) bool   { return isDigit(c) || (c|0x20 >= 'a' && c|0x20 <= 'f') }
