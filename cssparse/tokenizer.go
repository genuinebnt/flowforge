package cssparse

import (
	"strconv"
	"unicode/utf8"
)

// Options configure tokenization and parsing. The zero value follows the
// current CSS Syntax 3 token set.
type Options struct {
	// LegacyTokens enables the tokens the 2014 Candidate Recommendation
	// still had and the community css-parsing-tests suite still expects:
	// <unicode-range-token> and the match tokens ~= |= ^= $= *= ||.
	LegacyTokens bool
	// MaxDepth bounds the nesting of blocks and functions. A block that
	// would open deeper is elided (its tokens stay in the token list, but
	// the tree holds one DiagDepthLimit error node for it). It also bounds
	// how deep URL extraction re-parses nested rule blocks. 0 means
	// DefaultMaxDepth.
	MaxDepth int
	// MaxInputBytes bounds the input read; the rest is ignored with a
	// DiagInputTooLarge diagnostic. 0 means DefaultMaxInputBytes.
	MaxInputBytes int
	// MaxURLs bounds the URL references extracted per document; the rest
	// are dropped with DiagLimitExceeded. 0 means DefaultMaxURLs.
	MaxURLs int
}

// Default limits, identical in every port (V2_DESIGN parser assurance
// bounds).
const (
	DefaultMaxDepth      = 64
	DefaultMaxInputBytes = 8 << 20
	DefaultMaxURLs       = 20000
)

func (o Options) maxDepth() int {
	if o.MaxDepth > 0 {
		return o.MaxDepth
	}
	return DefaultMaxDepth
}

func (o Options) maxInput() int {
	if o.MaxInputBytes > 0 {
		return o.MaxInputBytes
	}
	return DefaultMaxInputBytes
}

func (o Options) maxURLs() int {
	if o.MaxURLs > 0 {
		return o.MaxURLs
	}
	return DefaultMaxURLs
}

// limitInput cuts src to the input limit at a UTF-8 boundary and reports
// whether it cut.
func limitInput(src string, max int) (string, bool) {
	if len(src) <= max {
		return src, false
	}
	cut := max
	for cut > 0 && cut > max-3 && src[cut]&0xC0 == 0x80 {
		cut--
	}
	return src[:cut], true
}

// tokState is the tokenizer state. CSS Syntax 3 defines tokenization as a
// set of "consume" algorithms; each state below is one of them, entered
// from stateConsumeToken and returning to it after emitting one token.
type tokState uint8

const (
	// stateConsumeToken: §4.3.1 "Consume a token" (the dispatch on the
	// next input code point).
	stateConsumeToken tokState = iota
	// stateComment: §4.3.2 "Consume comments" (kept as a trivia token).
	stateComment
	// stateWhitespace: §4.3.1, whitespace branch.
	stateWhitespace
	// stateString: §4.3.5 "Consume a string token".
	stateString
	// stateHash: §4.3.1, U+0023 NUMBER SIGN branch.
	stateHash
	// stateAtKeyword: §4.3.1, U+0040 COMMERCIAL AT branch.
	stateAtKeyword
	// stateNumeric: §4.3.3 "Consume a numeric token".
	stateNumeric
	// stateIdentLike: §4.3.4 "Consume an ident-like token".
	stateIdentLike
	// stateURL: §4.3.6 "Consume a url token".
	stateURL
	// stateBadURLRemnants: §4.3.14 "Consume the remnants of a bad url".
	stateBadURLRemnants
	// stateUnicodeRange: 2014 CR §4.3.7 "Consume a unicode-range token"
	// (Options.LegacyTokens only).
	stateUnicodeRange
)

const eof rune = -1

type tokenizer struct {
	src    string
	pos    int
	legacy bool
	toks   []Token
	diags  []Diagnostic
	// steps counts state-machine iterations; tests bound it by a constant
	// times the input length (CSS-INV-04).
	steps int
}

// Tokenize splits src into tokens following CSS Syntax 3 §4. It never
// fails: errors are returned as diagnostics, and the token spans tile src
// exactly (up to Options.MaxInputBytes; past it a DiagInputTooLarge
// diagnostic is recorded and the rest is not tokenized). Token values are
// substrings of a private copy of src.
func Tokenize(src []byte, opts Options) ([]Token, []Diagnostic) {
	t := tokenizeString(string(src), opts)
	return t.toks, t.diags
}

