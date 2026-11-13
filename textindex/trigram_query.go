package textindex

import (
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// TrigramOp is the operator of a TrigramQuery node.
type TrigramOp uint8

const (
	// TrigramAll matches every document (no information).
	TrigramAll TrigramOp = iota
	// TrigramNone matches no document.
	TrigramNone
	// TrigramAnd matches when every trigram and every sub-query matches.
	TrigramAnd
	// TrigramOr matches when any trigram or any sub-query matches.
	TrigramOr
)

// TrigramQuery is a boolean query over trigrams (Cox 2012). Queries built
// by this package are canonical: trigrams sorted and distinct, nested
// same-operator nodes flattened, sub-queries sorted by String and
// distinct, a single trigram is an And of one, and degenerate nodes are
// collapsed (see README "Regex queries").
type TrigramQuery struct {
	Op       TrigramOp
	Trigrams []uint32
	Sub      []*TrigramQuery
}

// allQ and noneQ are shared immutable leaves; canon never mutates its
// inputs, so sharing them is safe.
var (
	allQ  = &TrigramQuery{Op: TrigramAll}
	noneQ = &TrigramQuery{Op: TrigramNone}
)

// String renders the canonical text form: ALL, NONE, or
// "(and t... q...)" / "(or t... q...)" where each trigram t is a
// double-quoted string with bytes outside 0x20..0x7e, '"' and '\' written
// as \xNN (lower-case hex).
func (q *TrigramQuery) String() string {
	var b strings.Builder
	q.write(&b)
	return b.String()
}

func (q *TrigramQuery) write(b *strings.Builder) {
	switch q.Op {
	case TrigramAll:
		b.WriteString("ALL")
		return
	case TrigramNone:
		b.WriteString("NONE")
		return
	case TrigramAnd:
		b.WriteString("(and")
	default:
		b.WriteString("(or")
	}
	for _, t := range q.Trigrams {
		b.WriteByte(' ')
		writeTrigram(b, t)
	}
	for _, s := range q.Sub {
		b.WriteByte(' ')
		s.write(b)
	}
	b.WriteByte(')')
}

const hexDigits = "0123456789abcdef"

func writeTrigram(b *strings.Builder, t uint32) {
	b.WriteByte('"')
	for _, c := range [3]byte{byte(t >> 16), byte(t >> 8), byte(t)} {
		if c >= 0x20 && c <= 0x7e && c != '"' && c != '\\' {
			b.WriteByte(c)
		} else {
			b.WriteString(`\x`)
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&15])
		}
	}
	b.WriteByte('"')
}

// MatchTrigrams evaluates q against a sorted set of trigrams.
func (q *TrigramQuery) MatchTrigrams(set []uint32) bool {
	has := func(t uint32) bool { _, ok := slices.BinarySearch(set, t); return ok }
	switch q.Op {
	case TrigramAll:
		return true
	case TrigramNone:
		return false
	case TrigramAnd:
		for _, t := range q.Trigrams {
			if !has(t) {
				return false
			}
		}
		for _, s := range q.Sub {
			if !s.MatchTrigrams(set) {
				return false
			}
		}
		return true
	}
	for _, t := range q.Trigrams {
		if has(t) {
			return true
		}
	}
	for _, s := range q.Sub {
		if s.MatchTrigrams(set) {
			return true
		}
	}
	return false
}

// canon builds a canonical node of op from trigrams and subs (rules in the
// TrigramQuery doc). Sorting sub-queries by their text form makes the
// result independent of construction order, so equal queries print
// equally (the trigram conformance vectors compare text).
func canon(op TrigramOp, tris []uint32, subs []*TrigramQuery) *TrigramQuery {
	var outT []uint32
	var outS []*TrigramQuery
	outT = append(outT, tris...)
	for _, s := range subs {
		switch {
		case s.Op == op:
			outT = append(outT, s.Trigrams...)
			outS = append(outS, s.Sub...)
		case s.Op == TrigramAll:
			if op == TrigramOr {
				return allQ
			}
		case s.Op == TrigramNone:
			if op == TrigramAnd {
				return noneQ
			}
		case op == TrigramOr && s.Op == TrigramAnd && len(s.Trigrams) == 1 && len(s.Sub) == 0:
			outT = append(outT, s.Trigrams[0])
		default:
			outS = append(outS, s)
		}
	}
	outT = sortUnique32(outT)
	if len(outS) > 1 {
		slices.SortFunc(outS, func(a, b *TrigramQuery) int { return strings.Compare(a.String(), b.String()) })
		outS = slices.CompactFunc(outS, func(a, b *TrigramQuery) bool { return a.String() == b.String() })
	}
	switch {
	case len(outT) == 0 && len(outS) == 0:
		if op == TrigramAnd {
			return allQ
		}
		return noneQ
	case len(outT) == 0 && len(outS) == 1:
		return outS[0]
	case op == TrigramOr && len(outT) == 1 && len(outS) == 0:
		return &TrigramQuery{Op: TrigramAnd, Trigrams: outT}
	}
	return &TrigramQuery{Op: op, Trigrams: outT, Sub: outS}
}

