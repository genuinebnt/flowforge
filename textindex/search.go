package textindex

import (
	"slices"
	"sync"
)

// SearchOptions are per-query parameters.
type SearchOptions struct {
	// K is the number of hits to return (default 10).
	K int
	// Now is the Unix ms that relative since:/until: values are resolved
	// against. The index never reads the clock.
	Now int64
}

// Hit is one ranked result.
type Hit struct {
	ID    uint64
	Score float64
	// At is the document's time field (Unix ms) when HasAt.
	At    int64
	HasAt bool
}

// Result is a ranked result page.
type Result struct {
	Hits  []Hit
	Total int // number of matching live documents
}

// betterHit is the result order: score descending, then time field
// descending (documents without it last), then id ascending.
func betterHit(a, b Hit) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	if a.HasAt != b.HasAt {
		return a.HasAt
	}
	if a.At != b.At {
		return a.At > b.At
	}
	return a.ID < b.ID
}

// topK is a bounded min-heap whose root is the worst kept hit.
type topK struct {
	k int
	h []Hit
}

func (t *topK) push(x Hit) {
	if len(t.h) < t.k {
		t.h = append(t.h, x)
		for i := len(t.h) - 1; i > 0; {
			p := (i - 1) / 2
			if !betterHit(t.h[p], t.h[i]) {
				break
			}
			t.h[p], t.h[i] = t.h[i], t.h[p]
			i = p
		}
		return
	}
	if len(t.h) == 0 || !betterHit(x, t.h[0]) {
		return
	}
	t.h[0] = x
	for i := 0; ; {
		l, r, w := 2*i+1, 2*i+2, i
		if l < len(t.h) && betterHit(t.h[w], t.h[l]) {
			w = l
		}
		if r < len(t.h) && betterHit(t.h[w], t.h[r]) {
			w = r
		}
		if w == i {
			break
		}
		t.h[i], t.h[w] = t.h[w], t.h[i]
		i = w
	}
}

func (t *topK) sorted() []Hit {
	out := slices.Clone(t.h)
	slices.SortFunc(out, func(a, b Hit) int {
		if betterHit(a, b) {
			return -1
		}
		if betterHit(b, a) {
			return 1
		}
		return 0
	})
	return out
}

// segView is a segment with the tombstones of one snapshot.
type segView struct {
	name       string
	seg        *Segment
	del        bitmap // nil when nothing is deleted
	delCount   int
	delGen     uint64
	liveSumLen []uint64

	mu      sync.Mutex
	dfCache map[string]int
}

func newSegView(name string, seg *Segment, del bitmap, delGen uint64) *segView {
	v := &segView{name: name, seg: seg, del: del, delGen: delGen}
	v.liveSumLen = slices.Clone(seg.sumLen)
	if del != nil {
		v.delCount = popcount(del)
		for d := 0; d < seg.n; d++ {
			if del.get(uint32(d)) {
				for f := range v.liveSumLen {
					v.liveSumLen[f] -= uint64(seg.norms[f][d])
				}
			}
		}
		if v.delCount == 0 {
			v.del = nil
		}
	}
	return v
}

func (v *segView) live() int             { return v.seg.n - v.delCount }
func (v *segView) deleted(d uint32) bool { return v.del != nil && v.del.get(d) }

// liveDF is the number of live docs of the segment containing (field, term).
func (v *segView) liveDF(field int, term string, scratch []byte) int {
	if v.del == nil {
		df, _, _, ok := v.seg.lookupTerm(field, term, scratch)
		if !ok {
			return 0
		}
		return df
	}
	key := string(append(append(scratch[:0], byte(field)), term...))
	v.mu.Lock()
	if df, ok := v.dfCache[key]; ok {
		v.mu.Unlock()
		return df
	}
	v.mu.Unlock()
	n := 0
	if it := v.seg.termIter(field, term, scratch); it != nil {
		for d := it.nextDoc(); d != noMore; d = it.nextDoc() {
			if !v.deleted(d) {
				n++
			}
		}
	}
	v.mu.Lock()
	if v.dfCache == nil {
		v.dfCache = map[string]int{}
	}
	v.dfCache[key] = n
	v.mu.Unlock()
	return n
}

