package vecindex

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// relErr is |got - want| relative to the magnitude of the computation:
// max(|want|, sum of |terms|), the standard normalisation for summation
// error, so cancellation to near zero does not inflate it. The floor of
// 1e-30 absorbs products that underflow float32 (subnormal edge values),
// which no float32 kernel can represent.
func relErr(got, want, mag float64) float64 {
	den := max(math.Abs(want), mag, 1e-30)
	return math.Abs(got-want) / den
}

// vec_001: the optimised kernels agree with the scalar reference within
// 1e-4 relative on random vectors, including odd dimensions and NaN-free
// edge values.
func TestVec001_Kernels(t *testing.T) {
	rng := testkit.Rng(t)
	edges := []float32{0, float32(math.Copysign(0, -1)), 1e-40, -1e-40, math.SmallestNonzeroFloat32,
		1.17549435e-38, 1e-20, -1e-20, 1e15, -1e15, 1, -1, 65504, 3.4e15}
	dims := []int{0, 1, 2, 3, 4, 5, 7, 8, 9, 15, 16, 17, 31, 33, 63, 64, 65, 100, 127, 128, 129, 383, 384, 767, 768, 960, 961}
	for _, dim := range dims {
		for trial := range 20 {
			a, b := make([]float32, dim), make([]float32, dim)
			for i := range a {
				a[i] = float32(gaussian(rng) * 10)
				b[i] = float32(gaussian(rng) * 10)
				if trial%4 == 3 { // sprinkle edge values
					if core.IntN(rng, 3) == 0 {
						a[i] = edges[core.IntN(rng, len(edges))]
					}
					if core.IntN(rng, 3) == 0 {
						b[i] = edges[core.IntN(rng, len(edges))]
					}
				}
			}
			var magDot, exactDot, exactL2 float64
			for i := range a {
				p := float64(a[i]) * float64(b[i])
				magDot += math.Abs(p)
				exactDot += p
				d := float64(a[i]) - float64(b[i])
				exactL2 += d * d
			}
			fd, sd := float64(Dot(a, b)), float64(DotScalar(a, b))
			if e := relErr(fd, sd, magDot); e > 1e-4 {
				t.Fatalf("dim %d trial %d: Dot=%g DotScalar=%g rel err %g", dim, trial, fd, sd, e)
			}
			if e := relErr(fd, exactDot, magDot); e > 1e-4 {
				t.Fatalf("dim %d trial %d: Dot=%g exact=%g rel err %g", dim, trial, fd, exactDot, e)
			}
			fl, sl := float64(L2Squared(a, b)), float64(L2SquaredScalar(a, b))
			if e := relErr(fl, sl, sl); e > 1e-4 {
				t.Fatalf("dim %d trial %d: L2Squared=%g scalar=%g rel err %g", dim, trial, fl, sl, e)
			}
			if e := relErr(fl, exactL2, exactL2); e > 1e-4 {
				t.Fatalf("dim %d trial %d: L2Squared=%g exact=%g rel err %g", dim, trial, fl, exactL2, e)
			}
			for _, m := range []Metric{L2, Cosine, InnerProduct} {
				f, s := float64(Distance(m, a, b)), float64(DistanceScalar(m, a, b))
				mag := magDot + 1
				if m == L2 {
					mag = sl
				}
				if e := relErr(f, s, mag); e > 1e-4 {
					t.Fatalf("dim %d metric %v: %g vs %g", dim, m, f, s)
				}
			}
		}
	}
	t.Run("metric names", func(t *testing.T) {
		for _, m := range []Metric{L2, Cosine, InnerProduct} {
			back, err := ParseMetric(m.String())
			if err != nil || back != m {
				t.Fatalf("ParseMetric(%q) = %v, %v", m.String(), back, err)
			}
		}
		if _, err := ParseMetric("hamming"); !errors.Is(err, ErrInvalidMetric) {
			t.Fatalf("ParseMetric(hamming) err = %v", err)
		}
	})
	t.Run("normalize", func(t *testing.T) {
		v := []float32{3, 4}
		if !Normalize(v) || math.Abs(float64(v[0])-0.6) > 1e-7 || math.Abs(float64(v[1])-0.8) > 1e-7 {
			t.Fatalf("Normalize = %v", v)
		}
		if Normalize([]float32{0, 0}) {
			t.Fatal("zero vector normalised")
		}
	})
}

