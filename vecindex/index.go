package vecindex

import (
	"bytes"
	"fmt"
	"slices"

	"github.com/genuinebnt/flowforge/core"
)

// Index is the VectorIndex port of spec 24.4 (section 22.9). An index holds
// the vectors of one (tenant, program, model) triple (see Registry).
//
// Writes are ordered by source_seq, the graph change_events sequence that
// produced them: a write whose seq is not newer than the last write applied
// for the same id is ignored, so replays and redeliveries are idempotent and
// a stale upsert can never resurrect a deleted entity.
//
// Implementations are safe for concurrent use.
type Index interface {
	// Upsert inserts or replaces the vector of id. The vector must have
	// the index dimension and finite components.
	Upsert(id core.UUID, v []float32, seq uint64) error
	// Delete removes id. It hides the entity from searches immediately.
	Delete(id core.UUID, seq uint64) error
	// Search returns up to p.K live hits sorted by distance ascending,
	// ties broken by id ascending.
	Search(q []float32, p SearchParams) ([]Hit, error)
	// Info reports the index's size and health (GetIndexInfo).
	Info() IndexInfo
}

// Search parameter limits from spec 22.9.
const (
	// MaxK is the largest k a search may ask for.
	MaxK = 100
	// DefaultSearchList is L when SearchParams.L is zero.
	DefaultSearchList = 100
	// DefaultBeamWidth is W when SearchParams.BeamWidth is zero (spec
	// 22.5: "4 is a good start").
	DefaultBeamWidth = 4
	// MaxDimension bounds the vector dimension of every index and segment.
	MaxDimension = 1 << 16
)

// SearchParams mirrors the SearchParams message of spec 22.9.
type SearchParams struct {
	// K is the number of hits wanted, 1..100.
	K int
	// L is the search list size; zero means 100. It must be >= K.
	L int
	// BeamWidth is the number of nodes expanded (and sectors read) per
	// beam-search iteration on a segment; zero means 4.
	BeamWidth int
	// Filter, when non-nil, restricts results (spec 22.7).
	Filter *Filter
}

// Filter is a post-retrieval filter (spec 22.7). Tenant and program
// filtering is structural (one index per triple, see Registry), so a
// Filter only expresses the remaining conditions (asset type, tag,
// last_seen range), which the caller evaluates against entity metadata the
// index does not hold.
type Filter struct {
	// Keep reports whether id passes the filter. It is required and must
	// be safe for concurrent calls.
	Keep func(id core.UUID) bool
	// MatchingIDs optionally lists every id that passes the filter (the
	// spec's "matching ids fetched from Postgres"). When it is set and
	// fewer than 1% of the index's vectors match, the search skips the
	// approximate path and scans exactly those ids.
	MatchingIDs func() []core.UUID
}

// Hit is one search result (spec 22.9). The in_scope field of the proto is
// a service concern (every hit passes the scope service, spec 22) and is not
// represented here.
type Hit struct {
	// ID is the entity id.
	ID core.UUID
	// Distance is the metric distance to the query (smaller is closer).
	// For hybrid results it is the vector distance when the entity came
	// back from the vector search, and +Inf otherwise.
	Distance float32
	// Score is the reciprocal rank fusion score for hybrid results; plain
	// vector searches leave it zero.
	Score float32
}

// IndexInfo mirrors the IndexInfo message of spec 22.9.
type IndexInfo struct {
	ModelID         string
	ModelVersion    string
	Dimension       int
	Metric          Metric
	Vectors         uint64 // live vectors
	Tombstones      uint64 // superseded or deleted vectors still stored in segments
	Segments        int
	MemtableVectors uint64
	RAMBytes        uint64
	DiskBytes       uint64
	WatermarkSeq    uint64 // newest source seq applied
}