// stats are the index-wide BM25 statistics of one query over live docs.
type stats struct {
	n     int
	avgdl []float64
	df    map[string]int
}

func dfKey(field int, term string) string { return string(rune(field)) + "\x00" + term }

func collectStats(views []*segView, p *plan, nt int) *stats {
	st := &stats{df: map[string]int{}, avgdl: make([]float64, nt)}
	sum := make([]uint64, nt)
	for _, v := range views {
		st.n += v.live()
		for f := range sum {
			sum[f] += v.liveSumLen[f]
		}
	}
	for f := range sum {
		if st.n > 0 {
			st.avgdl[f] = float64(sum[f]) / float64(st.n)
		}
	}
	var scratch []byte
	for i := range p.nodes {
		n := &p.nodes[i]
		if n.kind != planTerm && n.kind != planPhrase {
			continue
		}
		for _, f := range n.fields {
			for _, t := range n.terms {
				k := dfKey(f, t)
				if _, ok := st.df[k]; ok {
					continue
				}
				df := 0
				for _, v := range views {
					df += v.liveDF(f, t, scratch)
				}
				st.df[k] = df
			}
		}
	}
	return st
}

// topFilters returns the root's positive keyword and range clauses.
func topFilters(p *plan) []int32 {
	r := &p.nodes[p.root]
	if r.kind == planKeyword || r.kind == planRange {
		return []int32{p.root}
	}
	if r.kind != planAnd {
		return nil
	}
	var out []int32
	for _, k := range r.kids {
		if kk := p.nodes[k].kind; kk == planKeyword || kk == planRange {
			out = append(out, k)
		}
	}
	return out
}

// checkUnbounded enforces the rule for regexes without trigrams (README
// "Regex queries"): the root's filter clauses must cap the candidates to
// at most limit live documents.
func checkUnbounded(views []*segView, p *plan, limit int) error {
	if len(p.unbounded) == 0 {
		return nil
	}
	sp := p.nodes[p.unbounded[0]].span
	filters := topFilters(p)
	if len(filters) == 0 {
		return qerr(CodeRegexNoTrigrams, sp.Start, sp.End, "regex has no usable trigrams; add filters (keyword, since/until) that select at most %d documents", limit)
	}
	count := 0
	for _, v := range views {
		var its []docIter
		for _, f := range filters {
			its = append(its, newSliceIter(filterDocs(v.seg, &p.nodes[f])))
		}
		it := newAndIter(its)
		for d := it.nextDoc(); d != noMore; d = it.nextDoc() {
			if !v.deleted(d) {
				count++
			}
		}
	}
	if count > limit {
		return qerr(CodeRegexNoTrigrams, sp.Start, sp.End, "regex has no usable trigrams and the filters select %d documents (limit %d)", count, limit)
	}
	return nil
}

// filterDocs scans the doc-values column of a keyword or range clause.
func filterDocs(sg *Segment, n *planNode) []uint32 {
	var out []uint32
	switch n.kind {
	case planKeyword:
		vals := sg.kwVals[n.field]
		i, ok := slices.BinarySearch(vals, n.value)
		if !ok {
			return nil
		}
		ord := uint32(i) + 1
		for d, o := range sg.kwOrds[n.field] {
			if o == ord {
				out = append(out, uint32(d))
			}
		}
	case planRange:
		has, vals := sg.numHas[n.field], sg.numVals[n.field]
		for d, x := range vals {
			if has.get(uint32(d)) && x >= n.lo && x < n.hi {
				out = append(out, uint32(d))
			}
		}
	}
	return out
}

// nodeState is the per-segment execution state of one plan node.
type nodeState struct {
	curs  []*postIter // term: one per field; phrase: field-major, one per term
	idf   []float64   // term: per field; phrase: per field (sum over terms)
	tri   docIter     // bounded regex: trigram candidates
	ord   uint32      // keyword: ordinal+1 in this segment, 0 if absent
	ready bool
}

type segExec struct {
	v        *segView
	p        *plan
	st       *stats
	ns       []nodeState
	cache    storedCache
	fieldBuf [][]byte
	scratch  []byte
	timeF    int
	err      error
}

