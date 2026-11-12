package vecindex

import (
	"errors"
	"math"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/genuinebnt/flowforge/core"
)

func fillIndex(t *testing.T, idx Index, vecs [][]float32) {
	t.Helper()
	for i, v := range vecs {
		if err := idx.Upsert(idOf(i), v, uint64(i+1)); err != nil {
			t.Fatal(err)
		}
	}
}

func randomVectors(seed uint64, n, dim int) [][]float32 {
	r := core.NewSplitMix64(seed)
	out := make([][]float32, n)
	for i := range out {
		out[i] = randomVector(r, dim)
	}
	return out
}

// Filters are applied after retrieval with over-fetch, retrying with a
// larger L when fewer than k survive (spec 22.7).
func TestSegmentedFilters(t *testing.T) {
	const dim = 8
	s := testSegmented(t, L2, dim, 100, func(c *Config) { c.DisableAutoMerge = true })
	vecs := randomVectors(1, 500, dim)
	fillIndex(t, s, vecs)
	bf, _ := NewBruteForce(L2, dim)
	fillIndex(t, bf, vecs)
	// Keep one id in 50 (2%: above the exact-fallback threshold, and
	// selective enough that k*4 over-fetch alone cannot find k).
	var calls atomic.Int64
	f := &Filter{Keep: func(id core.UUID) bool { calls.Add(1); return id[15]%50 == 0 }}
	for _, q := range randomVectors(2, 20, dim) {
		got, err := s.Search(q, SearchParams{K: 5, Filter: f})
		if err != nil {
			t.Fatal(err)
		}
		want, _ := bf.Search(q, SearchParams{K: 5, Filter: f})
		if len(got) != 5 {
			t.Fatalf("filtered search returned %d hits, want 5", len(got))
		}
		for _, h := range got {
			if !f.Keep(h.ID) {
				t.Fatalf("hit %s fails the filter", h.ID)
			}
		}
		if recallAt([][]core.UUID{ids(want)}, [][]core.UUID{ids(got)}) < 0.6 {
			t.Fatalf("filtered recall too low: got %v want %v", ids(got), ids(want))
		}
	}
	if calls.Load() == 0 {
		t.Fatal("filter never consulted")
	}

	t.Run("exact fallback for highly selective filters", func(t *testing.T) {
		match := []core.UUID{idOf(3), idOf(77), idOf(450), idOf(3), idOf(9999)} // duplicate and unknown ids
		f := &Filter{
			Keep:        func(id core.UUID) bool { return slices.Contains(match, id) },
			MatchingIDs: func() []core.UUID { return match },
		}
		q := vecs[77]
		got, err := s.Search(q, SearchParams{K: 10, Filter: f})
		if err != nil {
			t.Fatal(err)
		}
		want, _ := bf.Search(q, SearchParams{K: 10, Filter: f})
		if !slices.Equal(ids(got), ids(want)) || len(got) != 3 || got[0].ID != idOf(77) || got[0].Distance != 0 {
			t.Fatalf("exact fallback: got %+v want %+v", got, want)
		}
		// Deleted ids are skipped by the fallback too.
		if err := s.Delete(idOf(77), 10_000); err != nil {
			t.Fatal(err)
		}
		got, _ = s.Search(q, SearchParams{K: 10, Filter: f})
		if slices.Contains(ids(got), idOf(77)) || len(got) != 2 {
			t.Fatalf("fallback returned a deleted id: %+v", got)
		}
	})
}

func ids(hits []Hit) []core.UUID {
	out := make([]core.UUID, len(hits))
	for i, h := range hits {
		out[i] = h.ID
	}
	return out
}

