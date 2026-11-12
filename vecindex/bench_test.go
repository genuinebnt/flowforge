package vecindex

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// Benchmarks of spec 25.8 (b_vec_01 .. b_vec_05), scaled from SIFT1M to
// synthetic SIFT-like corpora that build in seconds. Run the heavy ones
// once:
//
//	go test ./vecindex -run '^$' -bench 'Vec0[2-5]' -benchtime=1x
//	go test ./vecindex -run '^$' -bench Vec01

// benchCorpus is the shared 20k x 128 corpus with 200 queries and their
// exact top-10.
var benchCorpus = sync.OnceValue(func() *corpus {
	base, qs := siftLike(2025, 20_000, 200, 128).generate()
	return &corpus{base: base, queries: qs}
})

type corpus struct {
	base, queries [][]float32
	truthOnce     sync.Once
	truth         [][]core.UUID
}

func (c *corpus) groundTruth(b *testing.B) [][]core.UUID {
	c.truthOnce.Do(func() { c.truth = groundTruth(b, L2, c.base, c.queries, 10) })
	return c.truth
}

// b_vec_01: distance kernels, scalar reference versus the unrolled kernel.
func BenchmarkVec01_Distance(b *testing.B) {
	kernels := []struct {
		name string
		fn   func(a, b []float32) float32
	}{
		{"l2/scalar", L2SquaredScalar},
		{"l2/unrolled", L2Squared},
		{"ip/scalar", DotScalar},
		{"ip/unrolled", Dot},
	}
	for _, dim := range []int{128, 384, 768, 960} {
		r := core.NewSplitMix64(uint64(dim))
		x, y := randomVector(r, dim), randomVector(r, dim)
		for _, k := range kernels {
			b.Run(fmt.Sprintf("dim=%d/%s", dim, k.name), func(b *testing.B) {
				bytes := int64(8 * dim)
				b.SetBytes(bytes)
				b.ReportAllocs()
				var sink float32
				for b.Loop() {
					sink += k.fn(x, y)
				}
				b.ReportMetric(float64(bytes)*float64(b.N)/b.Elapsed().Seconds()/1e9, "GB/s")
				_ = sink
			})
		}
	}
}

// b_vec_02: segment build (Vamana R=64, L_build=100, alpha=1.2, plus PQ and
// the file write) on 20k vectors. "sequential" is the spec algorithm;
// the batched builds produce one graph whatever the worker count.
func BenchmarkVec02_Build(b *testing.B) {
	c := benchCorpus()
	entries := make([]SegmentEntry, len(c.base))
	for i, v := range c.base {
		entries[i] = SegmentEntry{ID: idOf(i), Seq: uint64(i + 1), Vector: v}
	}
	for _, v := range []struct {
		name string
		p    VamanaParams
	}{
		{"sequential", VamanaParams{}},
		{"batch=256/workers=1", VamanaParams{Batch: 256, Workers: 1}},
		{"batch=256/workers=4", VamanaParams{Batch: 256, Workers: 4}},
		{"batch=256/workers=8", VamanaParams{Batch: 256, Workers: 8}},
	} {
		b.Run(v.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var f memFile
				if _, err := WriteSegment(&f, entries, SegmentConfig{Metric: L2, Graph: v.p, PQ: PQConfig{M: 16}}, core.NewSplitMix64(1)); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(len(entries))*float64(b.N)/b.Elapsed().Seconds(), "vectors/s")
		})
	}
}

var benchSegment = sync.OnceValue(func() *Segment {
	c := benchCorpus()
	entries := make([]SegmentEntry, len(c.base))
	for i, v := range c.base {
		entries[i] = SegmentEntry{ID: idOf(i), Seq: uint64(i + 1), Vector: v}
	}
	var f memFile
	cfg := SegmentConfig{Metric: L2, Graph: VamanaParams{Batch: 256, Workers: 4}, PQ: PQConfig{M: 16}}
	n, err := WriteSegment(&f, entries, cfg, core.NewSplitMix64(1))
	if err != nil {
		panic(err)
	}
	seg, err := OpenSegment(bytesReaderAt(f.b), n)
	if err != nil {
		panic(err)
	}
	return seg
})

// b_vec_03: recall@10 versus latency over L (and beam width) on one
// 20k-vector segment; each op runs the whole 200-query set, so recall and
// the per-query metrics hold even with -benchtime=1x.
func BenchmarkVec03_Search(b *testing.B) {
	c := benchCorpus()
	truth := c.groundTruth(b)
	seg := benchSegment()
	type point struct{ L, W int }
	var points []point
	for _, L := range []int{10, 50, 100, 200} {
		points = append(points, point{L, 4})
	}
	points = append(points, point{100, 1}, point{100, 8})
	for _, pt := range points {
		b.Run(fmt.Sprintf("L=%d/W=%d", pt.L, pt.W), func(b *testing.B) {
			b.ReportAllocs()
			got := make([][]core.UUID, len(c.queries))
			lat := make([]time.Duration, 0, len(c.queries))
			reads := 0
			for b.Loop() {
				lat = lat[:0]
				reads = 0
				for qi, q := range c.queries {
					start := time.Now()
					hits, st, err := seg.Search(q, 10, pt.L, pt.W)
					lat = append(lat, time.Since(start))
					if err != nil {
						b.Fatal(err)
					}
					got[qi] = got[qi][:0]
					for _, h := range hits {
						got[qi] = append(got[qi], h.ID)
					}
					reads += st.NodeReads
				}
			}
			slices.Sort(lat)
			nq := float64(len(c.queries))
			b.ReportMetric(recallAt(truth, got), "recall@10")
			b.ReportMetric(float64(reads)/nq, "reads/query")
			b.ReportMetric(nq*float64(b.N)/b.Elapsed().Seconds(), "QPS")
			b.ReportMetric(float64(lat[len(lat)/2].Microseconds()), "p50-us")
			b.ReportMetric(float64(lat[len(lat)*99/100].Microseconds()), "p99-us")
		})
	}
}