func (q *TrigramQuery) and(r *TrigramQuery) *TrigramQuery {
	return canon(TrigramAnd, nil, []*TrigramQuery{q, r})
}

func (q *TrigramQuery) or(r *TrigramQuery) *TrigramQuery {
	return canon(TrigramOr, nil, []*TrigramQuery{q, r})
}

// andTrigrams ANDs q with "one of the strings of s occurs". Strings are
// turned into trigrams with ASCII folding, matching the index. A set whose
// shortest string is shorter than 3 bytes carries no trigram information.
func (q *TrigramQuery) andTrigrams(s stringSet) *TrigramQuery {
	if s.minLen() < 3 {
		return q
	}
	var alts []*TrigramQuery
	for _, str := range s {
		tris := appendTrigrams(nil, []byte(str))
		alts = append(alts, canon(TrigramAnd, tris, nil))
	}
	return q.and(canon(TrigramOr, nil, alts))
}

// Cox's set limits (codesearch index/regexp.go).
const (
	maxExact = 7
	maxSet   = 20
)

// stringSet is a set of byte strings; nil means "unknown" (codesearch's
// !have()).
type stringSet []string

func (s stringSet) have() bool { return s != nil }

func (s stringSet) minLen() int {
	if len(s) == 0 {
		return 0
	}
	m := len(s[0])
	for _, x := range s {
		m = min(m, len(x))
	}
	return m
}

func compareSuffix(a, b string) int {
	for i, j := len(a)-1, len(b)-1; i >= 0 && j >= 0; i, j = i-1, j-1 {
		if a[i] != b[j] {
			if a[i] < b[j] {
				return -1
			}
			return 1
		}
	}
	return len(a) - len(b)
}

func (s *stringSet) clean(isSuffix bool) {
	t := *s
	if isSuffix {
		slices.SortFunc(t, compareSuffix)
	} else {
		slices.Sort(t)
	}
	*s = slices.Compact(t)
}

func (s *stringSet) add(str string) { *s = append(*s, str) }

func (s stringSet) union(t stringSet, isSuffix bool) stringSet {
	u := make(stringSet, 0, len(s)+len(t))
	u = append(append(u, s...), t...)
	u.clean(isSuffix)
	return u
}

func (s stringSet) cross(t stringSet, isSuffix bool) stringSet {
	var p stringSet
	for _, a := range s {
		for _, b := range t {
			p.add(a + b)
		}
	}
	p.clean(isSuffix)
	return p
}

// regexpInfo is codesearch's per-subexpression summary.
type regexpInfo struct {
	canEmpty bool
	exact    stringSet
	prefix   stringSet
	suffix   stringSet
	match    *TrigramQuery
}

func anyMatch() regexpInfo {
	return regexpInfo{canEmpty: true, prefix: stringSet{""}, suffix: stringSet{""}, match: allQ}
}
func anyChar() regexpInfo {
	return regexpInfo{prefix: stringSet{""}, suffix: stringSet{""}, match: allQ}
}
func noMatch() regexpInfo     { return regexpInfo{match: noneQ} }
func emptyString() regexpInfo { return regexpInfo{canEmpty: true, exact: stringSet{""}, match: allQ} }

func (info *regexpInfo) addExact() {
	if info.exact.have() {
		info.match = info.match.andTrigrams(info.exact)
	}
}

func (info *regexpInfo) simplify(force bool) {
	info.exact.clean(false)
	if len(info.exact) > maxExact || (info.exact.minLen() >= 3 && force) || info.exact.minLen() >= 4 {
		info.addExact()
		for _, s := range info.exact {
			n := len(s)
			if n < 3 {
				info.prefix.add(s)
				info.suffix.add(s)
			} else {
				info.prefix.add(s[:2])
				info.suffix.add(s[n-2:])
			}
		}
		info.exact = nil
	}
	if !info.exact.have() {
		info.simplifySet(&info.prefix, false)
		info.simplifySet(&info.suffix, true)
	}
}