func tokenizeString(src string, opts Options) *tokenizer {
	full := len(src)
	src, cut := limitInput(src, opts.maxInput())
	t := &tokenizer{src: src, legacy: opts.LegacyTokens, toks: make([]Token, 0, len(src)/4+4)}
	t.run()
	if cut {
		t.diag(DiagInputTooLarge, len(src), full)
	}
	return t
}

// cpAt returns the code point at byte offset i after §3.3 preprocessing
// (CR LF, CR and FF become LF; U+0000 and invalid UTF-8 become U+FFFD) and
// the number of source bytes it occupies. At the end it returns eof, 0.
func (t *tokenizer) cpAt(i int) (rune, int) {
	if i >= len(t.src) {
		return eof, 0
	}
	b := t.src[i]
	if b < utf8.RuneSelf {
		switch b {
		case '\r':
			if i+1 < len(t.src) && t.src[i+1] == '\n' {
				return '\n', 2
			}
			return '\n', 1
		case '\f':
			return '\n', 1
		case 0:
			return utf8.RuneError, 1
		}
		return rune(b), 1
	}
	return utf8.DecodeRuneInString(t.src[i:])
}

// peek3 returns the three code points starting at byte offset i.
func (t *tokenizer) peek3(i int) (rune, rune, rune) {
	c1, w1 := t.cpAt(i)
	c2, w2 := t.cpAt(i + w1)
	c3, _ := t.cpAt(i + w1 + w2)
	return c1, c2, c3
}

// literal reports whether the code point decoded at i with width w is
// spelled literally in the source (false for preprocessing replacements).
func (t *tokenizer) literal(r rune, i, w int) bool {
	if r != utf8.RuneError {
		return true
	}
	return w == 3 && t.src[i:i+3] == "�"
}

func isDigit(c rune) bool { return c >= '0' && c <= '9' }

func isHex(c rune) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func hexVal(c rune) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return int(c-'A') + 10
	}
}

// isIdentStart is §4.2 "ident-start code point" (2021 CR definition: any
// non-ASCII code point qualifies).
func isIdentStart(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c >= 0x80
}

// isIdentChar is §4.2 "ident code point".
func isIdentChar(c rune) bool { return isIdentStart(c) || isDigit(c) || c == '-' }

func isWhitespace(c rune) bool { return c == '\n' || c == '\t' || c == ' ' }

// isNonPrintable is §4.2 "non-printable code point".
func isNonPrintable(c rune) bool {
	return (c >= 0 && c <= 8) || c == 0x0B || (c >= 0x0E && c <= 0x1F) || c == 0x7F
}

// validEscape is §4.3.8 "Check if two code points are a valid escape".
func validEscape(c1, c2 rune) bool { return c1 == '\\' && c2 != '\n' }

// wouldStartIdent is §4.3.9 "Check if three code points would start an
// ident sequence".
func wouldStartIdent(c1, c2, c3 rune) bool {
	switch {
	case c1 == '-':
		return isIdentStart(c2) || c2 == '-' || validEscape(c2, c3)
	case c1 == '\\':
		return validEscape(c1, c2)
	default:
		return isIdentStart(c1)
	}
}

// startsNumber is §4.3.10 "Check if three code points would start a
// number".
func startsNumber(c1, c2, c3 rune) bool {
	switch {
	case c1 == '+' || c1 == '-':
		return isDigit(c2) || (c2 == '.' && isDigit(c3))
	case c1 == '.':
		return isDigit(c2)
	default:
		return isDigit(c1)
	}
}

func (t *tokenizer) emit(tok Token) { t.toks = append(t.toks, tok) }

func (t *tokenizer) diag(code DiagCode, start, end int) {
	t.diags = append(t.diags, Diagnostic{Code: code, Start: int32(start), End: int32(end)})
}

func (t *tokenizer) simple(kind TokenKind, start, n int) {
	t.emit(Token{Kind: kind, Start: int32(start), End: int32(start + n)})
	t.pos = start + n
}