func newSegExec(v *segView, p *plan, st *stats, timeF int) *segExec {
	return &segExec{v: v, p: p, st: st, ns: make([]nodeState, len(p.nodes)), timeF: timeF}
}

// state lazily initializes the eval cursors of node id.
func (e *segExec) state(id int32) *nodeState {
	s := &e.ns[id]
	if s.ready {
		return s
	}
	s.ready = true
	n := &e.p.nodes[id]
	sg := e.v.seg
	switch n.kind {
	case planTerm:
		for _, f := range n.fields {
			s.curs = append(s.curs, sg.termIter(f, n.terms[0], e.scratch))
			s.idf = append(s.idf, BM25IDF(e.st.n, e.st.df[dfKey(f, n.terms[0])]))
		}
	case planPhrase:
		for _, f := range n.fields {
			idf := 0.0
			for _, t := range n.terms {
				s.curs = append(s.curs, sg.termIter(f, t, e.scratch))
				idf += BM25IDF(e.st.n, e.st.df[dfKey(f, t)])
			}
			s.idf = append(s.idf, idf)
		}
	case planRegex:
		if n.bounded() {
			s.tri = e.trigramIter(n.tq)
		}
	case planKeyword:
		if i, ok := slices.BinarySearch(sg.kwVals[n.field], n.value); ok {
			s.ord = uint32(i) + 1
		}
	}
	return s
}

func emptyIter() docIter { return newSliceIter(nil) }

func (e *segExec) trigramIter(q *TrigramQuery) docIter {
	sg := e.v.seg
	switch q.Op {
	case TrigramAll:
		return &allIter{n: uint32(sg.n)}
	case TrigramNone:
		return emptyIter()
	case TrigramAnd:
		var its []docIter
		for _, t := range q.Trigrams {
			it := sg.trigramIter(t)
			if it == nil {
				return emptyIter()
			}
			its = append(its, it)
		}
		for _, s := range q.Sub {
			its = append(its, e.trigramIter(s))
		}
		return newAndIter(its)
	}
	var its []docIter
	for _, t := range q.Trigrams {
		if it := sg.trigramIter(t); it != nil {
			its = append(its, it)
		}
	}
	for _, s := range q.Sub {
		its = append(its, e.trigramIter(s))
	}
	if len(its) == 0 {
		return emptyIter()
	}
	return newOrIter(its)
}

// candidates returns an iterator over a superset of the docs matching
// node id, or all=true when the node does not bound its matches.
func (e *segExec) candidates(id int32) (docIter, bool) {
	n := &e.p.nodes[id]
	sg := e.v.seg
	switch n.kind {
	case planNone:
		return emptyIter(), false
	case planTerm:
		var its []docIter
		for _, f := range n.fields {
			if it := sg.termIter(f, n.terms[0], e.scratch); it != nil {
				its = append(its, it)
			}
		}
		if len(its) == 0 {
			return emptyIter(), false
		}
		return newOrIter(its), false
	case planPhrase:
		var per []docIter
	fields:
		for _, f := range n.fields {
			var its []docIter
			for _, t := range n.terms {
				it := sg.termIter(f, t, e.scratch)
				if it == nil {
					continue fields
				}
				its = append(its, it)
			}
			per = append(per, newAndIter(its))
		}
		if len(per) == 0 {
			return emptyIter(), false
		}
		return newOrIter(per), false
	case planRegex:
		if !n.bounded() {
			return nil, true
		}
		return e.trigramIter(n.tq), false
	case planKeyword, planRange:
		return newSliceIter(filterDocs(sg, n)), false
	case planAnd:
		var its []docIter
		for _, k := range n.kids {
			if e.p.nodes[k].kind == planNot {
				continue
			}
			if it, all := e.candidates(k); !all {
				its = append(its, it)
			}
		}
		if len(its) == 0 {
			return nil, true
		}
		return newAndIter(its), false
	case planOr:
		var its []docIter
		for _, k := range n.kids {
			it, all := e.candidates(k)
			if all {
				return nil, true
			}
			its = append(its, it)
		}
		return newOrIter(its), false
	}
	return nil, true // planNot
}