// Merge policy: a segment whose tombstones exceed 20% is rewritten, and more
// than MaxSegments segments are combined.
func TestSegmentedAutoMerge(t *testing.T) {
	const dim = 8
	t.Run("tombstone ratio", func(t *testing.T) {
		s := testSegmented(t, L2, dim, 50, nil)
		fillIndex(t, s, randomVectors(3, 100, dim))
		if s.Info().Segments != 2 {
			t.Fatalf("segments = %d", s.Info().Segments)
		}
		before := s.merges
		for i := range 10 { // exactly 20% of segment 0: no merge yet
			if err := s.Delete(idOf(i), uint64(1000+i)); err != nil {
				t.Fatal(err)
			}
		}
		if s.merges != before || s.Info().Tombstones != 10 {
			t.Fatalf("merged at 20%%: merges %d, info %+v", s.merges-before, s.Info())
		}
		if err := s.Delete(idOf(10), 2000); err != nil {
			t.Fatal(err)
		}
		info := s.Info()
		if s.merges != before+1 || info.Tombstones != 0 || info.Vectors != 89 || info.Segments != 2 {
			t.Fatalf("after crossing 20%%: merges %d, info %+v", s.merges-before, info)
		}
		for _, ls := range s.segs {
			if ls.countDead() != ls.deadCount {
				t.Fatal("tombstone count out of sync with bitset")
			}
		}
	})
	t.Run("segment count", func(t *testing.T) {
		s := testSegmented(t, L2, dim, 10, func(c *Config) { c.MaxSegments = 3 })
		vecs := randomVectors(4, 200, dim)
		fillIndex(t, s, vecs)
		info := s.Info()
		if info.Segments > 3 || info.Vectors != 200 || info.Tombstones != 0 {
			t.Fatalf("info %+v", info)
		}
		entries, _ := os.ReadDir(s.cfg.Dir)
		if len(entries) != info.Segments {
			t.Fatalf("%d files for %d segments", len(entries), info.Segments)
		}
		for i, v := range vecs {
			hits, err := s.Search(v, SearchParams{K: 1})
			if err != nil {
				t.Fatal(err)
			}
			if hits[0].ID != idOf(i) || hits[0].Distance != 0 {
				t.Fatalf("vector %d lost after merges: %+v", i, hits)
			}
		}
	})
}

func TestSegmentedLifecycle(t *testing.T) {
	const dim = 4
	s := testSegmented(t, Cosine, dim, 5, func(c *Config) { c.ModelID, c.ModelVersion = "m", "1" })
	fillIndex(t, s, randomVectors(5, 12, dim))
	info := s.Info()
	if info.ModelID != "m" || info.ModelVersion != "1" || info.Dimension != dim || info.Metric != Cosine ||
		info.Vectors != 12 || info.Segments != 2 || info.MemtableVectors != 2 || info.DiskBytes == 0 || info.RAMBytes == 0 || info.WatermarkSeq != 12 {
		t.Fatalf("info %+v", info)
	}
	// Concurrent searches and writes are safe (run with -race).
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Go(func() {
			r := core.NewSplitMix64(uint64(g))
			for i := range 50 {
				if g == 0 {
					if err := s.Upsert(idOf(100+i), randomVector(r, dim), uint64(100+i)); err != nil {
						t.Error(err)
					}
					continue
				}
				if _, err := s.Search(randomVector(r, dim), SearchParams{K: 3}); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	paths := s.SegmentPaths()
	if err := s.Destroy(); err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("segment %s survived Destroy", p)
		}
	}
	if err := s.Upsert(idOf(1), []float32{1, 0, 0, 0}, 1000); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err := s.Delete(idOf(1), 1000); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if _, err := s.Search([]float32{1, 0, 0, 0}, SearchParams{K: 1}); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err := s.Flush(); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err := s.Merge(); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}

	t.Run("config validation", func(t *testing.T) {
		dir := t.TempDir()
		bad := []Config{
			{Dim: 4, Metric: L2},
			{Dir: dir, Dim: 0, Metric: L2},
			{Dir: dir, Dim: 4},
			{Dir: dir, Dim: 4, Metric: L2, FlushThreshold: -1},
			{Dir: dir, Dim: 4, Metric: L2, MaxSegments: -1},
			{Dir: dir, Dim: 4, Metric: L2, MergeTombstoneRatio: 2},
			{Dir: dir, Dim: 4, Metric: L2, PQ: PQConfig{M: 5}},
			{Dir: dir, Dim: 4, Metric: L2, Graph: VamanaParams{Alpha: 0.5}},
		}
		for _, c := range bad {
			if _, err := NewSegmented(c); !errors.Is(err, ErrInvalidConfig) && !errors.Is(err, ErrInvalidMetric) {
				t.Fatalf("%+v: %v", c, err)
			}
		}
	})
}