// b_vec_04: RAM versus disk bytes per vector for PQ m = 16, 32, 64 (dim
// 128): RAM holds the PQ code plus the 24-byte id entry; the full vector
// (512 B) and adjacency live only on disk.
func BenchmarkVec04_Memory(b *testing.B) {
	base := benchCorpus().base[:5000]
	entries := make([]SegmentEntry, len(base))
	for i, v := range base {
		entries[i] = SegmentEntry{ID: idOf(i), Seq: uint64(i + 1), Vector: v}
	}
	for _, m := range []int{16, 32, 64} {
		b.Run(fmt.Sprintf("m=%d", m), func(b *testing.B) {
			var seg *Segment
			for b.Loop() {
				var f memFile
				cfg := SegmentConfig{Metric: L2, Graph: VamanaParams{Batch: 256, Workers: 4}, PQ: PQConfig{M: m}}
				n, err := WriteSegment(&f, entries, cfg, core.NewSplitMix64(1))
				if err != nil {
					b.Fatal(err)
				}
				if seg, err = OpenSegment(bytesReaderAt(f.b), n); err != nil {
					b.Fatal(err)
				}
			}
			nv := float64(seg.Len())
			b.ReportMetric(float64(seg.RAMBytes())/nv, "RAM-B/vector")
			b.ReportMetric(float64(len(seg.codes)+len(seg.ids)*idEntrySize)/nv, "RAM-B/vector-excl-codebook")
			b.ReportMetric(float64(seg.DiskBytes())/nv, "disk-B/vector")
		})
	}
}

// b_vec_05: streaming updates. Each op is 3,000 steps of one insert, one
// delete and one query against a segmented index preloaded with 10k
// vectors (flush every 2,000, merges per policy); recall@10 of the
// queries is measured against an exact mirror and compared with the
// static index's recall.
func BenchmarkVec05_Streaming(b *testing.B) {
	c := benchCorpus()
	const preload, steps = 10_000, 3000
	for b.Loop() {
		b.StopTimer()
		s := testSegmented(b, L2, 128, 2000, func(cfg *Config) {
			cfg.Graph = VamanaParams{Batch: 256, Workers: 4}
			cfg.PQ = PQConfig{M: 16}
		})
		mirror, _ := NewBruteForce(L2, 128)
		for i := range preload {
			if err := s.Upsert(idOf(i), c.base[i], uint64(i+1)); err != nil {
				b.Fatal(err)
			}
			_ = mirror.Upsert(idOf(i), c.base[i], uint64(i+1))
		}
		if err := s.Flush(); err != nil {
			b.Fatal(err)
		}
		measure := func(q []float32) float64 {
			got, err := s.Search(q, SearchParams{K: 10})
			if err != nil {
				b.Fatal(err)
			}
			want, _ := mirror.Search(q, SearchParams{K: 10})
			return recallAt([][]core.UUID{ids(want)}, [][]core.UUID{ids(got)})
		}
		var static float64
		for _, q := range c.queries {
			static += measure(q)
		}
		static /= float64(len(c.queries))
		mergesBefore := s.merges
		b.StartTimer()

		var recall float64
		var slowest time.Duration
		seq := uint64(preload)
		for step := range steps {
			seq++
			in := preload + step
			start := time.Now()
			if err := s.Upsert(idOf(in), c.base[in], seq); err != nil {
				b.Fatal(err)
			}
			_ = mirror.Upsert(idOf(in), c.base[in], seq)
			seq++
			if err := s.Delete(idOf(step*7%preload), seq); err != nil {
				b.Fatal(err)
			}
			_ = mirror.Delete(idOf(step*7%preload), seq)
			// A flush or merge runs inside the write that triggers it.
			slowest = max(slowest, time.Since(start))
			recall += measure(c.queries[step%len(c.queries)])
		}
		b.ReportMetric(static, "static-recall@10")
		b.ReportMetric(recall/steps, "streaming-recall@10")
		b.ReportMetric((static-recall/steps)*100, "recall-drift-points")
		b.ReportMetric(float64(s.merges-mergesBefore), "merges")
		b.ReportMetric(float64(slowest.Microseconds())/1000, "slowest-write-ms")
		b.ReportMetric(float64(s.Info().Segments), "segments")
	}
}
