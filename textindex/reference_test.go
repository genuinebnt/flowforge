package textindex

import (
	"errors"
	"maps"
	"slices"
)

// refIndex is the brute-force reference: a map of live documents per
// snapshot, evaluated by scanning every document and analyzing its text
// from scratch. It shares only the plan (lowering) and the BM25 formula
// with the index.
type refIndex struct {
	schema    Schema
	timeName  string
	timeF     int
	maxRegex  int
	current   map[uint64]docFields
	committed map[uint64]docFields
	gen       uint64
	cache     []refDoc // analysis of committed, nil when stale
}

func newRef(s Schema, maxRegex int) *refIndex {
	r := &refIndex{schema: s, timeF: -1, maxRegex: maxRegex, current: map[uint64]docFields{}, committed: map[uint64]docFields{}}
	if t, i := s.lookup("at"); t == fieldNumeric {
		r.timeName, r.timeF = "at", i
	}
	return r
}

func (r *refIndex) add(d Document, replace bool) error {
	f, err := normalize(r.schema, d)
	if err != nil {
		return err
	}
	if _, ok := r.current[d.ID]; ok && !replace {
		return ErrDuplicateID
	}
	r.gen++
	f.gen = r.gen
	r.current[d.ID] = f
	return nil
}

func (r *refIndex) delete(id uint64) {
	if _, ok := r.current[id]; ok {
		r.gen++
		delete(r.current, id)
	}
}

func (r *refIndex) refresh() { r.committed = maps.Clone(r.current); r.cache = nil }

// refDoc is a committed document with its analysis.
type refDoc struct {
	f   docFields
	pos []map[string][]int // per text field: term -> positions
	dl  []int
}

func analyzeRef(f docFields) refDoc {
	rd := refDoc{f: f}
	for _, text := range f.text {
		m := map[string][]int{}
		for _, t := range Analyze([]byte(text)) {
			m[t.Term] = append(m[t.Term], t.Pos)
		}
		rd.pos = append(rd.pos, m)
		rd.dl = append(rd.dl, FieldLength([]byte(text)))
	}
	return rd
}

func (r *refIndex) search(q string, opts SearchOptions) (Result, error) {
	pq, err := Parse(q)
	if err != nil {
		return Result{}, err
	}
	p, err := lowerQuery(pq, r.schema, r.timeName, opts.Now)
	if err != nil {
		return Result{}, err
	}
	if r.cache == nil {
		ids := slices.Sorted(maps.Keys(r.committed))
		r.cache = make([]refDoc, len(ids))
		for i, id := range ids {
			r.cache[i] = analyzeRef(r.committed[id])
		}
	}
	docs := r.cache
	nt := len(r.schema.Text)
	n := len(docs)
	avgdl := make([]float64, nt)
	for f := range nt {
		sum := 0
		for _, d := range docs {
			sum += d.dl[f]
		}
		if n > 0 {
			avgdl[f] = float64(sum) / float64(n)
		}
	}
	df := func(f int, t string) int {
		c := 0
		for _, d := range docs {
			if len(d.pos[f][t]) > 0 {
				c++
			}
		}
		return c
	}
	// Regexes without trigrams: the root's filters must cap candidates.
	if len(p.unbounded) > 0 {
		sp := p.nodes[p.unbounded[0]].span
		filters := topFilters(p)
		if len(filters) == 0 {
			return Result{}, qerr(CodeRegexNoTrigrams, sp.Start, sp.End, "ref")
		}
		count := 0
		for _, d := range docs {
			ok := true
			for _, fi := range filters {
				if m, _ := refEval(p, fi, &d, n, df, avgdl); !m {
					ok = false
				}
			}
			if ok {
				count++
			}
		}
		if count > r.maxRegex {
			return Result{}, qerr(CodeRegexNoTrigrams, sp.Start, sp.End, "ref")
		}
	}
	k := opts.K
	if k <= 0 {
		k = 10
	}
	var hits []Hit
	for _, d := range docs {
		m, s := refEval(p, p.root, &d, n, df, avgdl)
		if !m {
			continue
		}
		h := Hit{ID: d.f.id, Score: s}
		if r.timeF >= 0 && d.f.hasNum[r.timeF] {
			h.At, h.HasAt = d.f.numeric[r.timeF], true
		}
		hits = append(hits, h)
	}
	slices.SortFunc(hits, func(a, b Hit) int {
		if betterHit(a, b) {
			return -1
		}
		if betterHit(b, a) {
			return 1
		}
		return 0
	})
	total := len(hits)
	if len(hits) > k {
		hits = hits[:k]
	}
	return Result{Hits: hits, Total: total}, nil
}