// Cosine and inner-product segments reach high recall too.
func TestSegmentMetrics(t *testing.T) {
	base, qs := siftLike(12, 2000, 50, 32).generate()
	for _, m := range []Metric{Cosine, InnerProduct} {
		truth := groundTruth(t, m, base, qs, 10)
		seg, _ := buildTestSegment(t, base, SegmentConfig{Metric: m, Graph: VamanaParams{R: 32, LBuild: 64}}, 3)
		got := make([][]core.UUID, len(qs))
		for i, q := range qs {
			hits, _, err := seg.Search(q, 10, 100, 4)
			if err != nil {
				t.Fatal(err)
			}
			got[i] = make([]core.UUID, len(hits))
			for j, h := range hits {
				got[i][j] = h.ID
			}
		}
		r := recallAt(truth, got)
		t.Logf("%v recall@10 = %.4f", m, r)
		if r < 0.9 {
			t.Fatalf("%v recall@10 = %.4f", m, r)
		}
	}
}

func TestSegmentAccessors(t *testing.T) {
	base := randomVectors(9, 40, 6)
	seg, file := buildTestSegment(t, base, SegmentConfig{Metric: L2, Graph: VamanaParams{R: 6, LBuild: 12}, PQ: PQConfig{M: 3}, TombstoneEpoch: 7}, 1)
	if seg.Len() != 40 || seg.Dim() != 6 || seg.R() != 6 || seg.Metric() != L2 || seg.TombstoneEpoch() != 7 ||
		seg.MaxSeq() != 40 || seg.PQ().M() != 3 || seg.DiskBytes() != uint64(len(file)) || seg.RAMBytes() == 0 {
		t.Fatal("accessors disagree with the build")
	}
	if int(seg.Medoid()) >= seg.Len() {
		t.Fatal("medoid out of range")
	}
	for i := range uint32(40) {
		id, seq, ok := seg.Entry(i)
		if !ok || id != idOf(int(i)) || seq != uint64(i+1) {
			t.Fatalf("entry %d = %s %d", i, id, seq)
		}
		v, err := seg.Vector(i)
		if err != nil || !slices.Equal(v, base[i]) {
			t.Fatalf("vector %d: %v", i, err)
		}
		nb, err := seg.Neighbors(i)
		if err != nil || len(nb) > 6 {
			t.Fatalf("neighbours %d: %v %v", i, nb, err)
		}
	}
	if _, _, ok := seg.Entry(40); ok {
		t.Fatal("entry out of range")
	}
	if _, err := seg.Vector(40); !errors.Is(err, ErrInvalidParams) {
		t.Fatal(err)
	}
	if _, err := seg.Neighbors(40); !errors.Is(err, ErrInvalidParams) {
		t.Fatal(err)
	}
	if _, _, err := seg.Search(base[0], 5, 4, 1); !errors.Is(err, ErrInvalidParams) {
		t.Fatal(err)
	}
	if _, _, err := seg.Search([]float32{1}, 1, 4, 1); !errors.Is(err, ErrDimension) {
		t.Fatal(err)
	}
	// Large records span several sectors (dim 1100: 4400 + 4 + 4R bytes).
	big := randomVectors(10, 5, 1100)
	bseg, bfile := buildTestSegment(t, big, SegmentConfig{Metric: L2, Graph: VamanaParams{R: 4, LBuild: 8}, PQ: PQConfig{M: 4}}, 1)
	if bseg.lay.perSector != 0 || bseg.lay.sectorsPerRecord != 2 {
		t.Fatalf("layout %+v", bseg.lay)
	}
	for i := range 5 {
		hits, _, err := bseg.Search(big[i], 1, 8, 2)
		if err != nil || hits[0].ID != idOf(i) {
			t.Fatalf("big record %d: %+v %v", i, hits, err)
		}
	}
	bad := slices.Clone(bfile)
	bad[SectorSize*2+100] ^= 1
	if _, err := OpenSegment(bytesReaderAt(bad), int64(len(bad))); !errors.Is(err, ErrChecksum) {
		t.Fatal(err)
	}
	if _, err := WriteSegment(&memFile{}, nil, SegmentConfig{Metric: L2}, core.NewSplitMix64(1)); !errors.Is(err, ErrEmpty) {
		t.Fatal(err)
	}
	if _, err := WriteSegment(&memFile{}, []SegmentEntry{{Vector: []float32{1}}}, SegmentConfig{}, core.NewSplitMix64(1)); !errors.Is(err, ErrInvalidMetric) {
		t.Fatal(err)
	}
}

