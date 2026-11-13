package textindex

import (
	"regexp/syntax"
	"strconv"
	"strings"
	"unicode/utf8"
)

// NodeID indexes Query.Nodes; -1 means none.
type NodeID int32

// NodeKind is the kind of a query AST node.
type NodeKind uint8

// AST node kinds. JSON names in parentheses.
const (
	NodeAnd    NodeKind = iota // ("and") two or more children, all must match
	NodeOr                     // ("or") two or more children, any must match
	NodeNot                    // ("not") one child, must not match
	NodeTerm                   // ("term") bare word or field:word
	NodePhrase                 // ("phrase") "..." or field:"..."
	NodeRegex                  // ("regex") /pattern/flags or field:/pattern/flags
	NodeSince                  // ("since") since:<time>: time field >= t
	NodeUntil                  // ("until") until:<time>: time field < t
)

var nodeKindNames = [...]string{"and", "or", "not", "term", "phrase", "regex", "since", "until"}

// String returns the JSON name of k ("and", "term", ...).
func (k NodeKind) String() string {
	if int(k) < len(nodeKindNames) {
		return nodeKindNames[k]
	}
	return "unknown"
}

// Node is one AST node in the Query arena. Children are a linked list
// (Child = first child, Next = next sibling).
type Node struct {
	Kind  NodeKind
	Span  Span
	Field string // Term/Phrase/Regex: "" = all text fields
	Value string // term text, unescaped phrase text, regex pattern, time value
	Flags string // regex flags, canonical order "ims"
	Child NodeID
	Next  NodeID
}

// Query is a parsed query: an arena of nodes in pre-order (Root = 0) and
// the source text the spans refer to.
type Query struct {
	Text  string
	Nodes []Node
	Root  NodeID
}

// Children returns the child ids of n in order.
func (q *Query) Children(n NodeID) []NodeID {
	var out []NodeID
	for c := q.Nodes[n].Child; c >= 0; c = q.Nodes[c].Next {
		out = append(out, c)
	}
	return out
}

// astTmp is the parser's intermediate tree. Flattening nested AND/OR
// discards inner nodes, so the tree is compacted into the arena at the
// end: the arena then holds only reachable nodes, in pre-order.
type astTmp struct {
	kind  NodeKind
	span  Span
	field string
	value string
	flags string
	kids  []*astTmp
}

type parser struct {
	src    string
	toks   []qtoken
	p      int
	depth  int
	leaves int
}

// Parse parses a query (README "Query language"). Errors are *QueryError
// with a stable code and a span inside q.
func Parse(q string) (*Query, error) {
	if len(q) > MaxQueryBytes {
		return nil, qerr(CodeQueryTooLong, MaxQueryBytes, len(q), "query longer than %d bytes", MaxQueryBytes)
	}
	if !utf8.ValidString(q) {
		i := 0
		for i < len(q) {
			r, w := utf8.DecodeRuneInString(q[i:])
			if r == utf8.RuneError && w == 1 {
				break
			}
			i += w
		}
		return nil, qerr(CodeInvalidUTF8, i, i+1, "query is not valid UTF-8")
	}
	toks, lerr := lexQuery(q)
	if lerr != nil {
		return nil, lerr
	}
	ps := &parser{src: q, toks: toks}
	if toks[0].kind == tokEOF {
		return nil, qerr(CodeEmptyQuery, 0, len(q), "query has no clause")
	}
	root, err := ps.parseOr()
	if err != nil {
		return nil, err
	}
	if t := ps.tok(); t.kind != tokEOF {
		if t.kind == tokRParen {
			return nil, qerr(CodeUnexpectedToken, t.start, t.end, "unmatched ')'")
		}
		return nil, qerr(CodeUnexpectedToken, t.start, t.end, "unexpected %s", t.kind)
	}
	out := &Query{Text: q}
	compactAST(out, root)
	return out, nil
}

func compactAST(q *Query, t *astTmp) NodeID {
	id := NodeID(len(q.Nodes))
	q.Nodes = append(q.Nodes, Node{Kind: t.kind, Span: t.span, Field: t.field, Value: t.value, Flags: t.flags, Child: -1, Next: -1})
	prev := NodeID(-1)
	for _, k := range t.kids {
		c := compactAST(q, k)
		if prev < 0 {
			q.Nodes[id].Child = c
		} else {
			q.Nodes[prev].Next = c
		}
		prev = c
	}
	return id
}

func (ps *parser) tok() qtoken { return ps.toks[ps.p] }

func canStart(k tokKind) bool {
	switch k {
	case tokLParen, tokMinus, tokWord, tokPhrase, tokRegex, tokField:
		return true
	}
	return false
}