// eval decides whether doc matches node id and returns its score.
func (e *segExec) eval(id int32, doc uint32) (bool, float64) {
	n := &e.p.nodes[id]
	sg := e.v.seg
	switch n.kind {
	case planNone:
		return false, 0
	case planTerm:
		s := e.state(id)
		m, score := false, 0.0
		for i, f := range n.fields {
			c := s.curs[i]
			if c == nil || c.advance(doc) != doc {
				continue
			}
			m = true
			score += BM25(s.idf[i], int(c.tf), int(sg.norms[f][doc]), e.st.avgdl[f])
		}
		return m, score
	case planPhrase:
		s := e.state(id)
		m, score := false, 0.0
		nt := len(n.terms)
		for i, f := range n.fields {
			curs := s.curs[i*nt : (i+1)*nt]
			ok := true
			for _, c := range curs {
				if c == nil || c.advance(doc) != doc {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			pf := phraseFreq(curs, n.offsets)
			if pf == 0 {
				continue
			}
			m = true
			score += BM25(s.idf[i], pf, int(sg.norms[f][doc]), e.st.avgdl[f])
		}
		return m, score
	case planRegex:
		s := e.state(id)
		if s.tri != nil && s.tri.advance(doc) != doc {
			return false, 0
		}
		vals, err := sg.storedFields(doc, &e.cache, e.fieldBuf)
		if err != nil {
			e.err = err
			return false, 0
		}
		e.fieldBuf = vals
		for _, f := range n.fields {
			if v := vals[f]; len(v) > 0 && n.re.Match(v) {
				return true, 0
			}
		}
		return false, 0
	case planKeyword:
		s := e.state(id)
		return s.ord != 0 && sg.kwOrds[n.field][doc] == s.ord, 0
	case planRange:
		if !sg.numHas[n.field].get(doc) {
			return false, 0
		}
		x := sg.numVals[n.field][doc]
		return x >= n.lo && x < n.hi, 0
	case planAnd:
		score := 0.0
		for _, k := range n.kids {
			m, s := e.eval(k, doc)
			if !m {
				return false, 0
			}
			score += s
		}
		return true, score
	case planOr:
		any, score := false, 0.0
		for _, k := range n.kids {
			if m, s := e.eval(k, doc); m {
				any = true
				score += s
			}
		}
		return any, score
	case planNot:
		m, _ := e.eval(n.kids[0], doc)
		return !m, 0
	}
	return false, 0
}

// phraseFreq counts start positions p of terms[0] such that every
// terms[i] occurs at p + offsets[i].
func phraseFreq(curs []*postIter, offsets []int) int {
	first := curs[0].positions()
	n := 0
outer:
	for _, p := range first {
		for i := 1; i < len(curs); i++ {
			want := int64(p) + int64(offsets[i])
			if want < 0 || want > int64(^uint32(0)) {
				continue outer
			}
			if _, ok := slices.BinarySearch(curs[i].positions(), uint32(want)); !ok {
				continue outer
			}
		}
		n++
	}
	return n
}

// execute runs plan p over views and returns the top k.
func execute(views []*segView, p *plan, nt, timeF, k, regexLimit int) (Result, error) {
	if k <= 0 {
		k = 10
	}
	if err := checkUnbounded(views, p, regexLimit); err != nil {
		return Result{}, err
	}
	st := collectStats(views, p, nt)
	top := &topK{k: k}
	total := 0
	for _, v := range views {
		e := newSegExec(v, p, st, timeF)
		it, all := e.candidates(p.root)
		if all {
			it = &allIter{n: uint32(v.seg.n)}
		}
		for d := it.nextDoc(); d != noMore; d = it.nextDoc() {
			if v.deleted(d) {
				continue
			}
			m, score := e.eval(p.root, d)
			if e.err != nil {
				return Result{}, e.err
			}
			if !m {
				continue
			}
			total++
			h := Hit{ID: v.seg.ids[d], Score: score}
			if timeF >= 0 && v.seg.numHas[timeF].get(d) {
				h.At, h.HasAt = v.seg.numVals[timeF][d], true
			}
			top.push(h)
		}
	}
	return Result{Hits: top.sorted(), Total: total}, nil
}