func TestHybridSearch(t *testing.T) {
	bf, _ := NewBruteForce(L2, 1)
	for i, x := range []float32{0, 1, 2, 3} {
		if err := bf.Upsert(idOf(i), []float32{x}, 1); err != nil {
			t.Fatal(err)
		}
	}
	// Vector ranks: 0, 1, 2, 3. Lexical ranks: 3, 1.
	got, err := HybridSearch(bf, []float32{0}, SearchParams{K: 3}, []core.UUID{idOf(3), idOf(1)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// 1: 1/62 + 1/62; 3: 1/61; 0: 1/61; tie 0 vs 3 by id.
	want := []core.UUID{idOf(1), idOf(0), idOf(3)}
	if !slices.Equal(ids(got), want) {
		t.Fatalf("hybrid order %v, want %v", ids(got), want)
	}
	if got[0].Distance != 1 || math.Abs(float64(got[0].Score)-2.0/62) > 1e-7 {
		t.Fatalf("hit %+v", got[0])
	}
	got, _ = HybridSearch(bf, []float32{0}, SearchParams{K: 1}, []core.UUID{idOf(9)}, 60)
	if len(got) != 1 {
		t.Fatal(got)
	}
	if lex := RRF(60, []core.UUID{idOf(9)}); !math.IsInf(float64(lex[0].Distance), 1) {
		t.Fatal("lexical-only hit should have +Inf distance")
	}
	if _, err := HybridSearch(bf, []float32{0}, SearchParams{}, nil, 60); !errors.Is(err, ErrInvalidParams) {
		t.Fatal(err)
	}
}

func TestSimHashIndexAndLSH(t *testing.T) {
	x := NewSimHashIndex()
	x.Add(idOf(1), 0)
	x.Add(idOf(2), 0b111)
	x.Add(idOf(1), ^uint64(0)) // replace
	if x.Len() != 2 {
		t.Fatal(x.Len())
	}
	got := x.Near(0)
	if len(got) != 1 || got[0].ID != idOf(2) || got[0].Distance != 3 {
		t.Fatalf("Near(0) = %+v", got)
	}
	x.Remove(idOf(2))
	x.Remove(idOf(2))
	if got := x.Near(0); len(got) != 0 {
		t.Fatalf("after remove: %+v", got)
	}
	if got := x.Near(^uint64(0)); len(got) != 1 || got[0].ID != idOf(1) {
		t.Fatalf("Near(1<<63) = %+v", got)
	}
	if SimHash(nil) != 0 || len(Shingles(nil, 3)) != 0 || len(CharNGrams("", 3)) != 0 {
		t.Fatal("empty inputs")
	}
	if g := CharNGrams("ab\xffc", 2); !slices.Equal(g, []string{"ab", "b\xff", "\xffc"}) {
		t.Fatalf("invalid UTF-8 grams %q", g)
	}

	if _, err := NewLSH(0, 4); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal(err)
	}
	if _, err := NewMinHasher(0, 1); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal(err)
	}
	l, _ := NewLSH(DefaultLSHBands, DefaultLSHRows)
	m := DefaultMinHasher()
	if m.Permutations() != 128 {
		t.Fatal(m.Permutations())
	}
	sig := m.Signature([]string{"a", "b", "c"})
	if err := l.Add(idOf(1), sig[:5]); !errors.Is(err, ErrDimension) {
		t.Fatal(err)
	}
	if _, err := l.Candidates(sig[:5]); !errors.Is(err, ErrDimension) {
		t.Fatal(err)
	}
	if err := l.Add(idOf(1), sig); err != nil {
		t.Fatal(err)
	}
	if err := l.Add(idOf(2), sig); err != nil {
		t.Fatal(err)
	}
	if c, _ := l.Candidates(sig); !slices.Equal(c, []core.UUID{idOf(1), idOf(2)}) {
		t.Fatal(c)
	}
	l.Remove(idOf(1))
	if c, _ := l.Candidates(sig); !slices.Equal(c, []core.UUID{idOf(2)}) {
		t.Fatal(c)
	}
	if EstimateJaccard(sig, sig[:3]) != 0 || Jaccard(nil, nil) != 1 {
		t.Fatal("degenerate Jaccard")
	}
	// The MinHash estimate is unbiased: average error over many pairs is
	// small.
	r := core.NewSplitMix64(3)
	var bias float64
	for trial := range 200 {
		var a, b []string
		for i := range 60 {
			s := string(rune('a'+i%26)) + string(rune('a'+i/26)) + string(rune('0'+trial%10))
			if core.IntN(r, 3) > 0 {
				a = append(a, s)
			}
			if core.IntN(r, 3) > 0 {
				b = append(b, s)
			}
		}
		bias += EstimateJaccard(m.Signature(a), m.Signature(b)) - Jaccard(a, b)
	}
	if math.Abs(bias/200) > 0.02 {
		t.Fatalf("MinHash bias %.4f", bias/200)
	}
}