func (t *tokenizer) delim(start, n int) {
	t.emit(Token{Kind: TokenDelim, Start: int32(start), End: int32(start + n), Value: t.src[start : start+n]})
	t.pos = start + n
}

// valueBuf accumulates a decoded value. While every code point is spelled
// literally it only tracks a source range; the first replacement or
// escape switches it to an owned buffer.
type valueBuf struct {
	segStart int
	buf      []byte
	owned    bool
}

func (v *valueBuf) own(src string, upTo int) {
	if !v.owned {
		v.owned = true
		v.buf = append(make([]byte, 0, upTo-v.segStart+16), src[v.segStart:upTo]...)
	}
}

func (v *valueBuf) lit(src string, i, w int) {
	if v.owned {
		v.buf = append(v.buf, src[i:i+w]...)
	}
}

func (v *valueBuf) rune(r rune) { v.buf = utf8.AppendRune(v.buf, r) }

func (v *valueBuf) value(src string, end int) string {
	if v.owned {
		return string(v.buf)
	}
	return src[v.segStart:end]
}

// consumeEscape is §4.3.7 "Consume an escaped code point"; i is the
// offset just after the backslash. It returns the code point and the
// offset after the escape.
func (t *tokenizer) consumeEscape(i int) (rune, int) {
	c, w := t.cpAt(i)
	if isHex(c) {
		v := 0
		for n := 0; n < 6; n++ {
			t.steps++
			c, w = t.cpAt(i)
			if !isHex(c) {
				break
			}
			v = v*16 + hexVal(c)
			i += w
		}
		if c, w = t.cpAt(i); isWhitespace(c) {
			i += w
		}
		if v == 0 || (v >= 0xD800 && v <= 0xDFFF) || v > utf8.MaxRune {
			return utf8.RuneError, i
		}
		return rune(v), i
	}
	if c == eof {
		t.diag(DiagEOFInEscape, i-1, i)
		return utf8.RuneError, i
	}
	return c, i + w
}

// consumeIdentSequence is §4.3.11 "Consume an ident sequence" from byte
// offset i. It returns the decoded name and the offset after it.
func (t *tokenizer) consumeIdentSequence(i int) (string, int) {
	v := valueBuf{segStart: i}
	for {
		t.steps++
		c, w := t.cpAt(i)
		if isIdentChar(c) {
			if t.literal(c, i, w) {
				v.lit(t.src, i, w)
			} else {
				v.own(t.src, i)
				v.rune(c)
			}
			i += w
			continue
		}
		if c2, _ := t.cpAt(i + w); validEscape(c, c2) {
			v.own(t.src, i)
			r, n := t.consumeEscape(i + 1)
			v.rune(r)
			i = n
			continue
		}
		return v.value(t.src, i), i
	}
}

// consumeNumber is §4.3.12 "Consume a number" from byte offset i. It
// returns the value, whether the type is "integer", and the end offset.
func (t *tokenizer) consumeNumber(i int) (float64, bool, int) {
	s := t.src
	start := i
	digitAt := func(j int) bool { return j < len(s) && s[j] >= '0' && s[j] <= '9' }
	digits := func() {
		for digitAt(i) {
			t.steps++
			i++
		}
	}
	integer := true
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digits()
	if i+1 < len(s) && s[i] == '.' && digitAt(i+1) {
		integer = false
		i += 2
		digits()
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		switch {
		case digitAt(i + 1):
			integer = false
			i += 2
			digits()
		case i+1 < len(s) && (s[i+1] == '+' || s[i+1] == '-') && digitAt(i+2):
			integer = false
			i += 3
			digits()
		}
	}
	// §4.3.13 "Convert a string to a number". ParseFloat gives the
	// correctly rounded double; out-of-range values become ±Inf or 0.
	v, _ := strconv.ParseFloat(s[start:i], 64)
	return v, integer, i
}

