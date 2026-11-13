package jsparse

import (
	"strings"
	"unicode"
)

// scanRegularExpression recognises RegularExpressionLiteral at start
// (12.9.5): `/` RegularExpressionBody `/` RegularExpressionFlags. Value is
// the body, Raw the flags. Flag and pattern validity are early errors
// checked by the parser through validateRegExp.
func (l *Lexer) scanRegularExpression(start int) Token {
	src := l.src
	l.pos = start + 1
	t := Token{Kind: TRegExp, Start: int32(start)}
	inClass := false
	for {
		if l.pos >= len(src) {
			t.Err, t.ErrAt = CodeUnterminatedRegExp, int32(start)
			t.End = int32(l.pos)
			t.Value = src[start+1 : l.pos]
			return t
		}
		c := src[l.pos]
		r, n := decodeRune(src, l.pos)
		if isLineTerminator(r) {
			t.Err, t.ErrAt = CodeUnterminatedRegExp, int32(start)
			t.End = int32(l.pos)
			t.Value = src[start+1 : l.pos]
			return t
		}
		switch {
		case c == '\\':
			l.pos++
			if l.pos < len(src) {
				r2, n2 := decodeRune(src, l.pos)
				if isLineTerminator(r2) {
					continue // reported as unterminated on the next turn
				}
				l.pos += n2
			}
			continue
		case c == '[':
			inClass = true
		case c == ']':
			inClass = false
		case c == '/' && !inClass:
			t.Value = src[start+1 : l.pos]
			l.pos++
			flagStart := l.pos
			for l.pos < len(src) {
				r, n := decodeRune(src, l.pos)
				if !isIDContinue(r) {
					break
				}
				l.pos += n
			}
			t.Raw = src[flagStart:l.pos]
			t.End = int32(l.pos)
			return t
		}
		l.pos += n
	}
}

// regexpFlags is the set of flags of a regular expression literal.
type regexpFlags struct {
	unicode, unicodeSets bool
}

// validateRegExpFlags checks RegularExpressionFlags (22.2.1.1 early
// errors): only d g i m s u v y, none twice, not both u and v.
func validateRegExpFlags(flags string) (regexpFlags, int, bool) {
	var seen [128]bool
	var f regexpFlags
	for i := 0; i < len(flags); i++ {
		c := flags[i]
		if c >= 128 || !strings.ContainsRune("dgimsuvy", rune(c)) || seen[c] {
			return f, i, false
		}
		seen[c] = true
	}
	f.unicode = seen['u']
	f.unicodeSets = seen['v']
	if f.unicode && f.unicodeSets {
		return f, strings.IndexByte(flags, 'v'), false
	}
	return f, 0, true
}

// reValidator checks a Pattern (22.2.1) with the Annex B.1.2 extensions
// when neither u nor v is set. It validates syntax and the static
// semantics rules of 22.2.1.1 (quantifier bounds, class ranges, group
// names, backreferences); it does not build a matcher. Unicode property
// names are checked against General_Category, the binary properties of
// table 67 and Go's script table (README "Regular expressions").
type reValidator struct {
	src         string
	pos         int
	u           bool // UnicodeMode (u or v)
	v           bool // UnicodeSetsMode
	named       bool // NamedCaptureGroups
	groups      int
	groupNames  []string
	backrefName []string
	maxBackref  int
	err         bool
	errAt       int
}

// validateRegExpPattern reports whether body is a valid Pattern for flags;
// on failure it returns the offset of the offending code point.
func validateRegExpPattern(body string, f regexpFlags) (int, bool) {
	v := &reValidator{src: body, u: f.unicode || f.unicodeSets, v: f.unicodeSets}
	v.prescan()
	v.named = v.u || len(v.groupNames) > 0
	v.groupNames = v.groupNames[:0]
	v.pos = 0
	v.disjunction()
	if !v.err && v.pos < len(v.src) {
		v.fail() // unmatched ')'
	}
	if v.err {
		return v.errAt, false
	}
	// Backreferences: \N with N > groups is an error in UnicodeMode (or
	// whenever named groups exist; Annex B reinterprets it otherwise).
	if v.u && v.maxBackref > v.groups {
		return 0, false
	}
	for _, name := range v.backrefName {
		found := false
		for _, g := range v.groupNames {
			if g == name {
				found = true
				break
			}
		}
		if !found {
			return 0, false
		}
	}
	return 0, true
}