func refEval(p *plan, id int32, d *refDoc, n int, df func(int, string) int, avgdl []float64) (bool, float64) {
	nd := &p.nodes[id]
	switch nd.kind {
	case planNone:
		return false, 0
	case planTerm:
		m, s := false, 0.0
		for _, f := range nd.fields {
			tf := len(d.pos[f][nd.terms[0]])
			if tf == 0 {
				continue
			}
			m = true
			s += BM25(BM25IDF(n, df(f, nd.terms[0])), tf, d.dl[f], avgdl[f])
		}
		return m, s
	case planPhrase:
		m, s := false, 0.0
		for _, f := range nd.fields {
			pf := 0
			for _, p0 := range d.pos[f][nd.terms[0]] {
				ok := true
				for i := 1; i < len(nd.terms); i++ {
					if !slices.Contains(d.pos[f][nd.terms[i]], p0+nd.offsets[i]) {
						ok = false
						break
					}
				}
				if ok {
					pf++
				}
			}
			if pf == 0 {
				continue
			}
			idf := 0.0
			for _, t := range nd.terms {
				idf += BM25IDF(n, df(f, t))
			}
			m = true
			s += BM25(idf, pf, d.dl[f], avgdl[f])
		}
		return m, s
	case planRegex:
		for _, f := range nd.fields {
			if v := d.f.text[f]; v != "" && nd.re.MatchString(v) {
				return true, 0
			}
		}
		return false, 0
	case planKeyword:
		return d.f.keyword[nd.field] == nd.value && nd.value != "", 0
	case planRange:
		x := d.f.numeric[nd.field]
		return d.f.hasNum[nd.field] && x >= nd.lo && x < nd.hi, 0
	case planAnd:
		s := 0.0
		for _, k := range nd.kids {
			m, ks := refEval(p, k, d, n, df, avgdl)
			if !m {
				return false, 0
			}
			s += ks
		}
		return true, s
	case planOr:
		any, s := false, 0.0
		for _, k := range nd.kids {
			if m, ks := refEval(p, k, d, n, df, avgdl); m {
				any = true
				s += ks
			}
		}
		return any, s
	case planNot:
		m, _ := refEval(p, nd.kids[0], d, n, df, avgdl)
		return !m, 0
	}
	return false, 0
}

// sameOutcome compares an index result with the reference: equal error
// codes, or identical hits (ids, bit-identical scores, times) and totals.
func sameOutcome(got Result, gerr error, want Result, werr error) (bool, string) {
	var ge, we *QueryError
	if gerr != nil || werr != nil {
		if errors.As(gerr, &ge) && errors.As(werr, &we) && ge.Code == we.Code {
			return true, ""
		}
		return false, "errors differ: got " + errString(gerr) + " want " + errString(werr)
	}
	if got.Total != want.Total || len(got.Hits) != len(want.Hits) {
		return false, "totals or lengths differ"
	}
	for i := range got.Hits {
		if got.Hits[i] != want.Hits[i] {
			return false, "hit differs"
		}
	}
	return true, ""
}

func errString(e error) string {
	if e == nil {
		return "<nil>"
	}
	return e.Error()
}