// vec_002 is data driven (conformance/vecindex/bruteforce); see
// conformance_test.go. This test pins the API-level behaviour around it.
func TestVec002_BruteForceTiesByID(t *testing.T) {
	bf, err := NewBruteForce(L2, 2)
	if err != nil {
		t.Fatal(err)
	}
	// Four points at distance 1 from the origin, inserted in reverse id
	// order: ties must come back in id order.
	pts := [][]float32{{1, 0}, {0, 1}, {-1, 0}, {0, -1}}
	for i := len(pts) - 1; i >= 0; i-- {
		if err := bf.Upsert(idOf(i), pts[i], 1); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := bf.Search([]float32{0, 0}, SearchParams{K: 3})
	if err != nil {
		t.Fatal(err)
	}
	want := []core.UUID{idOf(0), idOf(1), idOf(2)}
	for i, h := range hits {
		if h.ID != want[i] || h.Distance != 1 {
			t.Fatalf("hit %d = %v, want %v at distance 1", i, h, want[i])
		}
	}
	t.Run("invalid input", func(t *testing.T) {
		if err := bf.Upsert(idOf(9), []float32{1}, 2); !errors.Is(err, ErrDimension) {
			t.Fatalf("short vector: %v", err)
		}
		if err := bf.Upsert(idOf(9), []float32{float32(math.NaN()), 0}, 2); !errors.Is(err, ErrInvalidVector) {
			t.Fatalf("NaN: %v", err)
		}
		for _, p := range []SearchParams{{K: 0}, {K: 101}, {K: 10, L: 5}, {K: 1, BeamWidth: -1}, {K: 1, Filter: &Filter{}}} {
			if _, err := bf.Search([]float32{0, 0}, p); !errors.Is(err, ErrInvalidParams) {
				t.Fatalf("params %+v: %v", p, err)
			}
		}
		cos, _ := NewBruteForce(Cosine, 2)
		if err := cos.Upsert(idOf(1), []float32{0, 0}, 1); !errors.Is(err, ErrInvalidVector) {
			t.Fatalf("zero cosine vector: %v", err)
		}
		if _, err := NewBruteForce(MetricUnspecified, 2); !errors.Is(err, ErrInvalidMetric) {
			t.Fatalf("metric: %v", err)
		}
	})
}

// vec_003 is data driven (conformance/vecindex/pq); this test pins the
// quantiser's API and determinism.
func TestVec003_PQEncodeDecode(t *testing.T) {
	base, _ := siftLike(3, 3000, 0, 32).generate()
	pq1, err := TrainPQ(base, PQConfig{M: 8, TrainSample: 1000}, core.NewSplitMix64(5))
	if err != nil {
		t.Fatal(err)
	}
	pq2, err := TrainPQ(base, PQConfig{M: 8, TrainSample: 1000, Workers: 1}, core.NewSplitMix64(5))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pq1.codebooks, pq2.codebooks) {
		t.Fatal("PQ training is not deterministic across worker counts")
	}
	if pq1.M() != 8 || pq1.Centroids() != 256 || pq1.Dim() != 32 {
		t.Fatalf("shape %d/%d/%d", pq1.M(), pq1.Centroids(), pq1.Dim())
	}
	code, err := pq1.Encode(base[0])
	if err != nil {
		t.Fatal(err)
	}
	dec, err := pq1.Decode(code)
	if err != nil {
		t.Fatal(err)
	}
	// The asymmetric distance from a query to a code equals the exact
	// distance from the query to the decoded vector.
	for _, m := range []Metric{L2, InnerProduct, Cosine} {
		q := base[1]
		if m == Cosine {
			q = slices.Clone(q)
			Normalize(q)
		}
		tab, err := pq1.Table(q, m)
		if err != nil {
			t.Fatal(err)
		}
		got, want := float64(tab.Distance(code)), float64(DistanceScalar(m, q, dec))
		if relErr(got, want, math.Abs(want)+1) > 1e-4 {
			t.Fatalf("%v: ADC %g, exact-to-decoded %g", m, got, want)
		}
	}
	// Uneven sub-spaces: dim 10 into 3 sub-vectors of 4, 3, 3.
	odd := make([][]float32, 50)
	r := core.NewSplitMix64(1)
	for i := range odd {
		odd[i] = randomVector(r, 10)
	}
	pq3, err := TrainPQ(odd, PQConfig{M: 3}, core.NewSplitMix64(2))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pq3.offs, []int{0, 4, 7, 10}) || pq3.Centroids() != 50 {
		t.Fatalf("offsets %v centroids %d", pq3.offs, pq3.Centroids())
	}
	for _, v := range odd { // k = n: every training point is a centroid
		c, _ := pq3.Encode(v)
		d, _ := pq3.Decode(c)
		if !slices.Equal(d, v) {
			t.Fatalf("lossless reconstruction failed: %v vs %v", d, v)
		}
	}
	t.Run("errors", func(t *testing.T) {
		if _, err := TrainPQ(nil, PQConfig{M: 1}, core.NewSplitMix64(1)); !errors.Is(err, ErrEmpty) {
			t.Fatal(err)
		}
		for _, cfg := range []PQConfig{{M: 0}, {M: 11}, {M: 2, Centroids: 300}, {M: 2, Iterations: -1}} {
			if _, err := TrainPQ(odd, cfg, core.NewSplitMix64(1)); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("%+v: %v", cfg, err)
			}
		}
		if _, err := TrainPQ([][]float32{{1, 2}, {1}}, PQConfig{M: 1}, core.NewSplitMix64(1)); !errors.Is(err, ErrDimension) {
			t.Fatal(err)
		}
		if _, err := pq3.Encode([]float32{1}); !errors.Is(err, ErrDimension) {
			t.Fatal(err)
		}
		if _, err := pq3.Decode([]byte{1}); !errors.Is(err, ErrDimension) {
			t.Fatal(err)
		}
		if _, err := pq3.Decode([]byte{1, 2, 200}); !errors.Is(err, ErrCorrupt) {
			t.Fatal(err)
		}
	})
}