func (ps *parser) expectOperand() *QueryError {
	t := ps.tok()
	if canStart(t.kind) {
		return nil
	}
	return qerr(CodeExpectedTerm, t.start, t.end, "expected a clause, found %s", t.kind)
}

// join builds an n-ary node of kind k, flattening children of the same kind.
func join(k NodeKind, items []*astTmp) *astTmp {
	if len(items) == 1 {
		return items[0]
	}
	n := &astTmp{kind: k, span: Span{items[0].span.Start, items[len(items)-1].span.End}}
	for _, it := range items {
		if it.kind == k {
			n.kids = append(n.kids, it.kids...)
		} else {
			n.kids = append(n.kids, it)
		}
	}
	return n
}

// or_expr = and_expr { "OR" and_expr }
func (ps *parser) parseOr() (*astTmp, *QueryError) {
	first, err := ps.parseAnd()
	if err != nil {
		return nil, err
	}
	items := []*astTmp{first}
	for ps.tok().kind == tokOr {
		ps.p++
		if err := ps.expectOperand(); err != nil {
			return nil, err
		}
		n, err := ps.parseAnd()
		if err != nil {
			return nil, err
		}
		items = append(items, n)
	}
	return join(NodeOr, items), nil
}

// and_expr = unary { [ "AND" ] unary }
func (ps *parser) parseAnd() (*astTmp, *QueryError) {
	first, err := ps.parseUnary()
	if err != nil {
		return nil, err
	}
	items := []*astTmp{first}
	for {
		t := ps.tok()
		if t.kind == tokAnd {
			ps.p++
			if err := ps.expectOperand(); err != nil {
				return nil, err
			}
		} else if !canStart(t.kind) {
			break
		}
		n, err := ps.parseUnary()
		if err != nil {
			return nil, err
		}
		items = append(items, n)
	}
	return join(NodeAnd, items), nil
}

// unary = [ "-" ] primary   (the "-" is adjacent to its operand)
func (ps *parser) parseUnary() (*astTmp, *QueryError) {
	t := ps.tok()
	if t.kind != tokMinus {
		return ps.parsePrimary()
	}
	ps.p++
	next := ps.tok()
	// The span of this EXPECTED_TERM is the '-' itself, not the token
	// found as for AND, OR and '(' (spec 40.3): with "- x" the token found
	// is a valid clause, only its distance is wrong.
	if !canStart(next.kind) || next.start != t.end {
		return nil, qerr(CodeExpectedTerm, t.start, t.end, "'-' must be followed directly by a clause")
	}
	if next.kind == tokMinus {
		return nil, qerr(CodeUnexpectedToken, next.start, next.end, "double negation; write -(-x)")
	}
	if ps.depth++; ps.depth > MaxQueryDepth {
		return nil, qerr(CodeTooDeep, t.start, t.end, "more than %d nested groups or negations", MaxQueryDepth)
	}
	child, err := ps.parsePrimary()
	if err != nil {
		return nil, err
	}
	ps.depth--
	return &astTmp{kind: NodeNot, span: Span{t.start, child.span.End}, kids: []*astTmp{child}}, nil
}

// primary = "(" or_expr ")" | field ":" value | term | phrase | regex
func (ps *parser) parsePrimary() (*astTmp, *QueryError) {
	t := ps.tok()
	switch t.kind {
	case tokLParen:
		if ps.depth++; ps.depth > MaxQueryDepth {
			return nil, qerr(CodeTooDeep, t.start, t.end, "more than %d nested groups or negations", MaxQueryDepth)
		}
		ps.p++
		if ps.tok().kind == tokEOF {
			return nil, qerr(CodeUnbalancedParen, t.start, len(ps.src), "'(' is not closed")
		}
		if err := ps.expectOperand(); err != nil {
			return nil, err
		}
		n, err := ps.parseOr()
		if err != nil {
			return nil, err
		}
		c := ps.tok()
		switch c.kind {
		case tokRParen:
			ps.p++
		case tokEOF:
			return nil, qerr(CodeUnbalancedParen, t.start, len(ps.src), "'(' is not closed")
		default:
			return nil, qerr(CodeUnexpectedToken, c.start, c.end, "unexpected %s", c.kind)
		}
		ps.depth--
		n.span = Span{t.start, c.end}
		return n, nil
	case tokField:
		ps.p++
		v := ps.tok() // the lexer guarantees an adjacent value token
		ps.p++
		return ps.leaf(v, t.text, t.start)
	case tokWord, tokPhrase, tokRegex:
		ps.p++
		return ps.leaf(t, "", t.start)
	}
	return nil, qerr(CodeExpectedTerm, t.start, t.end, "expected a clause, found %s", t.kind)
}