func TestMulAddMod61(t *testing.T) {
	r := core.NewSplitMix64(1)
	for range 10000 {
		a, x, b := r.Uint64()%mersenne61, r.Uint64()%mersenne61, r.Uint64()%mersenne61
		hi, lo := mulBig(a, x)
		want := addMod(modBig(hi, lo), b)
		if got := mulAddMod61(a, x, b); got != want {
			t.Fatalf("mulAddMod61(%d, %d, %d) = %d, want %d", a, x, b, got, want)
		}
	}
}

// mulBig, modBig and addMod are a slow, obviously correct reference for
// mulAddMod61 (schoolbook 128-bit remainder).
func mulBig(a, b uint64) (uint64, uint64) {
	const mask = 1<<32 - 1
	a0, a1, b0, b1 := a&mask, a>>32, b&mask, b>>32
	p00, p01, p10, p11 := a0*b0, a0*b1, a1*b0, a1*b1
	mid := p01&mask + p10&mask + p00>>32
	lo := (mid << 32) | (p00 & mask)
	hi := p11 + p01>>32 + p10>>32 + mid>>32
	return hi, lo
}

func modBig(hi, lo uint64) uint64 {
	var r uint64
	for i := 127; i >= 0; i-- {
		var bit uint64
		if i >= 64 {
			bit = (hi >> (i - 64)) & 1
		} else {
			bit = (lo >> i) & 1
		}
		r = r<<1 | bit
		if r >= mersenne61 {
			r -= mersenne61
		}
	}
	return r
}

func addMod(a, b uint64) uint64 {
	s := a + b
	if s >= mersenne61 {
		s -= mersenne61
	}
	return s
}