// vecCorpusSize is the vec_004/vec_005 corpus: 10,000 vectors, 2,000
// under -short (which -race runs use).
func vecCorpusSize() int {
	if testing.Short() {
		return 2000
	}
	return 10000
}

// vec_004: on 10,000 SIFT-like vectors every node's degree is at most R and
// every node is reachable from the medoid.
func TestVec004_VamanaInvariants(t *testing.T) {
	base, _ := siftLike(4, vecCorpusSize(), 0, 128).generate()
	p := VamanaParams{} // spec defaults: R 64, L_build 100, alpha 1.2
	g, err := BuildGraph(base, L2, p, core.NewSplitMix64(11))
	if err != nil {
		t.Fatal(err)
	}
	checkGraphInvariants(t, g)

	t.Run("deterministic and independent of workers", func(t *testing.T) {
		small := base[:600]
		p := VamanaParams{R: 16, LBuild: 32, Batch: 64, Workers: 1}
		g1, err := BuildGraph(small, L2, p, core.NewSplitMix64(3))
		if err != nil {
			t.Fatal(err)
		}
		p.Workers = 4
		g2, err := BuildGraph(small, L2, p, core.NewSplitMix64(3))
		if err != nil {
			t.Fatal(err)
		}
		if g1.Medoid() != g2.Medoid() {
			t.Fatal("medoid differs")
		}
		for i := range g1.Len() {
			if !slices.Equal(g1.Neighbors(uint32(i)), g2.Neighbors(uint32(i))) {
				t.Fatalf("node %d adjacency differs between 1 and 4 workers", i)
			}
		}
		checkGraphInvariants(t, g2)
	})
	t.Run("tiny graphs", func(t *testing.T) {
		for n := 1; n <= 5; n++ {
			g, err := BuildGraph(base[:n], L2, VamanaParams{R: 2, LBuild: 4}, core.NewSplitMix64(1))
			if err != nil {
				t.Fatal(err)
			}
			checkGraphInvariants(t, g)
			hits, err := g.Search(base[0], 1, 4)
			if err != nil || len(hits) != 1 || hits[0].Node != 0 {
				t.Fatalf("n=%d search: %v %v", n, hits, err)
			}
		}
	})
	t.Run("invalid params", func(t *testing.T) {
		for _, p := range []VamanaParams{{R: -1}, {R: MaxR + 1}, {LBuild: -1}, {Alpha: 0.5}, {Alpha: math.Inf(1)}} {
			if _, err := BuildGraph(base[:10], L2, p, core.NewSplitMix64(1)); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("%+v: %v", p, err)
			}
		}
		if _, err := BuildGraph(nil, L2, VamanaParams{}, core.NewSplitMix64(1)); !errors.Is(err, ErrEmpty) {
			t.Fatal(err)
		}
	})
}

