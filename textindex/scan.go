package textindex

import (
	"fmt"
	"maps"
	"slices"
	"sync"
)

// ScanIndex is the brute-force engine: live documents in a map, every
// query evaluated by re-analyzing and scanning every document. It has the
// semantics of Index (visibility at Refresh, the REGEX_NO_TRIGRAMS rule,
// scores, order) and shares only lowering and the Scorer with it, which
// makes it the test oracle (TIDX-INV-01, -02). Use it for tiny corpora;
// it keeps nothing on disk and ignores Dir, Codec, Intersector, Regex and
// the merge settings.
type ScanIndex struct {
	mu        sync.Mutex
	schema    Schema
	timeName  string
	timeF     int
	maxRegex  int
	scorer    Scorer
	current   map[uint64]docFields
	committed map[uint64]docFields
	gen       uint64
	cache     []refDoc // analysis of committed, nil when stale
	closed    bool
}

// NewScanIndex returns an empty ScanIndex for cfg.Schema.
func NewScanIndex(cfg Config) (*ScanIndex, error) {
	if err := cfg.Schema.Validate(); err != nil {
		return nil, err
	}
	name, tf, err := resolveTimeField(cfg.Schema, cfg.TimeField)
	if err != nil {
		return nil, err
	}
	if cfg.MaxRegexCandidates == 0 {
		cfg.MaxRegexCandidates = 10_000
	}
	scorer, _, _ := defaultsFor(cfg)
	return &ScanIndex{schema: cfg.Schema, timeName: name, timeF: tf, maxRegex: cfg.MaxRegexCandidates, scorer: scorer,
		current: map[uint64]docFields{}, committed: map[uint64]docFields{}}, nil
}

// Add implements Searcher.
func (r *ScanIndex) Add(d Document) error { return r.locked(func() error { return r.add(d, false) }) }

// Update implements Searcher.
func (r *ScanIndex) Update(d Document) error { return r.locked(func() error { return r.add(d, true) }) }

// Delete implements Searcher.
func (r *ScanIndex) Delete(id uint64) error {
	return r.locked(func() error { r.delete(id); return nil })
}

// Refresh implements Searcher.
func (r *ScanIndex) Refresh() error { return r.locked(func() error { r.refresh(); return nil }) }

// Close implements Searcher.
func (r *ScanIndex) Close() error {
	return r.locked(func() error { r.closed = true; return nil })
}

// Search implements Searcher.
func (r *ScanIndex) Search(q string, opts SearchOptions) (res Result, err error) {
	err = r.locked(func() error { res, err = r.search(q, opts); return err })
	return res, err
}

// SearchQuery implements Searcher.
func (r *ScanIndex) SearchQuery(q *Query, opts SearchOptions) (res Result, err error) {
	err = r.locked(func() error { res, err = r.searchQuery(q, opts); return err })
	return res, err
}

// Get implements Searcher.
func (r *ScanIndex) Get(id uint64) (Document, bool, error) {
	var d Document
	var ok bool
	err := r.locked(func() error {
		var f docFields
		if f, ok = r.committed[id]; ok {
			d = f.document(r.schema)
		}
		return nil
	})
	return d, ok, err
}

func (r *ScanIndex) locked(f func() error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	return f()
}

func (r *ScanIndex) add(d Document, replace bool) error {
	f, err := normalize(r.schema, d)
	if err != nil {
		return err
	}
	if _, ok := r.current[d.ID]; ok && !replace {
		return fmt.Errorf("%w: %d", ErrDuplicateID, d.ID)
	}
	r.gen++
	f.gen = r.gen
	r.current[d.ID] = f
	return nil
}

func (r *ScanIndex) delete(id uint64) {
	if _, ok := r.current[id]; ok {
		r.gen++
		delete(r.current, id)
	}
}

func (r *ScanIndex) refresh() { r.committed = maps.Clone(r.current); r.cache = nil }

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

func (r *ScanIndex) search(q string, opts SearchOptions) (Result, error) {
	pq, err := Parse(q)
	if err != nil {
		return Result{}, err
	}
	return r.searchQuery(pq, opts)
}

func (r *ScanIndex) searchQuery(pq *Query, opts SearchOptions) (Result, error) {
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
			return Result{}, qerr(CodeRegexNoTrigrams, sp.Start, sp.End, "regex has no usable trigrams and the filters do not cap the candidates")
		}
		count := 0
		for _, d := range docs {
			ok := true
			for _, fi := range filters {
				if m, _ := r.eval(p, fi, &d, n, df, avgdl); !m {
					ok = false
				}
			}
			if ok {
				count++
			}
		}
		if count > r.maxRegex {
			return Result{}, qerr(CodeRegexNoTrigrams, sp.Start, sp.End, "regex has no usable trigrams and the filters do not cap the candidates")
		}
	}
	k := opts.K
	if k <= 0 {
		k = 10
	}
	var hits []Hit
	for _, d := range docs {
		m, s := r.eval(p, p.root, &d, n, df, avgdl)
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

func (r *ScanIndex) eval(p *plan, id int32, d *refDoc, n int, df func(int, string) int, avgdl []float64) (bool, float64) {
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
			s += r.scorer.Score(r.scorer.IDF(n, df(f, nd.terms[0])), tf, d.dl[f], avgdl[f])
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
				idf += r.scorer.IDF(n, df(f, t))
			}
			m = true
			s += r.scorer.Score(idf, pf, d.dl[f], avgdl[f])
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
			m, ks := r.eval(p, k, d, n, df, avgdl)
			if !m {
				return false, 0
			}
			s += ks
		}
		return true, s
	case planOr:
		any, s := false, 0.0
		for _, k := range nd.kids {
			if m, ks := r.eval(p, k, d, n, df, avgdl); m {
				any = true
				s += ks
			}
		}
		return any, s
	case planNot:
		m, _ := r.eval(p, nd.kids[0], d, n, df, avgdl)
		return !m, 0
	}
	return false, 0
}