func (ps *parser) leaf(v qtoken, field string, start int) (*astTmp, *QueryError) {
	if ps.leaves++; ps.leaves > MaxQueryClauses {
		return nil, qerr(CodeTooManyClauses, start, v.end, "more than %d clauses", MaxQueryClauses)
	}
	sp := Span{start, v.end}
	if field == "since" || field == "until" {
		if v.kind == tokRegex {
			return nil, qerr(CodeBadTime, v.start, v.end, "%s: needs a duration or date, not a regex", field)
		}
		if _, ok := parseTimeValue(v.text); !ok {
			return nil, qerr(CodeBadTime, v.start, v.end, "%s: %q is not a duration (e.g. 7d) or date (YYYY-MM-DD)", field, v.text)
		}
		k := NodeSince
		if field == "until" {
			k = NodeUntil
		}
		return &astTmp{kind: k, span: sp, value: v.text}, nil
	}
	switch v.kind {
	case tokWord:
		return &astTmp{kind: NodeTerm, span: sp, field: field, value: v.text}, nil
	case tokPhrase:
		return &astTmp{kind: NodePhrase, span: sp, field: field, value: v.text}, nil
	case tokRegex:
		if len(v.text) > MaxRegexBytes {
			return nil, qerr(CodeBadRegex, v.start, v.end, "regex longer than %d bytes", MaxRegexBytes)
		}
		if _, err := syntax.Parse(regexSource(v.text, v.flags), syntax.Perl); err != nil {
			return nil, qerr(CodeBadRegex, v.start, v.end, "%v", err)
		}
		return &astTmp{kind: NodeRegex, span: sp, field: field, value: v.text, flags: v.flags}, nil
	}
	return nil, qerr(CodeExpectedValue, v.start, v.end, "expected a value")
}

// timeValue is a parsed since:/until: value.
type timeValue struct {
	relative bool
	ms       int64 // duration (relative) or Unix ms (absolute)
}

// parseTimeValue accepts <n><unit> with 1..9 digits and unit s, m, h, d
// or w; YYYY-MM-DD (midnight UTC); or YYYY-MM-DDTHH:MM:SS[.fff]Z.
func parseTimeValue(s string) (timeValue, bool) {
	if len(s) >= 2 && s[0] >= '0' && s[0] <= '9' {
		if unit, ok := unitMs(s[len(s)-1]); ok && len(s)-1 <= 9 && allDigits(s[:len(s)-1]) {
			n, _ := strconv.ParseInt(s[:len(s)-1], 10, 64)
			return timeValue{relative: true, ms: n * unit}, true
		}
	}
	if len(s) < 10 || s[4] != '-' || s[7] != '-' || !allDigits(s[0:4]) || !allDigits(s[5:7]) || !allDigits(s[8:10]) {
		return timeValue{}, false
	}
	y, _ := strconv.Atoi(s[0:4])
	mo, _ := strconv.Atoi(s[5:7])
	d, _ := strconv.Atoi(s[8:10])
	if mo < 1 || mo > 12 || d < 1 || d > daysIn(y, mo) {
		return timeValue{}, false
	}
	var hh, mm, ss, ms int
	switch {
	case len(s) == 10:
	case len(s) >= 20 && s[10] == 'T' && s[13] == ':' && s[16] == ':' && s[len(s)-1] == 'Z' &&
		allDigits(s[11:13]) && allDigits(s[14:16]) && allDigits(s[17:19]):
		hh, _ = strconv.Atoi(s[11:13])
		mm, _ = strconv.Atoi(s[14:16])
		ss, _ = strconv.Atoi(s[17:19])
		frac := s[19 : len(s)-1]
		switch {
		case frac == "":
		case len(frac) >= 2 && len(frac) <= 4 && frac[0] == '.' && allDigits(frac[1:]):
			digits := frac[1:]
			for len(digits) < 3 {
				digits += "0"
			}
			ms, _ = strconv.Atoi(digits)
		default:
			return timeValue{}, false
		}
		if hh > 23 || mm > 59 || ss > 59 {
			return timeValue{}, false
		}
	default:
		return timeValue{}, false
	}
	days := daysFromCivil(y, mo, d)
	return timeValue{ms: ((days*24+int64(hh))*60+int64(mm))*60000 + int64(ss)*1000 + int64(ms)}, true
}