func (v *reValidator) fail() {
	if !v.err {
		v.err = true
		v.errAt = v.pos
	}
}

func (v *reValidator) eof() bool { return v.pos >= len(v.src) }

func (v *reValidator) peek() byte {
	if v.pos < len(v.src) {
		return v.src[v.pos]
	}
	return 0
}

func (v *reValidator) peekAt(off int) byte {
	if v.pos+off < len(v.src) {
		return v.src[v.pos+off]
	}
	return 0
}

// prescan counts capturing groups and collects group names so that
// backreferences and NamedCaptureGroups are known before the main pass.
func (v *reValidator) prescan() {
	inClass := false
	for i := 0; i < len(v.src); i++ {
		c := v.src[i]
		switch {
		case c == '\\':
			i++
		case c == '[':
			inClass = true
		case c == ']':
			inClass = false
		case c == '(' && !inClass:
			if i+1 < len(v.src) && v.src[i+1] == '?' {
				if i+2 < len(v.src) && v.src[i+2] == '<' && i+3 < len(v.src) && v.src[i+3] != '=' && v.src[i+3] != '!' {
					v.groups++
					end := strings.IndexByte(v.src[i+3:], '>')
					if end >= 0 {
						v.groupNames = append(v.groupNames, v.src[i+3:i+3+end])
					}
				}
				continue
			}
			v.groups++
		}
	}
}

// disjunction :: Alternative ( | Alternative )*
func (v *reValidator) disjunction() {
	v.alternative()
	for !v.err && v.peek() == '|' {
		v.pos++
		v.alternative()
	}
}

// alternative :: Term*
func (v *reValidator) alternative() {
	for !v.err && !v.eof() {
		c := v.peek()
		if c == '|' || c == ')' {
			return
		}
		v.term()
	}
}

// term :: Assertion | Atom Quantifier?
func (v *reValidator) term() {
	c := v.peek()
	switch c {
	case '^', '$':
		v.pos++
		return
	case '\\':
		if n := v.peekAt(1); n == 'b' || n == 'B' {
			v.pos += 2
			return
		}
	case '(':
		if v.peekAt(1) == '?' {
			n2, n3 := v.peekAt(2), v.peekAt(3)
			lookahead := n2 == '=' || n2 == '!'
			lookbehind := n2 == '<' && (n3 == '=' || n3 == '!')
			if lookahead || lookbehind {
				v.pos += 3
				if lookbehind {
					v.pos++
				}
				v.disjunction()
				if v.peek() != ')' {
					v.fail()
					return
				}
				v.pos++
				// Annex B: lookaheads are quantifiable without u.
				if lookahead && !v.u {
					v.quantifier()
				} else if v.isQuantifierStart() {
					v.fail()
				}
				return
			}
		}
	}
	v.atom()
	if !v.err {
		v.quantifier()
	}
}

func (v *reValidator) isQuantifierStart() bool {
	switch v.peek() {
	case '*', '+', '?':
		return true
	case '{':
		_, _, ok := v.braceQuantifier(false)
		return ok
	}
	return false
}

// braceQuantifier parses {n}, {n,} or {n,m} at v.pos; with consume it
// advances past it.
func (v *reValidator) braceQuantifier(consume bool) (int, int, bool) {
	i := v.pos + 1
	readInt := func() (int, bool) {
		start := i
		n := 0
		for i < len(v.src) && isDecimalDigit(v.src[i]) {
			if n < 1<<30 {
				n = n*10 + int(v.src[i]-'0')
			}
			i++
		}
		return n, i > start
	}
	lo, ok := readInt()
	if !ok {
		return 0, 0, false
	}
	hi := lo
	if i < len(v.src) && v.src[i] == ',' {
		i++
		if h, ok := readInt(); ok {
			hi = h
		} else {
			hi = -1
		}
	}
	if i >= len(v.src) || v.src[i] != '}' {
		return 0, 0, false
	}
	if consume {
		v.pos = i + 1
	}
	return lo, hi, true
}