func (info *regexpInfo) simplifySet(s *stringSet, isSuffix bool) {
	t := *s
	t.clean(isSuffix)
	info.match = info.match.andTrigrams(t)
	for n := 3; n == 3 || t.size() > maxSet; n-- {
		w := 0
		for _, str := range t {
			if len(str) >= n {
				if !isSuffix {
					str = str[:n-1]
				} else {
					str = str[len(str)-n+1:]
				}
			}
			if w == 0 || t[w-1] != str {
				t[w] = str
				w++
			}
		}
		t = t[:w]
		t.clean(isSuffix)
	}
	w := 0
	f := strings.HasPrefix
	if isSuffix {
		f = strings.HasSuffix
	}
	for _, str := range t {
		if w == 0 || !f(str, t[w-1]) {
			t[w] = str
			w++
		}
	}
	*s = t[:w]
}

func (s stringSet) size() int { return len(s) }

func concat(x, y regexpInfo) regexpInfo {
	var xy regexpInfo
	xy.match = x.match.and(y.match)
	if x.exact.have() && y.exact.have() {
		xy.exact = x.exact.cross(y.exact, false)
	} else {
		if x.exact.have() {
			xy.prefix = x.exact.cross(y.prefix, false)
		} else {
			xy.prefix = x.prefix
			if x.canEmpty {
				xy.prefix = xy.prefix.union(y.prefix, false)
			}
		}
		if y.exact.have() {
			xy.suffix = x.suffix.cross(y.exact, true)
		} else {
			xy.suffix = y.suffix
			if y.canEmpty {
				xy.suffix = xy.suffix.union(x.suffix, true)
			}
		}
	}
	if !x.exact.have() && !y.exact.have() &&
		x.suffix.size() <= maxSet && y.prefix.size() <= maxSet &&
		x.suffix.minLen()+y.prefix.minLen() >= 3 {
		xy.match = xy.match.andTrigrams(x.suffix.cross(y.prefix, false))
	}
	xy.canEmpty = x.canEmpty && y.canEmpty
	xy.simplify(false)
	return xy
}

func alternate(x, y regexpInfo) regexpInfo {
	var xy regexpInfo
	switch {
	case x.exact.have() && y.exact.have():
		xy.exact = x.exact.union(y.exact, false)
	case x.exact.have():
		xy.prefix = x.exact.union(y.prefix, false)
		xy.suffix = x.exact.union(y.suffix, true)
		x.addExact()
	case y.exact.have():
		xy.prefix = x.prefix.union(y.exact, false)
		xy.suffix = x.suffix.union(y.exact, true)
		y.addExact()
	default:
		xy.prefix = x.prefix.union(y.prefix, false)
		xy.suffix = x.suffix.union(y.suffix, true)
	}
	xy.canEmpty = x.canEmpty || y.canEmpty
	xy.match = x.match.or(y.match)
	xy.simplify(false)
	return xy
}

func fold(f func(x, y regexpInfo) regexpInfo, sub []*syntax.Regexp, zero regexpInfo) regexpInfo {
	if len(sub) == 0 {
		return zero
	}
	if len(sub) == 1 {
		return analyzeRegexp(sub[0])
	}
	info := f(analyzeRegexp(sub[0]), analyzeRegexp(sub[1]))
	for i := 2; i < len(sub); i++ {
		info = f(info, analyzeRegexp(sub[i]))
	}
	return info
}

func literalInfo(rs []rune) regexpInfo {
	if !slices.Contains(rs, utf8.RuneError) {
		return regexpInfo{exact: stringSet{string(rs)}, match: allQ}
	}
	// U+FFFD in a pattern also matches invalid UTF-8 bytes in the text,
	// which have no trigram of its encoding: treat it as any character.
	info := emptyString()
	for _, r := range rs {
		if r == utf8.RuneError {
			info = concat(info, anyChar())
		} else {
			info = concat(info, regexpInfo{exact: stringSet{string(r)}, match: allQ})
		}
	}
	return info
}