func unitMs(c byte) (int64, bool) {
	switch c {
	case 's':
		return 1000, true
	case 'm':
		return 60_000, true
	case 'h':
		return 3_600_000, true
	case 'd':
		return 86_400_000, true
	case 'w':
		return 604_800_000, true
	}
	return 0, false
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func daysIn(y, m int) int {
	switch m {
	case 2:
		if y%4 == 0 && (y%100 != 0 || y%400 == 0) {
			return 29
		}
		return 28
	case 4, 6, 9, 11:
		return 30
	}
	return 31
}

// daysFromCivil is Howard Hinnant's days_from_civil (proleptic Gregorian).
func daysFromCivil(y, m, d int) int64 {
	if m <= 2 {
		y--
	}
	era := y / 400
	if y < 0 && y%400 != 0 {
		era = (y - 399) / 400
	}
	yoe := y - era*400
	mp := (m + 9) % 12
	doy := (153*mp+2)/5 + d - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return int64(era)*146097 + int64(doe) - 719468
}

// String prints the canonical form of q (README "Canonical printing");
// Parse(q.String()) yields the same AST up to spans.
func (q *Query) String() string {
	var b strings.Builder
	q.print(&b, q.Root)
	return b.String()
}

func (q *Query) print(b *strings.Builder, id NodeID) {
	n := &q.Nodes[id]
	field := func() {
		if n.Field != "" {
			b.WriteString(n.Field)
			b.WriteByte(':')
		}
	}
	switch n.Kind {
	case NodeTerm:
		field()
		b.WriteString(n.Value)
	case NodePhrase:
		field()
		b.WriteByte('"')
		for i := 0; i < len(n.Value); i++ {
			if c := n.Value[i]; c == '"' || c == '\\' {
				b.WriteByte('\\')
			}
			b.WriteByte(n.Value[i])
		}
		b.WriteByte('"')
	case NodeRegex:
		field()
		b.WriteByte('/')
		v := n.Value
		for i := 0; i < len(v); i++ {
			switch {
			case v[i] == '\\' && i+1 < len(v):
				b.WriteByte('\\')
				b.WriteByte(v[i+1])
				i++
			case v[i] == '/':
				b.WriteString(`\/`)
			default:
				b.WriteByte(v[i])
			}
		}
		b.WriteByte('/')
		b.WriteString(n.Flags)
	case NodeSince:
		b.WriteString("since:")
		b.WriteString(n.Value)
	case NodeUntil:
		b.WriteString("until:")
		b.WriteString(n.Value)
	case NodeNot:
		b.WriteByte('-')
		c := n.Child
		if k := q.Nodes[c].Kind; k == NodeAnd || k == NodeOr || k == NodeNot {
			b.WriteByte('(')
			q.print(b, c)
			b.WriteByte(')')
		} else {
			q.print(b, c)
		}
	case NodeAnd, NodeOr:
		sep := " "
		if n.Kind == NodeOr {
			sep = " OR "
		}
		for i, c := range q.Children(id) {
			if i > 0 {
				b.WriteString(sep)
			}
			if n.Kind == NodeAnd && q.Nodes[c].Kind == NodeOr {
				b.WriteByte('(')
				q.print(b, c)
				b.WriteByte(')')
			} else {
				q.print(b, c)
			}
		}
	}
}

// JSON renders the AST as canonical JSON (the conformance form): objects
// with keys in the order kind, span, field, value, flags, children; empty
// field, value and flags omitted; spans as [start, end].
func (q *Query) JSON(withSpans bool) string {
	var b strings.Builder
	q.json(&b, q.Root, withSpans)
	return b.String()
}

func (q *Query) json(b *strings.Builder, id NodeID, spans bool) {
	n := &q.Nodes[id]
	b.WriteString(`{"kind":`)
	writeJSONString(b, n.Kind.String())
	if spans {
		b.WriteString(`,"span":[`)
		b.WriteString(strconv.Itoa(n.Span.Start))
		b.WriteByte(',')
		b.WriteString(strconv.Itoa(n.Span.End))
		b.WriteByte(']')
	}
	for _, kv := range [...][2]string{{"field", n.Field}, {"value", n.Value}, {"flags", n.Flags}} {
		if kv[1] != "" {
			b.WriteString(`,"` + kv[0] + `":`)
			writeJSONString(b, kv[1])
		}
	}
	if n.Child >= 0 {
		b.WriteString(`,"children":[`)
		for i, c := range q.Children(id) {
			if i > 0 {
				b.WriteByte(',')
			}
			q.json(b, c, spans)
		}
		b.WriteByte(']')
	}
	b.WriteByte('}')
}

func writeJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&15])
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
}