// quantifier :: QuantifierPrefix ?? (optional)
func (v *reValidator) quantifier() {
	switch v.peek() {
	case '*', '+', '?':
		v.pos++
	case '{':
		save := v.pos
		lo, hi, ok := v.braceQuantifier(true)
		if !ok {
			if v.u {
				v.fail()
			}
			v.pos = save
			return
		}
		if hi >= 0 && hi < lo {
			v.pos = save
			v.fail()
			return
		}
	default:
		return
	}
	if v.peek() == '?' {
		v.pos++
	}
}

// atom :: PatternCharacter | . | \ AtomEscape | CharacterClass | ( ... )
func (v *reValidator) atom() {
	c := v.peek()
	switch c {
	case '.':
		v.pos++
	case '(':
		v.pos++
		if v.peek() == '?' {
			switch v.peekAt(1) {
			case ':':
				v.pos += 2
			case '<':
				v.pos += 2
				name, ok := v.groupName()
				if !ok {
					v.fail()
					return
				}
				for _, g := range v.groupNames {
					if g == name {
						v.fail()
						return
					}
				}
				v.groupNames = append(v.groupNames, name)
			default:
				v.fail()
				return
			}
		}
		v.disjunction()
		if v.peek() != ')' {
			v.fail()
			return
		}
		v.pos++
	case '[':
		v.characterClass()
	case '\\':
		v.pos++
		v.atomEscape()
	case '*', '+', '?':
		v.fail()
	case '{':
		if v.u {
			v.fail()
			return
		}
		// Annex B: ExtendedPatternCharacter, unless it is a valid quantifier
		// with nothing to quantify.
		if _, _, ok := v.braceQuantifier(false); ok {
			v.fail()
			return
		}
		v.pos++
	case '}', ']':
		if v.u {
			v.fail()
			return
		}
		v.pos++
	case ')', '|':
		v.fail()
	default:
		_, n := decodeRune(v.src, v.pos)
		v.pos += n
	}
}

// groupName :: < RegExpIdentifierName >; v.pos is after '<'.
func (v *reValidator) groupName() (string, bool) {
	start := v.pos
	var b strings.Builder
	first := true
	for !v.eof() && v.peek() != '>' {
		var r rune
		if v.peek() == '\\' {
			v.pos++
			if v.peek() != 'u' {
				return "", false
			}
			v.pos++
			var ok bool
			r, ok = v.unicodeEscape(true)
			if !ok {
				return "", false
			}
		} else {
			var n int
			r, n = decodeRune(v.src, v.pos)
			v.pos += n
			// A surrogate pair written literally is one code point already.
		}
		if (first && !isIDStart(r)) || (!first && !isIDContinue(r)) {
			return "", false
		}
		b.WriteRune(r)
		first = false
	}
	if v.eof() || v.pos == start {
		return "", false
	}
	v.pos++ // '>'
	return b.String(), true
}

// unicodeEscape parses the part after `\u`: XXXX (with a trailing
// surrogate pair in UnicodeMode or names) or {X...} when braces allowed.
func (v *reValidator) unicodeEscape(braces bool) (rune, bool) {
	if v.peek() == '{' && braces {
		v.pos++
		var r rune
		n := 0
		for !v.eof() && isHexDigit(v.peek()) {
			r = r*16 + rune(hexVal(v.peek()))
			if r > 0x10FFFF {
				return 0, false
			}
			v.pos++
			n++
		}
		if n == 0 || v.peek() != '}' {
			return 0, false
		}
		v.pos++
		return r, true
	}
	r, ok := v.hex4()
	if !ok {
		return 0, false
	}
	if r >= 0xD800 && r <= 0xDBFF && braces && v.peek() == '\\' && v.peekAt(1) == 'u' {
		save := v.pos
		v.pos += 2
		if lo, ok := v.hex4(); ok && lo >= 0xDC00 && lo <= 0xDFFF {
			return 0x10000 + (r-0xD800)<<10 + (lo - 0xDC00), true
		}
		v.pos = save
	}
	return r, true
}

