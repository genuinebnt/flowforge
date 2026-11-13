package textindex

import (
	"unicode"
	"unicode/utf8"
)

// MaxTokenBytes is the longest token (measured on the original text span,
// before folding) the analyzer emits. A longer compound is not emitted; a
// longer part is not emitted but still consumes its position, so phrases
// never bridge it.
const MaxTokenBytes = 128

// TokenKind says how a token relates to the word it came from.
type TokenKind uint8

const (
	// TokenWord is a word that is a single part (no case, digit or '_'
	// boundary inside): the compound and the part are the same token.
	TokenWord TokenKind = iota
	// TokenCompound is the original compound of a word with two or more
	// parts, or with a leading or trailing '_' ("getUserById", "user_id").
	TokenCompound
	// TokenPart is one part of a compound ("get", "user", "by", "id").
	TokenPart
)

// String returns "word", "compound" or "part".
func (k TokenKind) String() string {
	switch k {
	case TokenWord:
		return "word"
	case TokenCompound:
		return "compound"
	case TokenPart:
		return "part"
	}
	return "unknown"
}

// Token is one analyzer output. Term is case-folded; Start and End are
// byte offsets of the original span in the input; Pos is the token
// position. A compound shares the position of its first part.
type Token struct {
	Term  string
	Pos   int
	Start int
	End   int
	Kind  TokenKind
}

// Analyze runs the code/URL analyzer over text (README "Analyzer rules").
// It never fails: invalid UTF-8 bytes are delimiters.
func Analyze(text []byte) []Token {
	var a analyzer
	var out []Token
	a.run(text, func(term []byte, pos, start, end int, kind TokenKind) {
		out = append(out, Token{Term: string(term), Pos: pos, Start: start, End: end, Kind: kind})
	})
	return out
}

// FieldLength is the number of positions Analyze assigns to text: the
// field length dl used by BM25.
func FieldLength(text []byte) int {
	var a analyzer
	return a.run(text, func([]byte, int, int, int, TokenKind) {})
}

// runeClass is the analyzer's character class.
type runeClass uint8

const (
	classOther     runeClass = iota // delimiter: everything not below
	classConnector                  // '_' (snake_case)
	classUpper                      // Lu, Lt
	classLower                      // other letters (Ll, Lm, Lo) and marks (M*)
	classDigit                      // Nd
)

// asciiClass is classOf for ASCII, as a table.
var asciiClass = func() (t [128]runeClass) {
	for c := 0; c < 128; c++ {
		switch {
		case c >= 'A' && c <= 'Z':
			t[c] = classUpper
		case c >= 'a' && c <= 'z':
			t[c] = classLower
		case c >= '0' && c <= '9':
			t[c] = classDigit
		case c == '_':
			t[c] = classConnector
		}
	}
	return t
}()

// classOf classifies r (README "Analyzer rules" 1).
//
// PORT: the unicode.Is* predicates use Go's Unicode 15.0 tables. Rust's
// char methods follow the toolchain's Unicode version; generate fixed
// tables for 15.0 so token boundaries match the conformance vectors.
func classOf(r rune) runeClass {
	if r < utf8.RuneSelf {
		return asciiClass[r]
	}
	switch {
	case r == utf8.RuneError:
		return classOther
	case unicode.IsUpper(r) || unicode.IsTitle(r):
		return classUpper
	case unicode.IsLetter(r) || unicode.Is(unicode.M, r):
		return classLower
	case unicode.IsDigit(r):
		return classDigit
	}
	return classOther
}

// foldRune is the analyzer's case folding: toLower(toUpper(r)) with the
// single-rune (simple) mappings of UnicodeData.txt, so all case variants
// of a rune fold to one rune (TIDX-INV-11). ASCII takes a fast path.
//
// PORT: use the simple mappings of Unicode 15.0, not Rust's
// char::to_lowercase/to_uppercase, which are full mappings ("İ" lowercases
// to two runes there). See OPEN_QUESTIONS 3.
func foldRune(r rune) rune {
	if r < utf8.RuneSelf {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}
	return unicode.ToLower(unicode.ToUpper(r))
}

// appendFold appends the case fold of src to dst.
func appendFold(dst, src []byte) []byte {
	for i := 0; i < len(src); {
		c := src[i]
		if c < utf8.RuneSelf {
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			dst = append(dst, c)
			i++
			continue
		}
		r, w := utf8.DecodeRune(src[i:])
		if r == utf8.RuneError && w == 1 {
			dst = append(dst, c)
			i++
			continue
		}
		dst = utf8.AppendRune(dst, foldRune(r))
		i += w
	}
	return dst
}

// tokState is the state of the analyzer's word-segmentation machine. The
// machine is specified by README "Analyzer rules"; states are named after
// what has been consumed of the current word.
type tokState uint8

const (
	// stDelimiter: outside any word.
	stDelimiter tokState = iota
	// stConnector: inside a word, after '_' (no part open).
	stConnector
	// stUpper: in a part opened by exactly one upper-case letter.
	stUpper
	// stUpperRun: in a part of two or more upper-case letters (acronym).
	stUpperRun
	// stLower: in a part whose last letter is lower-case (or caseless).
	stLower
	// stDigit: in a part of digits.
	stDigit
)

