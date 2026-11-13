package jsparse

// Lexer turns ECMAScript source text into input elements (ECMA-262 clause
// 12). It is an explicit state machine (see lexer_states.go): Next runs
// the machine from stateInputElement until one token is complete.
//
// The lexer does not know the syntactic context, so every call names the
// lexical goal. The parser is the only component that decides goals
// (README "Goal-selection protocol").
//
// A Lexer keeps its source as a string copy of the input bytes; token
// values are substrings of it (or freshly built strings when escapes had
// to be decoded).
type Lexer struct {
	src    string
	pos    int
	module bool

	// newline is true when a LineTerminator (or a multi-line comment
	// containing one) was skipped since the previous token.
	newline bool
	// sawToken is true once a non-trivia token was produced; before that
	// an HTML close comment is allowed at the start of the input.
	sawToken bool

	recordTrivia bool
	trivia       []Token
	comments     []Comment

	buf []byte // scratch for decoded values
}

// Comment is a comment in the source (12.4, B.1.1).
type Comment struct {
	Kind TokenKind // TLineComment, TBlockComment, THTMLOpenComment, THTMLCloseComment or THashbang
	Span Span
}

// NewLexer returns a lexer over src. In a module, HTML-like comments are
// not recognised (B.1.1 applies to scripts only).
func NewLexer(src []byte, module bool) *Lexer {
	return newLexer(string(src), module)
}

func newLexer(src string, module bool) *Lexer {
	return &Lexer{src: src, module: module}
}

// RecordTrivia makes the lexer keep white space, line terminator and
// comment elements; Trivia returns them.
func (l *Lexer) RecordTrivia(on bool) { l.recordTrivia = on }

// Trivia returns the trivia recorded so far, in source order.
func (l *Lexer) Trivia() []Token { return l.trivia }

// Comments returns the comments skipped so far, in source order.
func (l *Lexer) Comments() []Comment { return l.comments }

// Pos returns the offset of the next unread byte.
func (l *Lexer) Pos() int { return l.pos }

// lexerState is the resumable part of the lexer: enough to re-lex from an
// earlier token under a different goal.
type lexerState struct {
	pos         int
	newline     bool
	sawToken    bool
	nTrivia     int
	nComments   int
	afterTokPos int
}

func (l *Lexer) save() lexerState {
	return lexerState{pos: l.pos, newline: l.newline, sawToken: l.sawToken, nTrivia: len(l.trivia), nComments: len(l.comments)}
}

func (l *Lexer) restore(s lexerState) {
	l.pos = s.pos
	l.newline = s.newline
	l.sawToken = s.sawToken
	l.trivia = l.trivia[:s.nTrivia]
	l.comments = l.comments[:s.nComments]
}

// rewindTo moves the lexer back to the start of tok so that the token can
// be lexed again under another goal. Trivia before tok are kept; the
// newline flag is restored from the token.
func (l *Lexer) rewindTo(tok *Token) {
	l.pos = int(tok.Start)
	l.newline = tok.Flags&FlagNewlineBefore != 0
	for len(l.trivia) > 0 && int(l.trivia[len(l.trivia)-1].Start) >= l.pos {
		l.trivia = l.trivia[:len(l.trivia)-1]
	}
	for len(l.comments) > 0 && int(l.comments[len(l.comments)-1].Span.Start) >= l.pos {
		l.comments = l.comments[:len(l.comments)-1]
	}
}

func (l *Lexer) addTrivia(kind TokenKind, start int) {
	if kind >= TLineComment {
		l.comments = append(l.comments, Comment{Kind: kind, Span: Span{int32(start), int32(l.pos)}})
	}
	if l.recordTrivia {
		l.trivia = append(l.trivia, Token{Kind: kind, Start: int32(start), End: int32(l.pos)})
	}
}

// peekByte returns the byte at pos+off or 0 past the end.
func (l *Lexer) peekByte(off int) byte {
	if i := l.pos + off; i < len(l.src) {
		return l.src[i]
	}
	return 0
}

// decodeRune decodes the code point at i. Invalid UTF-8 yields
// (utf8.RuneError, 1).
func decodeRune(s string, i int) (rune, int) {
	c := s[i]
	if c < 0x80 {
		return rune(c), 1
	}
	return decodeRuneSlow(s, i)
}

func decodeRuneSlow(s string, i int) (rune, int) {
	// Manual decoding keeps the state machine free of the utf8 package's
	// string conversions; it follows RFC 3629 exactly.
	c := s[i]
	n := len(s) - i
	switch {
	case c&0xE0 == 0xC0 && n >= 2 && c >= 0xC2 && isCont(s[i+1]):
		return rune(c&0x1F)<<6 | rune(s[i+1]&0x3F), 2
	case c&0xF0 == 0xE0 && n >= 3 && isCont(s[i+1]) && isCont(s[i+2]):
		r := rune(c&0x0F)<<12 | rune(s[i+1]&0x3F)<<6 | rune(s[i+2]&0x3F)
		if r < 0x800 {
			return 0xFFFD, 1
		}
		// Surrogate code points (WTF-8, produced by our own escapes) decode
		// as themselves; they never satisfy identifier or space predicates.
		return r, 3
	case c&0xF8 == 0xF0 && n >= 4 && isCont(s[i+1]) && isCont(s[i+2]) && isCont(s[i+3]):
		r := rune(c&0x07)<<18 | rune(s[i+1]&0x3F)<<12 | rune(s[i+2]&0x3F)<<6 | rune(s[i+3]&0x3F)
		if r < 0x10000 || r > 0x10FFFF {
			return 0xFFFD, 1
		}
		return r, 4
	}
	return 0xFFFD, 1
}

func isCont(b byte) bool { return b&0xC0 == 0x80 }

// appendWTF8 appends r in UTF-8, encoding lone surrogates (which string
// escapes can produce) as three-byte WTF-8 sequences so that no code unit
// is lost.
func appendWTF8(b []byte, r rune) []byte {
	switch {
	case r < 0x80:
		return append(b, byte(r))
	case r < 0x800:
		return append(b, 0xC0|byte(r>>6), 0x80|byte(r)&0x3F)
	case r < 0x10000:
		return append(b, 0xE0|byte(r>>12), 0x80|byte(r>>6)&0x3F, 0x80|byte(r)&0x3F)
	default:
		return append(b, 0xF0|byte(r>>18), 0x80|byte(r>>12)&0x3F, 0x80|byte(r>>6)&0x3F, 0x80|byte(r)&0x3F)
	}
}

// Tokenize lexes src completely with a fixed goal policy and returns all
// elements including trivia. It exists for tools and tests that need a
// token stream without a parse; because it has no syntactic context it
// uses GoalDiv for every token, so regular expression literals come out as
// punctuators. Use Parse with Options.Tokens for goal-correct tokens.
func Tokenize(src []byte, module bool) []Token {
	l := newLexer(string(src), module)
	l.recordTrivia = true
	var out []Token
	for {
		t := l.Next(GoalDiv)
		out = mergeTrivia(out, l)
		if t.Kind == TEOF {
			return out
		}
		out = append(out, t)
	}
}

// mergeTrivia moves the trivia recorded by l into out.
func mergeTrivia(out []Token, l *Lexer) []Token {
	out = append(out, l.trivia...)
	l.trivia = l.trivia[:0]
	return out
}