func (t *tokenizer) run() {
	st := stateConsumeToken
	start := 0
	var quote rune
	for {
		t.steps++
		switch st {
		case stateConsumeToken:
			if t.pos >= len(t.src) {
				return
			}
			start = t.pos
			c, w := t.cpAt(start)
			c2, c3, c4 := t.peek3(start + w)
			switch {
			case c == '/' && c2 == '*':
				st = stateComment
			case isWhitespace(c):
				st = stateWhitespace
			case c == '"' || c == '\'':
				quote = c
				st = stateString
			case c == '#':
				if isIdentChar(c2) || validEscape(c2, c3) {
					st = stateHash
				} else {
					t.delim(start, 1)
				}
			case c == '(':
				t.simple(TokenOpenParen, start, 1)
			case c == ')':
				t.simple(TokenCloseParen, start, 1)
			case c == '+':
				if startsNumber(c, c2, c3) {
					st = stateNumeric
				} else {
					t.delim(start, 1)
				}
			case c == ',':
				t.simple(TokenComma, start, 1)
			case c == '-':
				switch {
				case startsNumber(c, c2, c3):
					st = stateNumeric
				case c2 == '-' && c3 == '>':
					t.simple(TokenCDC, start, 3)
				case wouldStartIdent(c, c2, c3):
					st = stateIdentLike
				default:
					t.delim(start, 1)
				}
			case c == '.':
				if startsNumber(c, c2, c3) {
					st = stateNumeric
				} else {
					t.delim(start, 1)
				}
			case c == ':':
				t.simple(TokenColon, start, 1)
			case c == ';':
				t.simple(TokenSemicolon, start, 1)
			case c == '<':
				if c2 == '!' && c3 == '-' && c4 == '-' {
					t.simple(TokenCDO, start, 4)
				} else {
					t.delim(start, 1)
				}
			case c == '@':
				if wouldStartIdent(c2, c3, c4) {
					st = stateAtKeyword
				} else {
					t.delim(start, 1)
				}
			case c == '[':
				t.simple(TokenOpenSquare, start, 1)
			case c == ']':
				t.simple(TokenCloseSquare, start, 1)
			case c == '{':
				t.simple(TokenOpenCurly, start, 1)
			case c == '}':
				t.simple(TokenCloseCurly, start, 1)
			case c == '\\':
				if validEscape(c, c2) {
					st = stateIdentLike
				} else {
					t.diag(DiagInvalidEscape, start, start+1)
					t.delim(start, 1)
				}
			case isDigit(c):
				st = stateNumeric
			case t.legacy && (c == 'u' || c == 'U') && c2 == '+' && (isHex(c3) || c3 == '?'):
				st = stateUnicodeRange
			case isIdentStart(c):
				st = stateIdentLike
			case t.legacy && c2 == '=' && (c == '~' || c == '|' || c == '^' || c == '$' || c == '*'):
				kind := map2[c]
				t.simple(kind, start, 2)
			case t.legacy && c == '|' && c2 == '|':
				t.simple(TokenColumn, start, 2)
			default:
				t.delim(start, w)
			}

		case stateComment:
			i := start + 2
			end := -1
			for j := i; j+1 < len(t.src); j++ {
				t.steps++
				if t.src[j] == '*' && t.src[j+1] == '/' {
					end = j + 2
					break
				}
			}
			tok := Token{Kind: TokenComment, Start: int32(start)}
			if end < 0 {
				end = len(t.src)
				tok.Flags = FlagUnterminated
				t.diag(DiagEOFInComment, start, end)
			}
			tok.End = int32(end)
			t.emit(tok)
			t.pos = end
			st = stateConsumeToken

		case stateWhitespace:
			i := start
			for {
				t.steps++
				c, w := t.cpAt(i)
				if !isWhitespace(c) {
					break
				}
				i += w
			}
			t.simple(TokenWhitespace, start, i-start)
			st = stateConsumeToken

		case stateString:
			t.consumeString(start, quote)
			st = stateConsumeToken

		case stateHash:
			c1, c2, c3 := t.peek3(start + 1)
			var flags TokenFlags
			if wouldStartIdent(c1, c2, c3) {
				flags = FlagHashID
			}
			name, end := t.consumeIdentSequence(start + 1)
			t.emit(Token{Kind: TokenHash, Flags: flags, Start: int32(start), End: int32(end), Value: name})
			t.pos = end
			st = stateConsumeToken

		case stateAtKeyword:
			name, end := t.consumeIdentSequence(start + 1)
			t.emit(Token{Kind: TokenAtKeyword, Start: int32(start), End: int32(end), Value: name})
			t.pos = end
			st = stateConsumeToken

		case stateNumeric:
			v, integer, i := t.consumeNumber(start)
			tok := Token{Start: int32(start), ReprEnd: int32(i), Number: v}
			if integer {
				tok.Flags = FlagInteger
			}
			c1, c2, c3 := t.peek3(i)
			switch {
			case wouldStartIdent(c1, c2, c3):
				tok.Kind = TokenDimension
				tok.Value, i = t.consumeIdentSequence(i)
			case c1 == '%':
				tok.Kind = TokenPercentage
				i++
			default:
				tok.Kind = TokenNumber
			}
			tok.End = int32(i)
			t.emit(tok)
			t.pos = i
			st = stateConsumeToken

		case stateIdentLike:
			name, i := t.consumeIdentSequence(start)
			c, _ := t.cpAt(i)
			switch {
			case c == '(' && asciiEqualFold(name, "url"):
				j := i + 1
				for {
					t.steps++
					c1, w1 := t.cpAt(j)
					c2, _ := t.cpAt(j + w1)
					if !isWhitespace(c1) || !isWhitespace(c2) {
						break
					}
					j += w1
				}
				c1, w1 := t.cpAt(j)
				c2, _ := t.cpAt(j + w1)
				if c1 == '"' || c1 == '\'' || (isWhitespace(c1) && (c2 == '"' || c2 == '\'')) {
					t.emit(Token{Kind: TokenFunction, Start: int32(start), End: int32(j), Value: name})
					t.pos = j
					st = stateConsumeToken
				} else {
					t.pos = j
					st = stateURL
				}
			case c == '(':
				t.emit(Token{Kind: TokenFunction, Start: int32(start), End: int32(i + 1), Value: name})
				t.pos = i + 1
				st = stateConsumeToken
			default:
				t.emit(Token{Kind: TokenIdent, Start: int32(start), End: int32(i), Value: name})
				t.pos = i
				st = stateConsumeToken
			}

		case stateURL:
			if t.consumeURL(start) {
				st = stateConsumeToken
			} else {
				st = stateBadURLRemnants
			}

		case stateBadURLRemnants:
			i := t.pos
			for {
				t.steps++
				c, w := t.cpAt(i)
				if c == ')' {
					i++
					break
				}
				if c == eof {
					break
				}
				if c2, _ := t.cpAt(i + w); validEscape(c, c2) {
					_, i = t.consumeEscape(i + 1)
					continue
				}
				i += w
			}
			t.emit(Token{Kind: TokenBadURL, Start: int32(start), End: int32(i)})
			t.pos = i
			st = stateConsumeToken

		case stateUnicodeRange:
			t.consumeUnicodeRange(start)
			st = stateConsumeToken
		}
	}
}