// analyzeRegexp is codesearch's analyze over a simplified regexp/syntax
// tree, with the U+FFFD rule of literalInfo and of character classes.
//
// PORT: this walks Go's regexp/syntax tree after Simplify (counted
// repeats expanded, case-insensitive literals as FoldCase flags, Perl
// classes). Rust's regex-syntax HIR is shaped differently (case folding
// pre-expanded into classes, Unicode-aware classes by default). A port
// re-derives the same analysis on its HIR; it may produce an equivalent or
// weaker query but never one that drops a match (TIDX-INV-03). The
// trigram vectors pin the Go output.
func analyzeRegexp(re *syntax.Regexp) regexpInfo {
	var info regexpInfo
	switch re.Op {
	case syntax.OpNoMatch:
		return noMatch()
	case syntax.OpEmptyMatch, syntax.OpBeginLine, syntax.OpEndLine,
		syntax.OpBeginText, syntax.OpEndText, syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		return emptyString()
	case syntax.OpLiteral:
		if re.Flags&syntax.FoldCase != 0 {
			switch len(re.Rune) {
			case 0:
				return emptyString()
			case 1:
				r0 := re.Rune[0]
				cls := &syntax.Regexp{Op: syntax.OpCharClass, Rune: []rune{r0, r0}}
				for r1 := unicode.SimpleFold(r0); r1 != r0; r1 = unicode.SimpleFold(r1) {
					cls.Rune = append(cls.Rune, r1, r1)
				}
				return analyzeRegexp(cls)
			}
			info = emptyString()
			for _, r := range re.Rune {
				one := &syntax.Regexp{Op: syntax.OpLiteral, Flags: syntax.FoldCase, Rune: []rune{r}}
				info = concat(info, analyzeRegexp(one))
			}
			return info
		}
		info = literalInfo(re.Rune)
	case syntax.OpAnyCharNotNL, syntax.OpAnyChar:
		return anyChar()
	case syntax.OpCapture:
		return analyzeRegexp(re.Sub[0])
	case syntax.OpConcat:
		return fold(concat, re.Sub, emptyString())
	case syntax.OpAlternate:
		return fold(alternate, re.Sub, noMatch())
	case syntax.OpQuest:
		return alternate(analyzeRegexp(re.Sub[0]), emptyString())
	case syntax.OpStar:
		return anyMatch()
	case syntax.OpRepeat:
		// Simplify expands counted repeats; this keeps unsimplified trees safe.
		if re.Min == 0 {
			return anyMatch()
		}
		info = analyzeRegexp(re.Sub[0])
		if info.exact.have() {
			info.prefix = info.exact
			info.suffix = slices.Clone(info.exact)
			info.exact = nil
		}
	case syntax.OpPlus:
		info = analyzeRegexp(re.Sub[0])
		if info.exact.have() {
			info.prefix = info.exact
			info.suffix = slices.Clone(info.exact)
			info.exact = nil
		}
	case syntax.OpCharClass:
		info.match = allQ
		if len(re.Rune) == 0 {
			return noMatch()
		}
		n := 0
		for i := 0; i+1 < len(re.Rune); i += 2 {
			lo, hi := re.Rune[i], re.Rune[i+1]
			if lo <= utf8.RuneError && utf8.RuneError <= hi {
				return anyChar()
			}
			n += int(hi - lo)
		}
		if n > 100 {
			return anyChar()
		}
		info.exact = stringSet{}
		for i := 0; i+1 < len(re.Rune); i += 2 {
			for r := re.Rune[i]; r <= re.Rune[i+1]; r++ {
				info.exact.add(string(r))
			}
		}
	default:
		return anyMatch() // unknown op: assume the worst (never drops a match)
	}
	info.simplify(false)
	return info
}

// MaxRegexBytes bounds a regex pattern.
const MaxRegexBytes = 1024

// regexSource is the RE2 source compiled for pattern with the query
// language flags (a subset of "ims", already validated).
func regexSource(pattern, flags string) string {
	if flags == "" {
		return pattern
	}
	return "(?" + flags + ")" + pattern
}

// CompileRegex compiles a query-language regex (RE2 syntax, flags from
// "ims") into the verifying regexp and its trigram query.
func CompileRegex(pattern, flags string) (*regexp.Regexp, *TrigramQuery, error) {
	src := regexSource(pattern, flags)
	sre, err := syntax.Parse(src, syntax.Perl)
	if err != nil {
		return nil, nil, err
	}
	re, err := regexp.Compile(src)
	if err != nil {
		return nil, nil, err
	}
	return re, RegexpTrigramQuery(sre), nil
}

// RegexpTrigramQuery computes the trigram query of a parsed regexp (Cox
// 2012): every text the regexp matches contains a set of trigrams that
// satisfies the query.
func RegexpTrigramQuery(re *syntax.Regexp) *TrigramQuery {
	info := analyzeRegexp(re.Simplify())
	info.simplify(true)
	info.addExact()
	return info.match
}