// span is a half-open byte range of the input.
type span struct{ start, end int }

// analyzer holds reusable buffers. It is not safe for concurrent use.
type analyzer struct {
	parts []span
	buf   []byte
}

// emitFunc receives one token; term is only valid during the call.
type emitFunc func(term []byte, pos, start, end int, kind TokenKind)

// run segments text and calls emit for each token in output order
// (positions non-decreasing; a compound before its parts). It returns the
// number of positions used.
func (a *analyzer) run(text []byte, emit emitFunc) int {
	pos := 0
	state := stDelimiter
	wordStart, partStart, lastStart := 0, 0, 0
	a.parts = a.parts[:0]

	endPart := func(end int) { a.parts = append(a.parts, span{partStart, end}) }
	endWord := func(end int) {
		pos = a.emitWord(text, wordStart, end, pos, emit)
	}

	for i := 0; i < len(text); {
		r, w := rune(text[i]), 1
		if r >= utf8.RuneSelf {
			// PORT: an invalid byte decodes as (RuneError, 1) and is a
			// delimiter; a valid U+FFFD is a delimiter too (class So).
			r, w = utf8.DecodeRune(text[i:])
		}
		c := classOf(r)
		switch state {
		case stDelimiter:
			switch c {
			case classConnector:
				wordStart, state = i, stConnector
			case classUpper:
				wordStart, partStart, state = i, i, stUpper
			case classLower:
				wordStart, partStart, state = i, i, stLower
			case classDigit:
				wordStart, partStart, state = i, i, stDigit
			}
		case stConnector:
			switch c {
			case classOther:
				endWord(i)
				state = stDelimiter
			case classUpper:
				partStart, state = i, stUpper
			case classLower:
				partStart, state = i, stLower
			case classDigit:
				partStart, state = i, stDigit
			}
		case stUpper, stUpperRun, stLower, stDigit:
			switch {
			case c == classOther:
				endPart(i)
				endWord(i)
				state = stDelimiter
			case c == classConnector:
				endPart(i)
				state = stConnector
			case state == stUpper && c == classUpper:
				state = stUpperRun
			case state == stUpper && c == classLower:
				state = stLower // "Get": one capital starts a word part
			case state == stUpperRun && c == classLower:
				// "HTTPServer": the last capital of a run starts the next part.
				endPart(lastStart)
				partStart, state = lastStart, stLower
			case state == stUpperRun && c == classUpper,
				state == stLower && c == classLower,
				state == stDigit && c == classDigit:
				// no boundary: the run of this class goes on
			default:
				// lower→upper, letter↔digit
				endPart(i)
				partStart = i
				switch c {
				case classUpper:
					state = stUpper
				case classLower:
					state = stLower
				default:
					state = stDigit
				}
			}
		}
		lastStart = i
		i += w
	}
	switch state {
	case stUpper, stUpperRun, stLower, stDigit:
		endPart(len(text))
		endWord(len(text))
	case stConnector:
		endWord(len(text))
	}
	return pos
}

// emitWord emits the tokens of the word text[start:end] whose parts are
// in a.parts, starting at position pos, and returns the next position.
func (a *analyzer) emitWord(text []byte, start, end, pos int, emit emitFunc) int {
	parts := a.parts
	a.parts = a.parts[:0]
	if len(parts) == 0 {
		return pos // only connectors
	}
	if len(parts) == 1 && parts[0].start == start && parts[0].end == end {
		if end-start <= MaxTokenBytes {
			a.buf = appendFold(a.buf[:0], text[start:end])
			emit(a.buf, pos, start, end, TokenWord)
		}
		return pos + 1
	}
	if end-start <= MaxTokenBytes {
		a.buf = appendFold(a.buf[:0], text[start:end])
		emit(a.buf, pos, start, end, TokenCompound)
	}
	for k, p := range parts {
		if p.end-p.start <= MaxTokenBytes {
			a.buf = appendFold(a.buf[:0], text[p.start:p.end])
			emit(a.buf, pos+k, p.start, p.end, TokenPart)
		}
	}
	return pos + len(parts)
}

// Trigrams returns the sorted, distinct trigrams of text: every run of
// three consecutive bytes, with ASCII letters folded to lower case,
// encoded as b0<<16 | b1<<8 | b2.
func Trigrams(text []byte) []uint32 {
	t := appendTrigrams(nil, text)
	return sortUnique32(t)
}

// lowerASCII folds A-Z: trigrams fold ASCII letters only (FORMAT.md),
// so they work on raw bytes without decoding.
func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// appendTrigrams appends every trigram of text, duplicates included.
func appendTrigrams(dst []uint32, text []byte) []uint32 {
	if len(text) < 3 {
		return dst
	}
	t := uint32(lowerASCII(text[0]))<<8 | uint32(lowerASCII(text[1]))
	for i := 2; i < len(text); i++ {
		t = (t<<8 | uint32(lowerASCII(text[i]))) & 0xFFFFFF
		dst = append(dst, t)
	}
	return dst
}
