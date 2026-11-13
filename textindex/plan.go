package textindex

import (
	"math"
	"regexp"
)

// The plan is the executable form of a query, shared by the index executor
// and the brute-force reference (README "Query semantics"). Lowering
// resolves fields against the schema, analyzes term and phrase text,
// compiles regexes and their trigram queries, and resolves since/until
// against the caller's now.

type planKind uint8

const (
	planNone    planKind = iota // matches nothing
	planTerm                    // one term in any of fields
	planPhrase                  // terms at relative offsets in one of fields
	planRegex                   // regexp matches the text of one of fields
	planKeyword                 // keyword field == value
	planRange                   // lo <= numeric field < hi
	planAnd                     // all kids
	planOr                      // any kid
	planNot                     // not kid
)

type planNode struct {
	kind    planKind
	span    Span
	fields  []int    // text field ids (term, phrase, regex)
	terms   []string // term: 1; phrase: >= 2
	offsets []int    // phrase: position offset of terms[i] relative to terms[0]
	re      *regexp.Regexp
	tq      *TrigramQuery
	field   int    // keyword / numeric field id
	value   string // keyword value
	lo, hi  int64  // range [lo, hi)
	kids    []int32
}

type plan struct {
	nodes []planNode
	root  int32
	// unbounded lists regex nodes without usable trigrams.
	unbounded []int32
}

// bounded reports whether a regex node has trigram information.
func (n *planNode) bounded() bool { return n.tq.Op != TrigramAll }

type lowerer struct {
	s         Schema
	timeField int // numeric field id of since/until, -1 if none
	now       int64
	p         *plan
}

func (l *lowerer) add(n planNode) int32 {
	l.p.nodes = append(l.p.nodes, n)
	return int32(len(l.p.nodes) - 1)
}

// lowerQuery lowers q against schema s. timeField is the numeric field of
// since:/until: ("" disables them); now is Unix ms.
func lowerQuery(q *Query, s Schema, timeField string, now int64) (*plan, error) {
	l := &lowerer{s: s, timeField: -1, now: now, p: &plan{}}
	if timeField != "" {
		if t, i := s.lookup(timeField); t == fieldNumeric {
			l.timeField = i
		}
	}
	root, err := l.lower(q, q.Root)
	if err != nil {
		return nil, err
	}
	l.p.root = root
	return l.p, nil
}

func allTextFields(s Schema) []int {
	f := make([]int, len(s.Text))
	for i := range f {
		f[i] = i
	}
	return f
}

func (l *lowerer) lower(q *Query, id NodeID) (int32, error) {
	n := &q.Nodes[id]
	switch n.Kind {
	case NodeAnd, NodeOr, NodeNot:
		k := planAnd
		if n.Kind == NodeOr {
			k = planOr
		} else if n.Kind == NodeNot {
			k = planNot
		}
		var kids []int32
		for _, c := range q.Children(id) {
			kid, err := l.lower(q, c)
			if err != nil {
				return 0, err
			}
			kids = append(kids, kid)
		}
		return l.add(planNode{kind: k, span: n.Span, kids: kids}), nil
	case NodeSince, NodeUntil:
		if l.timeField < 0 {
			return 0, qerr(CodeFieldType, n.Span.Start, n.Span.End, "%s: the index has no time field", n.Kind)
		}
		tv, ok := parseTimeValue(n.Value)
		if !ok {
			return 0, qerr(CodeBadTime, n.Span.Start, n.Span.End, "bad time %q", n.Value)
		}
		t := tv.ms
		if tv.relative {
			t = satSub(l.now, tv.ms)
		}
		lo, hi := t, int64(math.MaxInt64)
		if n.Kind == NodeUntil {
			lo, hi = math.MinInt64, t
		}
		return l.add(planNode{kind: planRange, span: n.Span, field: l.timeField, lo: lo, hi: hi}), nil
	}
	// Leaves with an optional field.
	fields := allTextFields(l.s)
	if n.Field != "" {
		typ, idx := l.s.lookup(n.Field)
		switch typ {
		case fieldNone:
			return 0, qerr(CodeUnknownField, n.Span.Start, n.Span.Start+len(n.Field), "unknown field %q", n.Field)
		case fieldKeyword:
			if n.Kind == NodeRegex {
				return 0, qerr(CodeFieldType, n.Span.Start, n.Span.End, "regex on keyword field %q is not supported", n.Field)
			}
			return l.add(planNode{kind: planKeyword, span: n.Span, field: idx, value: n.Value}), nil
		case fieldNumeric, fieldStored:
			return 0, qerr(CodeFieldType, n.Span.Start, n.Span.End, "field %q is not searchable this way", n.Field)
		}
		fields = []int{idx}
	}
	if n.Kind == NodeRegex {
		re, tq, err := CompileRegex(n.Value, n.Flags)
		if err != nil {
			return 0, qerr(CodeBadRegex, n.Span.Start, n.Span.End, "%v", err)
		}
		if tq.Op == TrigramNone {
			return l.add(planNode{kind: planNone, span: n.Span}), nil
		}
		id := l.add(planNode{kind: planRegex, span: n.Span, fields: fields, re: re, tq: tq})
		if tq.Op == TrigramAll {
			l.p.unbounded = append(l.p.unbounded, id)
		}
		return id, nil
	}
	return l.lowerText(n.Span, fields, n.Value), nil
}

func satSub(a, b int64) int64 {
	if b > 0 && a < math.MinInt64+b {
		return math.MinInt64
	}
	return a - b
}

// lowerText lowers term or phrase text (README "Query semantics"):
//   - no tokens: matches nothing;
//   - one word: term(compound) OR seq(parts), where seq is a term for one
//     part and a phrase for several (just the term when the word is a
//     single part);
//   - several words: seq of all parts across the words.
func (l *lowerer) lowerText(sp Span, fields []int, text string) int32 {
	toks := Analyze([]byte(text))
	if len(toks) == 0 {
		return l.add(planNode{kind: planNone, span: sp})
	}
	words := wordsIn(text)
	compound := ""
	single := false
	var parts []string
	var offs []int
	base := -1
	for _, t := range toks {
		if t.Kind == TokenCompound || t.Kind == TokenWord {
			compound = t.Term
			single = t.Kind == TokenWord
		}
		if t.Kind == TokenCompound {
			continue
		}
		if base < 0 {
			base = t.Pos
		}
		parts = append(parts, t.Term)
		offs = append(offs, t.Pos-base)
	}
	seq := func() int32 {
		switch len(parts) {
		case 0:
			return l.add(planNode{kind: planNone, span: sp})
		case 1:
			return l.add(planNode{kind: planTerm, span: sp, fields: fields, terms: parts})
		}
		return l.add(planNode{kind: planPhrase, span: sp, fields: fields, terms: parts, offsets: offs})
	}
	if words != 1 || compound == "" || single {
		return seq()
	}
	t := l.add(planNode{kind: planTerm, span: sp, fields: fields, terms: []string{compound}})
	s := seq()
	return l.add(planNode{kind: planOr, span: sp, kids: []int32{t, s}})
}

// wordsIn counts the words of text that have at least one part (the
// analyzer's word segmentation without emission limits).
func wordsIn(text string) int {
	n := 0
	inWord, hasPart := false, false
	for _, r := range text {
		c := classOf(r)
		switch {
		case c == classOther:
			if inWord && hasPart {
				n++
			}
			inWord, hasPart = false, false
		default:
			inWord = true
			if c != classConnector {
				hasPart = true
			}
		}
	}
	if inWord && hasPart {
		n++
	}
	return n
}