func (v *reValidator) hex4() (rune, bool) {
	if v.pos+4 > len(v.src) {
		return 0, false
	}
	var r rune
	for k := 0; k < 4; k++ {
		c := v.src[v.pos+k]
		if !isHexDigit(c) {
			return 0, false
		}
		r = r*16 + rune(hexVal(c))
	}
	v.pos += 4
	return r, true
}

// atomEscape :: DecimalEscape | CharacterClassEscape | CharacterEscape |
// k GroupName. v.pos is after the backslash.
func (v *reValidator) atomEscape() {
	if v.eof() {
		v.fail()
		return
	}
	c := v.peek()
	switch {
	case c >= '1' && c <= '9':
		n := 0
		for !v.eof() && isDecimalDigit(v.peek()) {
			if n < 1<<20 {
				n = n*10 + int(v.peek()-'0')
			}
			v.pos++
		}
		if n > v.maxBackref {
			v.maxBackref = n
		}
		return
	case c == 'k':
		v.pos++
		if v.named {
			if v.peek() != '<' {
				v.fail()
				return
			}
			v.pos++
			name, ok := v.groupName()
			if !ok {
				v.fail()
				return
			}
			v.backrefName = append(v.backrefName, name)
		}
		return
	}
	if v.characterClassEscape() {
		return
	}
	v.characterEscape(false)
}

// characterClassEscape :: d D s S w W | p{...} P{...} (UnicodeMode).
func (v *reValidator) characterClassEscape() bool {
	switch v.peek() {
	case 'd', 'D', 's', 'S', 'w', 'W':
		v.pos++
		return true
	case 'p', 'P':
		if !v.u {
			return false
		}
		neg := v.peek() == 'P'
		v.pos++
		if v.peek() != '{' {
			v.fail()
			return true
		}
		v.pos++
		end := strings.IndexByte(v.src[v.pos:], '}')
		if end < 0 {
			v.fail()
			return true
		}
		expr := v.src[v.pos : v.pos+end]
		if !validUnicodeProperty(expr, v.v && !neg) {
			v.fail()
			return true
		}
		v.pos += end + 1
		return true
	}
	return false
}

// characterEscape :: ControlEscape | c AsciiLetter | 0 | HexEscape |
// RegExpUnicodeEscape | IdentityEscape. inClass enables \b and \- forms.
func (v *reValidator) characterEscape(inClass bool) {
	c := v.peek()
	switch {
	case strings.IndexByte("fnrtv", c) >= 0:
		v.pos++
	case c == 'c':
		n := v.peekAt(1)
		if (n >= 'a' && n <= 'z') || (n >= 'A' && n <= 'Z') {
			v.pos += 2
			return
		}
		if !v.u && inClass && (isDecimalDigit(n) || n == '_') {
			v.pos += 2 // Annex B ClassControlLetter
			return
		}
		if v.u {
			v.fail()
			return
		}
		// Annex B: "\c" is matched literally; leave 'c' as a pattern char.
	case c == '0':
		v.pos++
		if v.u && isDecimalDigit(v.peek()) {
			v.fail()
			return
		}
		if !v.u {
			// Annex B LegacyOctalEscapeSequence.
			for k := 0; k < 2 && v.peek() >= '0' && v.peek() <= '7'; k++ {
				v.pos++
			}
		}
	case c == 'x':
		v.pos++
		if isHexDigit(v.peek()) && isHexDigit(v.peekAt(1)) {
			v.pos += 2
		} else if v.u {
			v.fail()
		}
	case c == 'u':
		v.pos++
		save := v.pos
		if _, ok := v.unicodeEscape(v.u); !ok {
			if v.u {
				v.fail()
				return
			}
			v.pos = save
		}
	case isDecimalDigit(c):
		// Only reachable in classes: \1-\9 there are legacy octal / identity.
		if v.u {
			v.fail()
			return
		}
		v.pos++
	default:
		r, n := decodeRune(v.src, v.pos)
		if v.u {
			// IdentityEscape[+UnicodeMode] :: SyntaxCharacter | /
			if strings.IndexByte("^$\\.*+?()[]{}|/", c) < 0 && !(inClass && c == '-') {
				v.fail()
				return
			}
		} else if c == 'k' && v.named {
			v.fail()
			return
		}
		_ = r
		v.pos += n
	}
}