func checkGraphInvariants(t *testing.T, g *Graph) {
	t.Helper()
	n := g.Len()
	for i := range n {
		nb := g.Neighbors(uint32(i))
		if len(nb) > g.R() {
			t.Fatalf("node %d degree %d > R=%d", i, len(nb), g.R())
		}
		seen := map[uint32]bool{}
		for _, j := range nb {
			if int(j) >= n || int(j) == i || seen[j] {
				t.Fatalf("node %d has bad neighbour %d (self, duplicate or out of range)", i, j)
			}
			seen[j] = true
		}
	}
	reached := make([]bool, n)
	queue := []uint32{g.Medoid()}
	reached[g.Medoid()] = true
	count := 1
	for len(queue) > 0 {
		x := queue[0]
		queue = queue[1:]
		for _, j := range g.Neighbors(x) {
			if !reached[j] {
				reached[j] = true
				count++
				queue = append(queue, j)
			}
		}
	}
	if count != n {
		t.Fatalf("only %d of %d nodes reachable from the medoid", count, n)
	}
}

// vec_005: recall@10 against brute force at the default L (100) on 10,000
// vectors is at least 0.95, through the on-disk segment (beam search, PQ
// navigation, exact re-rank) and through the in-memory graph.
func TestVec005_Recall(t *testing.T) {
	const k = 10
	base, qs := siftLike(5, vecCorpusSize(), 200, 128).generate()
	truth := groundTruth(t, L2, base, qs, k)
	// Spec build parameters (R 64, L_build 100, alpha 1.2), built in
	// deterministic batches of 64 on four workers to keep -race runs short.
	build := VamanaParams{Batch: 64, Workers: 4}
	seg, _ := buildTestSegment(t, base, SegmentConfig{Metric: L2, Graph: build, PQ: PQConfig{M: 16}}, 9)
	got := make([][]core.UUID, len(qs))
	reads := 0
	for i, q := range qs {
		hits, st, err := seg.Search(q, k, DefaultSearchList, DefaultBeamWidth)
		if err != nil {
			t.Fatal(err)
		}
		reads += st.NodeReads
		for _, h := range hits {
			got[i] = append(got[i], h.ID)
		}
	}
	r := recallAt(truth, got)
	t.Logf("segment recall@10 = %.4f at L=100, W=4 (%d node reads/query)", r, reads/len(qs))
	if r < 0.95 {
		t.Fatalf("segment recall@10 = %.4f < 0.95", r)
	}

	g, err := BuildGraph(base, L2, build, core.NewSplitMix64(9))
	if err != nil {
		t.Fatal(err)
	}
	for i, q := range qs {
		hits, err := g.Search(q, k, DefaultSearchList)
		if err != nil {
			t.Fatal(err)
		}
		got[i] = got[i][:0]
		for _, h := range hits {
			got[i] = append(got[i], idOf(int(h.Node)))
		}
	}
	if r := recallAt(truth, got); r < 0.95 {
		t.Fatalf("in-memory graph recall@10 = %.4f < 0.95", r)
	}
}