var map2 = [128]TokenKind{
	'~': TokenIncludeMatch, '|': TokenDashMatch, '^': TokenPrefixMatch,
	'$': TokenSuffixMatch, '*': TokenSubstringMatch,
}

// consumeString implements §4.3.5 from the opening quote at start.
func (t *tokenizer) consumeString(start int, quote rune) {
	i := start + 1
	v := valueBuf{segStart: i}
	for {
		t.steps++
		c, w := t.cpAt(i)
		switch {
		case c == eof:
			t.diag(DiagEOFInString, start, i)
			t.emit(Token{Kind: TokenString, Flags: FlagUnterminated, Start: int32(start), End: int32(i), Value: v.value(t.src, i)})
			t.pos = i
			return
		case c == quote:
			t.emit(Token{Kind: TokenString, Start: int32(start), End: int32(i + w), Value: v.value(t.src, i)})
			t.pos = i + w
			return
		case c == '\n':
			t.diag(DiagNewlineInString, start, i)
			t.emit(Token{Kind: TokenBadString, Start: int32(start), End: int32(i)})
			t.pos = i
			return
		case c == '\\':
			c2, w2 := t.cpAt(i + 1)
			v.own(t.src, i)
			switch {
			case c2 == eof:
				i++
			case c2 == '\n':
				i += 1 + w2
			default:
				var r rune
				r, i = t.consumeEscape(i + 1)
				v.rune(r)
			}
		default:
			if t.literal(c, i, w) {
				v.lit(t.src, i, w)
			} else {
				v.own(t.src, i)
				v.rune(c)
			}
			i += w
		}
	}
}