// normalized validates p and fills defaults.
func (p SearchParams) normalized() (SearchParams, error) {
	if p.K < 1 || p.K > MaxK {
		return p, fmt.Errorf("%w: k=%d outside 1..%d", ErrInvalidParams, p.K, MaxK)
	}
	if p.L == 0 {
		p.L = max(DefaultSearchList, p.K)
	}
	if p.L < p.K {
		return p, fmt.Errorf("%w: search list size %d < k=%d", ErrInvalidParams, p.L, p.K)
	}
	if p.BeamWidth < 0 {
		return p, fmt.Errorf("%w: beam width %d", ErrInvalidParams, p.BeamWidth)
	}
	if p.BeamWidth == 0 {
		p.BeamWidth = DefaultBeamWidth
	}
	if p.Filter != nil && p.Filter.Keep == nil {
		return p, fmt.Errorf("%w: filter without Keep", ErrInvalidParams)
	}
	return p, nil
}

// keep applies the optional filter.
func (p SearchParams) keep(id core.UUID) bool { return p.Filter == nil || p.Filter.Keep(id) }

// compareID orders ids bytewise (the canonical string order).
func compareID(a, b core.UUID) int { return bytes.Compare(a[:], b[:]) }

// compareHit is the result order: distance ascending, then id ascending.
func compareHit(a, b Hit) int {
	switch {
	case a.Distance < b.Distance:
		return -1
	case a.Distance > b.Distance:
		return 1
	}
	return compareID(a.ID, b.ID)
}

// topK keeps the k best hits seen so far in a bounded max-heap.
type topK struct {
	k int
	h []Hit // max-heap by compareHit
}

func newTopK(k int) *topK { return &topK{k: k, h: make([]Hit, 0, k)} }

// full reports whether the heap holds k hits.
func (t *topK) full() bool { return len(t.h) >= t.k }

// push offers x.
func (t *topK) push(x Hit) {
	if t.k <= 0 {
		return
	}
	if len(t.h) < t.k {
		t.h = append(t.h, x)
		t.up(len(t.h) - 1)
		return
	}
	if compareHit(x, t.h[0]) >= 0 {
		return
	}
	t.h[0] = x
	t.down(0)
}

// accepts reports whether a hit at distance d could still enter the heap.
func (t *topK) accepts(d float32) bool { return !t.full() || d <= t.h[0].Distance }

func (t *topK) up(i int) {
	for i > 0 {
		p := (i - 1) / 2
		if compareHit(t.h[i], t.h[p]) <= 0 {
			return
		}
		t.h[i], t.h[p] = t.h[p], t.h[i]
		i = p
	}
}

func (t *topK) down(i int) {
	n := len(t.h)
	for {
		l := 2*i + 1
		if l >= n {
			return
		}
		c := l
		if r := l + 1; r < n && compareHit(t.h[r], t.h[l]) > 0 {
			c = r
		}
		if compareHit(t.h[c], t.h[i]) <= 0 {
			return
		}
		t.h[i], t.h[c] = t.h[c], t.h[i]
		i = c
	}
}

// sorted returns the kept hits in result order. The heap is consumed.
func (t *topK) sorted() []Hit {
	out := t.h
	t.h = nil
	slices.SortFunc(out, compareHit)
	return out
}

// mergeSorted is the scatter-gather k-way merge: it combines result lists
// that are each sorted by compareHit, keeps the best occurrence of every id
// and returns at most k hits.
func mergeSorted(lists [][]Hit, k int) []Hit {
	type cursor struct {
		list []Hit
		pos  int
	}
	cur := make([]cursor, 0, len(lists))
	for _, l := range lists {
		if len(l) > 0 {
			cur = append(cur, cursor{list: l})
		}
	}
	out := make([]Hit, 0, k)
	seen := make(map[core.UUID]struct{}, k)
	for len(out) < k {
		best := -1
		for i := range cur {
			c := &cur[i]
			if c.pos >= len(c.list) {
				continue
			}
			if best < 0 || compareHit(c.list[c.pos], cur[best].list[cur[best].pos]) < 0 {
				best = i
			}
		}
		if best < 0 {
			break
		}
		h := cur[best].list[cur[best].pos]
		cur[best].pos++
		if _, dup := seen[h.ID]; dup {
			continue
		}
		seen[h.ID] = struct{}{}
		out = append(out, h)
	}
	return out
}
