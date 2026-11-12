package vecindex

import (
	"math"
	"slices"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// propIndex is a small, fast segmented index for property tests: every
// few writes flush, merges trigger often.
func propIndex(t *testing.T, metric Metric, dim int, rng core.Rng) *Segmented {
	return testSegmented(t, metric, dim, 8+core.IntN(rng, 40), func(c *Config) {
		c.Seed = rng.Uint64()
		c.MaxSegments = 2 + core.IntN(rng, 4)
		c.Graph = VamanaParams{R: 4 + core.IntN(rng, 8), LBuild: 8 + core.IntN(rng, 16)}
	})
}

// opsDriver applies a random mix of writes to an index under test and to
// the exact model, including stale and replayed seqs.
type opsDriver struct {
	rng   core.Rng
	dim   int
	ids   int
	seq   uint64
	model *BruteForce
	idx   Index
}

func (d *opsDriver) step(t *testing.T) {
	t.Helper()
	id := idOf(core.IntN(d.rng, d.ids))
	seq := d.seq + 1
	if core.IntN(d.rng, 10) == 0 && d.seq > 5 {
		seq = d.seq - uint64(core.IntN(d.rng, 5)) // stale or replayed
	} else {
		d.seq = seq
	}
	if core.IntN(d.rng, 4) == 0 {
		if err := d.model.Delete(id, seq); err != nil {
			t.Fatal(err)
		}
		if err := d.idx.Delete(id, seq); err != nil {
			t.Fatal(err)
		}
		return
	}
	v := randomVector(d.rng, d.dim)
	if err := d.model.Upsert(id, v, seq); err != nil {
		t.Fatal(err)
	}
	if err := d.idx.Upsert(id, v, seq); err != nil {
		t.Fatal(err)
	}
}

func newDriver(t *testing.T, rng core.Rng, metric Metric) *opsDriver {
	dim := 4 + core.IntN(rng, 12)
	model, err := NewBruteForce(metric, dim)
	if err != nil {
		t.Fatal(err)
	}
	return &opsDriver{rng: rng, dim: dim, ids: 30 + core.IntN(rng, 120), model: model, idx: propIndex(t, metric, dim, rng)}
}

// vecindex property: results are always a subset of live ids, with the
// live vector's exact distance, and satisfy the filter.
func TestVecProp_ResultsAreLive(t *testing.T) {
	rng := testkit.Rng(t)
	cases := testkit.Cases()
	metrics := []Metric{L2, Cosine, InnerProduct}
	d := newDriver(t, rng, metrics[0])
	for i := range cases {
		if i%250 == 0 {
			d = newDriver(t, rng, metrics[(i/250)%len(metrics)])
		}
		d.step(t)
		q := randomVector(rng, d.dim)
		p := SearchParams{K: 1 + core.IntN(rng, 15)}
		if core.IntN(rng, 3) == 0 {
			parity := byte(core.IntN(rng, 2))
			p.Filter = &Filter{Keep: func(id core.UUID) bool { return id[15]&1 == parity }}
		}
		hits, err := d.idx.Search(q, p)
		if err != nil {
			t.Fatal(err)
		}
		pq := slices.Clone(q)
		if d.model.tab.metric == Cosine {
			Normalize(pq)
		}
		for _, h := range hits {
			v, ok := d.model.Vector(h.ID)
			if !ok {
				t.Fatalf("case %d: hit %s is not live", i, h.ID)
			}
			if p.Filter != nil && !p.Filter.Keep(h.ID) {
				t.Fatalf("case %d: hit %s fails the filter", i, h.ID)
			}
			want := Distance(d.model.tab.metric, pq, v)
			if math.Abs(float64(want-h.Distance)) > 1e-4*(1+math.Abs(float64(want))) {
				t.Fatalf("case %d: hit %s distance %v, live vector gives %v", i, h.ID, h.Distance, want)
			}
		}
		if n := d.model.Len(); len(hits) > p.K || (p.Filter == nil && len(hits) < min(p.K, n)) {
			t.Fatalf("case %d: %d hits for k=%d with %d live", i, len(hits), p.K, n)
		}
		if info := d.idx.Info(); info.Vectors != uint64(d.model.Len()) {
			t.Fatalf("case %d: index reports %d live vectors, model has %d", i, info.Vectors, d.model.Len())
		}
	}
}

// vecindex property: distances are non-decreasing (ties by id) in every
// result list, for the exact and the approximate index.
func TestVecProp_DistancesSorted(t *testing.T) {
	rng := testkit.Rng(t)
	d := newDriver(t, rng, L2)
	for range 200 {
		d.step(t)
	}
	for i := range testkit.Cases() {
		if i%10 == 0 {
			d.step(t)
		}
		q := randomVector(rng, d.dim)
		p := SearchParams{K: 1 + core.IntN(rng, MaxK)}
		p.L = p.K + core.IntN(rng, 50)
		for _, idx := range []Index{d.idx, d.model} {
			hits, err := idx.Search(q, p)
			if err != nil {
				t.Fatal(err)
			}
			for j := 1; j < len(hits); j++ {
				if compareHit(hits[j-1], hits[j]) >= 0 {
					t.Fatalf("case %d (%T): hits %d and %d out of order: %+v, %+v", i, idx, j-1, j, hits[j-1], hits[j])
				}
			}
		}
	}
}

// vecindex property: after deleting everything, searches are empty.
func TestVecProp_EmptyAfterDeleteAll(t *testing.T) {
	rng := testkit.Rng(t)
	runs := max(2, testkit.Cases()/50)
	for run := range runs {
		metric := []Metric{L2, Cosine, InnerProduct}[run%3]
		dim := 2 + core.IntN(rng, 10)
		idx := propIndex(t, metric, dim, rng)
		n := 1 + core.IntN(rng, 120)
		seq := uint64(0)
		for i := range n {
			seq++
			if err := idx.Upsert(idOf(i), randomVector(rng, dim), seq); err != nil {
				t.Fatal(err)
			}
		}
		if core.IntN(rng, 2) == 0 {
			if err := idx.Flush(); err != nil {
				t.Fatal(err)
			}
		}
		order := make([]int, n)
		for i := range order {
			order[i] = i
		}
		core.Shuffle(rng, n, func(i, j int) { order[i], order[j] = order[j], order[i] })
		for _, i := range order {
			seq++
			if err := idx.Delete(idOf(i), seq); err != nil {
				t.Fatal(err)
			}
		}
		for range 5 {
			hits, err := idx.Search(randomVector(rng, dim), SearchParams{K: 1 + core.IntN(rng, 20)})
			if err != nil {
				t.Fatal(err)
			}
			if len(hits) != 0 {
				t.Fatalf("run %d: %d hits after deleting all %d vectors", run, len(hits), n)
			}
		}
		if info := idx.Info(); info.Vectors != 0 || info.MemtableVectors != 0 {
			t.Fatalf("run %d: info after delete-all: %+v", run, info)
		}
	}
}

// vecindex property: on a fixed dataset, recall@k does not decrease as L
// grows (within a tolerance of 0.02: beam search is not strictly monotone
// per query).
func TestVecProp_RecallMonotonicInL(t *testing.T) {
	rng := testkit.Rng(t)
	const k = 10
	n := 3000
	if testing.Short() {
		n = 1500
	}
	base, qs := siftLike(rng.Uint64(), n, 100, 32).generate()
	truth := groundTruth(t, L2, base, qs, k)
	seg, _ := buildTestSegment(t, base, SegmentConfig{Metric: L2, Graph: VamanaParams{R: 24, LBuild: 48}, PQ: PQConfig{M: 8}}, rng.Uint64())
	prev := -1.0
	for _, L := range []int{10, 20, 40, 80, 160, 320} {
		got := make([][]core.UUID, len(qs))
		for i, q := range qs {
			hits, _, err := seg.Search(q, k, L, 1+core.IntN(rng, 8))
			if err != nil {
				t.Fatal(err)
			}
			for _, h := range hits {
				got[i] = append(got[i], h.ID)
			}
		}
		r := recallAt(truth, got)
		t.Logf("L=%d recall@10=%.4f", L, r)
		if r < prev-0.02 {
			t.Fatalf("recall dropped from %.4f to %.4f at L=%d", prev, r, L)
		}
		prev = max(prev, r)
	}
}