// characterClass parses [ ... ] in both modes.
func (v *reValidator) characterClass() {
	v.pos++ // '['
	if v.peek() == '^' {
		v.pos++
	}
	if v.v {
		v.classSetExpression()
	} else {
		v.classRanges()
	}
	if v.err {
		return
	}
	if v.peek() != ']' {
		v.fail()
		return
	}
	v.pos++
}

// classAtom parses one ClassAtom and returns its code point, or -1 for a
// class escape such as \d.
func (v *reValidator) classAtom() rune {
	c := v.peek()
	if c != '\\' {
		r, n := decodeRune(v.src, v.pos)
		v.pos += n
		return r
	}
	v.pos++
	if v.eof() {
		v.fail()
		return 0
	}
	c = v.peek()
	switch {
	case c == 'b':
		v.pos++
		return '\b'
	case c == '-' && v.u:
		v.pos++
		return '-'
	case c == 'k' && !v.named:
		v.pos++
		return 'k'
	}
	if v.characterClassEscape() {
		return -1
	}
	start := v.pos
	v.characterEscape(true)
	return escapeValue(v.src[start:v.pos])
}

// escapeValue approximates the code point an escape denotes, for range
// order checks.
func escapeValue(s string) rune {
	if s == "" {
		return 0
	}
	switch s[0] {
	case 'f':
		return '\f'
	case 'n':
		return '\n'
	case 'r':
		return '\r'
	case 't':
		return '\t'
	case 'v':
		return '\v'
	case 'c':
		if len(s) == 2 {
			return rune(s[1]) % 32
		}
		return '\\'
	case '0', '1', '2', '3', '4', '5', '6', '7':
		var r rune
		for i := 0; i < len(s) && s[i] >= '0' && s[i] <= '7'; i++ {
			r = r*8 + rune(s[i]-'0')
		}
		return r
	case 'x':
		if len(s) == 3 {
			return rune(hexVal(s[1])*16 + hexVal(s[2]))
		}
		return 'x'
	case 'u':
		if len(s) == 1 {
			return 'u'
		}
		var r rune
		pair := false
		var lo rune
		body := s[1:]
		if body[0] == '{' {
			for i := 1; i < len(body) && body[i] != '}'; i++ {
				r = r*16 + rune(hexVal(body[i]))
			}
			return r
		}
		for i := 0; i < 4 && i < len(body); i++ {
			r = r*16 + rune(hexVal(body[i]))
		}
		if len(body) >= 10 {
			pair = true
			for i := 6; i < 10; i++ {
				lo = lo*16 + rune(hexVal(body[i]))
			}
		}
		if pair {
			return 0x10000 + (r-0xD800)<<10 + (lo - 0xDC00)
		}
		return r
	}
	r, _ := decodeRune(s, 0)
	return r
}

// classRanges :: NonemptyClassRanges? (non-v mode).
func (v *reValidator) classRanges() {
	for !v.err && !v.eof() && v.peek() != ']' {
		a := v.classAtom()
		if v.err {
			return
		}
		if v.peek() == '-' && v.peekAt(1) != ']' && v.pos+1 < len(v.src) {
			v.pos++
			b := v.classAtom()
			if v.err {
				return
			}
			if a == -1 || b == -1 {
				if v.u {
					v.fail()
				}
				continue
			}
			if a > b {
				v.fail()
				return
			}
		}
	}
}