// testSegmented returns a Segmented index in a temp dir with small,
// fast build parameters.
func testSegmented(t testing.TB, metric Metric, dim, flush int, mut func(*Config)) *Segmented {
	t.Helper()
	cfg := Config{
		Dir: t.TempDir(), Dim: dim, Metric: metric, FlushThreshold: flush,
		Graph: VamanaParams{R: 12, LBuild: 24}, PQ: PQConfig{M: max(1, dim/4)}, Seed: 1,
	}
	if mut != nil {
		mut(&cfg)
	}
	s, err := NewSegmented(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func vectorBytes(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

// vec_006: an upsert replaces the old vector, a delete hides the entity
// immediately, and a merge physically removes it from the segment files.
func TestVec006_UpsertDeleteMerge(t *testing.T) {
	const dim = 16
	s := testSegmented(t, L2, dim, 100, func(c *Config) { c.DisableAutoMerge = true })
	r := core.NewSplitMix64(6)
	vecs := make([][]float32, 300)
	for i := range vecs {
		vecs[i] = randomVector(r, dim)
		if err := s.Upsert(idOf(i), vecs[i], uint64(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if info := s.Info(); info.Segments != 3 || info.MemtableVectors != 0 || info.Vectors != 300 {
		t.Fatalf("after 300 upserts: %+v", info)
	}
	search := func(q []float32, k int) []Hit {
		t.Helper()
		hits, err := s.Search(q, SearchParams{K: k})
		if err != nil {
			t.Fatal(err)
		}
		return hits
	}
	contains := func(hits []Hit, id core.UUID) (Hit, bool) {
		for _, h := range hits {
			if h.ID == id {
				return h, true
			}
		}
		return Hit{}, false
	}

	// Upsert replaces: entity 7 moves far away.
	x, oldX := idOf(7), vecs[7]
	newX := make([]float32, dim)
	for i := range newX {
		newX[i] = 50
	}
	if err := s.Upsert(x, newX, 1000); err != nil {
		t.Fatal(err)
	}
	if h, ok := contains(search(newX, 1), x); !ok || h.Distance != 0 {
		t.Fatalf("replaced vector not found at distance 0: %+v", search(newX, 1))
	}
	if h, ok := contains(search(oldX, 10), x); ok {
		t.Fatalf("old vector of %s still returned: %+v", x, h)
	}

	// Delete hides immediately, including from a segment.
	y := idOf(42)
	if err := s.Delete(y, 1001); err != nil {
		t.Fatal(err)
	}
	if _, ok := contains(search(vecs[42], 10), y); ok {
		t.Fatal("deleted entity returned")
	}
	// A stale upsert (older seq) cannot resurrect it; replaying the delete
	// is a no-op.
	if err := s.Upsert(y, vecs[42], 999); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(y, 1001); err != nil {
		t.Fatal(err)
	}
	if _, ok := contains(search(vecs[42], 10), y); ok {
		t.Fatal("stale upsert resurrected a deleted entity")
	}
	info := s.Info()
	if info.Vectors != 299 || info.Tombstones != 2 || info.WatermarkSeq != 1001 {
		t.Fatalf("before merge: %+v", info)
	}

	// The files still hold y and the old x before the merge...
	fileHas := func(needle []byte) bool {
		for _, p := range s.SegmentPaths() {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(b, needle) {
				return true
			}
		}
		return false
	}
	if !fileHas(y[:]) || !fileHas(vectorBytes(oldX)) {
		t.Fatal("test setup: segment files should still contain the deleted data")
	}
	if err := s.Flush(); err != nil { // newX is in the memtable
		t.Fatal(err)
	}
	if err := s.Merge(); err != nil {
		t.Fatal(err)
	}
	// ...and no longer after it.
	if fileHas(y[:]) {
		t.Fatal("deleted id still in a segment file after merge")
	}
	if fileHas(vectorBytes(vecs[42])) || fileHas(vectorBytes(oldX)) {
		t.Fatal("deleted or superseded vector still in a segment file after merge")
	}
	info = s.Info()
	if info.Segments != 1 || info.Tombstones != 0 || info.Vectors != 299 {
		t.Fatalf("after merge: %+v", info)
	}
	entries, _ := os.ReadDir(filepath.Dir(s.SegmentPaths()[0]))
	if len(entries) != 1 {
		t.Fatalf("old segment files not deleted: %d files", len(entries))
	}
	if _, ok := contains(search(vecs[42], 10), y); ok {
		t.Fatal("deleted entity returned after merge")
	}
	if h, ok := contains(search(newX, 1), x); !ok || h.Distance != 0 {
		t.Fatal("replaced vector lost by merge")
	}
}

// vec_007: flipping any byte of a segment file makes OpenSegment fail with a
// checksum error, never answer from damaged data.
func TestVec007_SegmentCorruption(t *testing.T) {
	base, _ := siftLike(7, 60, 0, 24).generate()
	_, file := buildTestSegment(t, base, SegmentConfig{Metric: L2, Graph: VamanaParams{R: 8, LBuild: 16}, PQ: PQConfig{M: 4}}, 1)
	step := 1
	if testing.Short() {
		step = 7
	}
	for pos := 0; pos < len(file); pos += step {
		if pos > SectorSize && pos < len(file)-trailerSize && pos%step != 0 {
			continue
		}
		for _, mask := range []byte{0x01, 0x80} {
			bad := slices.Clone(file)
			bad[pos] ^= mask
			_, err := OpenSegment(bytesReaderAt(bad), int64(len(bad)))
			if !errors.Is(err, ErrChecksum) {
				t.Fatalf("flip at byte %d (mask %#x): err = %v, want ErrChecksum", pos, mask, err)
			}
		}
	}
	t.Run("truncated and extended", func(t *testing.T) {
		for _, n := range []int{0, 10, SectorSize, len(file) - 1} {
			if _, err := OpenSegment(bytesReaderAt(file[:n]), int64(n)); !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrChecksum) {
				t.Fatalf("truncated to %d: %v", n, err)
			}
		}
		long := append(slices.Clone(file), 0)
		if _, err := OpenSegment(bytesReaderAt(long), int64(len(long))); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("extended: %v", err)
		}
	})
	t.Run("on disk", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "seg.ffda")
		bad := slices.Clone(file)
		bad[len(bad)/2] ^= 0xff
		if err := os.WriteFile(path, bad, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenSegmentFile(path); !errors.Is(err, ErrChecksum) {
			t.Fatalf("OpenSegmentFile: %v", err)
		}
		if err := os.WriteFile(path, file, 0o644); err != nil {
			t.Fatal(err)
		}
		seg, err := OpenSegmentFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := seg.Search(base[0], 1, 10, 4); err != nil {
			t.Fatal(err)
		}
		seg.Close()
		if _, _, err := seg.Search(base[0], 1, 10, 4); !errors.Is(err, ErrClosed) {
			t.Fatalf("search after close: %v", err)
		}
	})
}

// vec_008: with an adversarial dataset of identical vectors in two
// tenants (same program id, same model), a query in tenant A never
// returns tenant B ids.
func TestVec008_TenantIsolation(t *testing.T) {
	const dim = 8
	tenantA := core.MustParseUUID("0190a000-0000-7000-8000-00000000000a")
	tenantB := core.MustParseUUID("0190a000-0000-7000-8000-00000000000b")
	program := core.MustParseUUID("0190a000-0000-7000-8000-0000000000f0")
	program2 := core.MustParseUUID("0190a000-0000-7000-8000-0000000000f1")
	const model = "e5-small@1"
	reg := NewRegistry()
	keyA := Key{Tenant: tenantA, Program: program, Model: model}
	keyB := Key{Tenant: tenantB, Program: program, Model: model}
	keyB2 := Key{Tenant: tenantB, Program: program2, Model: model}
	for _, k := range []Key{keyA, keyB, keyB2} {
		if err := reg.Register(k, testSegmented(t, Cosine, dim, 20, nil)); err != nil {
			t.Fatal(err)
		}
	}
	r := core.NewSplitMix64(8)
	vecs := make([][]float32, 60)
	idsA := map[core.UUID]bool{}
	for i := range vecs {
		vecs[i] = randomVector(r, dim)
		a := idOf(i)
		b := idOf(1000 + i)
		idsA[a] = true
		// Identical vectors under both tenants and both of B's programs.
		for _, w := range []struct {
			k  Key
			id core.UUID
		}{{keyA, a}, {keyB, b}, {keyB2, idOf(2000 + i)}} {
			if err := reg.Upsert(w.k, w.id, vecs[i], uint64(i+1)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, q := range vecs {
		hits, err := reg.Search(keyA, q, SearchParams{K: 20})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 20 {
			t.Fatalf("got %d hits", len(hits))
		}
		for _, h := range hits {
			if !idsA[h.ID] {
				t.Fatalf("tenant A query returned foreign id %s", h.ID)
			}
		}
		// A cross-program fan-out within tenant A, naming B's other
		// program, still sees only A.
		hits, err = reg.SearchPrograms(tenantA, []core.UUID{program, program2}, model, q, SearchParams{K: 20})
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hits {
			if !idsA[h.ID] {
				t.Fatalf("tenant A fan-out returned foreign id %s", h.ID)
			}
		}
	}
	// Tenant B's fan-out merges its two programs.
	hits, err := reg.SearchPrograms(tenantB, []core.UUID{program, program2}, model, vecs[0], SearchParams{K: 2})
	if err != nil || len(hits) != 2 || idsA[hits[0].ID] || idsA[hits[1].ID] || hits[0].Distance > 1e-6 || hits[1].Distance > 1e-6 {
		t.Fatalf("tenant B fan-out: %+v %v", hits, err)
	}
	t.Run("registry errors", func(t *testing.T) {
		if err := reg.Register(keyA, testSegmented(t, Cosine, dim, 20, nil)); !errors.Is(err, ErrIndexExists) {
			t.Fatal(err)
		}
		if _, err := reg.Lookup(Key{Tenant: tenantA, Program: program2, Model: model}); !errors.Is(err, ErrNoIndex) {
			t.Fatal(err)
		}
		for _, k := range []Key{{Program: program, Model: model}, {Tenant: tenantA, Model: model}, {Tenant: tenantA, Program: program}} {
			if _, err := reg.Search(k, vecs[0], SearchParams{K: 1}); !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("%+v: %v", k, err)
			}
		}
		if got := reg.Keys(tenantB); len(got) != 2 || got[0] != keyB || got[1] != keyB2 {
			t.Fatalf("Keys(B) = %v", got)
		}
		idx, ok := reg.Remove(keyB2)
		if !ok || idx == nil {
			t.Fatal("Remove failed")
		}
		if err := idx.(*Segmented).Destroy(); err != nil {
			t.Fatal(err)
		}
		if _, err := reg.Lookup(keyB2); !errors.Is(err, ErrNoIndex) {
			t.Fatal(err)
		}
	})
}