// consumeURL implements §4.3.6 from t.pos (just after "url(" and any
// whitespace the ident-like algorithm consumed). It reports false when the
// bad-url remnants must be consumed from t.pos.
func (t *tokenizer) consumeURL(start int) bool {
	i := t.pos
	for {
		c, w := t.cpAt(i)
		if !isWhitespace(c) {
			break
		}
		i += w
	}
	v := valueBuf{segStart: i}
	for {
		t.steps++
		c, w := t.cpAt(i)
		switch {
		case c == ')':
			t.emit(Token{Kind: TokenURL, Start: int32(start), End: int32(i + 1), Value: v.value(t.src, i)})
			t.pos = i + 1
			return true
		case c == eof:
			t.diag(DiagEOFInURL, start, i)
			t.emit(Token{Kind: TokenURL, Flags: FlagUnterminated, Start: int32(start), End: int32(i), Value: v.value(t.src, i)})
			t.pos = i
			return true
		case isWhitespace(c):
			valEnd := i
			for {
				t.steps++
				c, w = t.cpAt(i)
				if !isWhitespace(c) {
					break
				}
				i += w
			}
			switch c {
			case ')':
				t.emit(Token{Kind: TokenURL, Start: int32(start), End: int32(i + 1), Value: v.value(t.src, valEnd)})
				t.pos = i + 1
				return true
			case eof:
				t.diag(DiagEOFInURL, start, i)
				t.emit(Token{Kind: TokenURL, Flags: FlagUnterminated, Start: int32(start), End: int32(i), Value: v.value(t.src, valEnd)})
				t.pos = i
				return true
			}
			t.diag(DiagBadURL, i, i+w)
			t.pos = i
			return false
		case c == '"' || c == '\'' || c == '(' || isNonPrintable(c):
			t.diag(DiagBadURL, i, i+w)
			t.pos = i
			return false
		case c == '\\':
			if c2, _ := t.cpAt(i + 1); validEscape(c, c2) {
				v.own(t.src, i)
				var r rune
				r, i = t.consumeEscape(i + 1)
				v.rune(r)
				continue
			}
			t.diag(DiagBadURL, i, i+1)
			t.pos = i
			return false
		default:
			if t.literal(c, i, w) {
				v.lit(t.src, i, w)
			} else {
				v.own(t.src, i)
				v.rune(c)
			}
			i += w
		}
	}
}

// consumeUnicodeRange implements the 2014 CR §4.3.7 from "u+" at start.
func (t *tokenizer) consumeUnicodeRange(start int) {
	s := t.src
	i := start + 2
	hexAt := func(j int) bool { return j < len(s) && isHex(rune(s[j])) }
	var lo, hi uint32
	n := 0
	for n < 6 && hexAt(i) {
		t.steps++
		lo = lo*16 + uint32(hexVal(rune(s[i])))
		i++
		n++
	}
	hi = lo
	q := 0
	for n < 6 && i < len(s) && s[i] == '?' {
		t.steps++
		lo *= 16
		hi = hi*16 + 15
		i++
		n++
		q++
	}
	if q == 0 && i+1 < len(s) && s[i] == '-' && hexAt(i+1) {
		i++
		hi = 0
		for m := 0; m < 6 && hexAt(i); m++ {
			t.steps++
			hi = hi*16 + uint32(hexVal(rune(s[i])))
			i++
		}
	}
	t.emit(Token{Kind: TokenUnicodeRange, Start: int32(start), End: int32(i), RangeStart: lo, RangeEnd: hi})
	t.pos = i
}

// asciiEqualFold reports whether a equals the ASCII-lower-case b,
// ignoring ASCII case only (CSS "ASCII case-insensitive match").
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		c := a[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != b[i] {
			return false
		}
	}
	return true
}