// classSetExpression parses ClassSetExpression (UnicodeSetsMode):
// ClassUnion, or one operator kind (&& or --) between ClassSetOperands.
func (v *reValidator) classSetExpression() {
	if v.peek() == ']' {
		return
	}
	isRange := v.classSetOperand(true)
	if v.err {
		return
	}
	if o := v.peek(); (o == '&' || o == '-') && v.peekAt(1) == o {
		if isRange {
			v.fail()
			return
		}
		for !v.err && v.peek() == o && v.peekAt(1) == o {
			v.pos += 2
			if o == '&' && v.peek() == '&' {
				v.fail()
				return
			}
			v.classSetOperand(false)
		}
		if !v.err && v.peek() != ']' {
			v.fail()
		}
		return
	}
	for !v.err && !v.eof() && v.peek() != ']' {
		if o := v.peek(); (o == '&' || o == '-') && v.peekAt(1) == o {
			v.fail()
			return
		}
		v.classSetOperand(true)
	}
}

// classSetOperand parses a nested class, \q{...}, an escape or a
// ClassSetCharacter (or a ClassSetRange when allowRange). It reports
// whether a range was parsed.
func (v *reValidator) classSetOperand(allowRange bool) bool {
	c := v.peek()
	switch {
	case c == '[':
		v.characterClass()
		return false
	case c == '\\' && v.peekAt(1) == 'q' && v.peekAt(2) == '{':
		v.pos += 3
		for !v.err && v.peek() != '}' {
			if v.eof() {
				v.fail()
				return false
			}
			if v.peek() == '|' {
				v.pos++
				continue
			}
			v.classSetCharacter()
		}
		v.pos++
		return false
	case c == '\\':
		v.pos++
		if v.characterClassEscape() {
			return false
		}
		v.pos--
	}
	a := v.classSetCharacter()
	if v.err {
		return false
	}
	if allowRange && v.peek() == '-' && v.peekAt(1) != '-' {
		v.pos++
		b := v.classSetCharacter()
		if !v.err && a > b {
			v.fail()
		}
		return true
	}
	return false
}

// classSetCharacter parses ClassSetCharacter: no unescaped syntax
// characters or reserved double punctuators.
func (v *reValidator) classSetCharacter() rune {
	if v.eof() {
		v.fail()
		return 0
	}
	c := v.peek()
	if c == '\\' {
		v.pos++
		n := v.peek()
		if n == 'b' {
			v.pos++
			return '\b'
		}
		if strings.IndexByte("&-!#%,:;<=>@`~", n) >= 0 {
			v.pos++
			return rune(n)
		}
		start := v.pos
		v.characterEscape(true)
		return escapeValue(v.src[start:v.pos])
	}
	if strings.IndexByte("()[]{}/-|", c) >= 0 {
		v.fail()
		return 0
	}
	if strings.IndexByte("&!#$%*+,.:;<=>?@^`~", c) >= 0 && v.peekAt(1) == c {
		v.fail()
		return 0
	}
	r, n := decodeRune(v.src, v.pos)
	v.pos += n
	return r
}

// validUnicodeProperty checks UnicodePropertyValueExpression (22.2.2.9).
func validUnicodeProperty(expr string, allowStrings bool) bool {
	name, value, hasEq := strings.Cut(expr, "=")
	if !hasEq {
		if isGeneralCategory(expr) || binaryProperties[expr] {
			return true
		}
		return allowStrings && stringProperties[expr]
	}
	switch name {
	case "General_Category", "gc":
		return isGeneralCategory(value)
	case "Script", "sc", "Script_Extensions", "scx":
		if _, ok := unicode.Scripts[value]; ok {
			return true
		}
		return isScriptAlias(value)
	}
	return false
}

func isGeneralCategory(s string) bool {
	if _, ok := unicode.Categories[s]; ok {
		return true
	}
	return gcLongNames[s]
}

// isScriptAlias accepts four-letter ISO 15924 codes (Latn, Grek, ...);
// Go's tables carry long names only, so codes are checked by shape.
func isScriptAlias(s string) bool {
	if len(s) != 4 || s[0] < 'A' || s[0] > 'Z' {
		return false
	}
	for i := 1; i < 4; i++ {
		if s[i] < 'a' || s[i] > 'z' {
			return false
		}
	}
	return true
}

var gcLongNames = setOf("Cased_Letter", "Close_Punctuation", "Connector_Punctuation", "Control", "Currency_Symbol",
	"Dash_Punctuation", "Decimal_Number", "Enclosing_Mark", "Final_Punctuation", "Format", "Initial_Punctuation",
	"Letter", "Letter_Number", "Line_Separator", "Lowercase_Letter", "Mark", "Math_Symbol", "Modifier_Letter",
	"Modifier_Symbol", "Nonspacing_Mark", "Number", "Open_Punctuation", "Other", "Other_Letter", "Other_Number",
	"Other_Punctuation", "Other_Symbol", "Paragraph_Separator", "Private_Use", "Punctuation", "Separator",
	"Space_Separator", "Spacing_Mark", "Surrogate", "Symbol", "Titlecase_Letter", "Unassigned", "Uppercase_Letter",
	"Combining_Mark", "digit", "punct", "LC", "Cn")

// binaryProperties lists ECMA-262 table 67 (binary Unicode properties)
// with their aliases.
var binaryProperties = setOf("ASCII", "ASCII_Hex_Digit", "AHex", "Alphabetic", "Alpha", "Any", "Assigned",
	"Bidi_Control", "Bidi_C", "Bidi_Mirrored", "Bidi_M", "Case_Ignorable", "CI", "Cased",
	"Changes_When_Casefolded", "CWCF", "Changes_When_Casemapped", "CWCM", "Changes_When_Lowercased", "CWL",
	"Changes_When_NFKC_Casefolded", "CWKCF", "Changes_When_Titlecased", "CWT", "Changes_When_Uppercased", "CWU",
	"Dash", "Default_Ignorable_Code_Point", "DI", "Deprecated", "Dep", "Diacritic", "Dia", "Emoji",
	"Emoji_Component", "EComp", "Emoji_Modifier", "EMod", "Emoji_Modifier_Base", "EBase", "Emoji_Presentation",
	"EPres", "Extended_Pictographic", "ExtPict", "Extender", "Ext", "Grapheme_Base", "Gr_Base", "Grapheme_Extend",
	"Gr_Ext", "Hex_Digit", "Hex", "IDS_Binary_Operator", "IDSB", "IDS_Trinary_Operator", "IDST", "ID_Continue",
	"IDC", "ID_Start", "IDS", "Ideographic", "Ideo", "Join_Control", "Join_C", "Logical_Order_Exception", "LOE",
	"Lowercase", "Lower", "Math", "Noncharacter_Code_Point", "NChar", "Pattern_Syntax", "Pat_Syn",
	"Pattern_White_Space", "Pat_WS", "Quotation_Mark", "QMark", "Radical", "Regional_Indicator", "RI",
	"Sentence_Terminal", "STerm", "Soft_Dotted", "SD", "Terminal_Punctuation", "Term", "Unified_Ideograph",
	"UIdeo", "Uppercase", "Upper", "Variation_Selector", "VS", "White_Space", "space", "XID_Continue", "XIDC",
	"XID_Start", "XIDS")

// stringProperties are the properties of strings allowed with the v flag.
var stringProperties = setOf("Basic_Emoji", "Emoji_Keycap_Sequence", "RGI_Emoji_Modifier_Sequence",
	"RGI_Emoji_Flag_Sequence", "RGI_Emoji_Tag_Sequence", "RGI_Emoji_ZWJ_Sequence", "RGI_Emoji")

func setOf(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}
