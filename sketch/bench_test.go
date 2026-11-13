package sketch

import (
	"fmt"
	"math"
	"testing"

	"github.com/genuinebnt/flowforge/core"
)

// BenchmarkSketchAlgo compares the similarity methods on one document pair
// (1 % of tokens replaced): go test -run '^$' -bench SketchAlgo ./sketch.
// "pair" measures EstimateSimilarity from raw tokens (sketching both
// sides); "stored" measures comparing two precomputed sketches, the cost
// that matters when one side is in an index. abs-err is |estimate - exact
// Jaccard|.
func BenchmarkSketchAlgo(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000} {
		r := core.NewSplitMix64(uint64(n))
		a := randomSeq(r, n, 50_000)
		bb := mutate(r, a, 0.01)
		exact := JaccardSorted(WShingling(a, 3), WShingling(bb, 3))
		for _, m := range Methods() {
			b.Run(fmt.Sprintf("%s/pair/%dk", m, n/1000), func(b *testing.B) {
				b.ReportAllocs()
				var est float64
				for b.Loop() {
					est, _ = EstimateSimilarity(a, bb, SimilarityOptions{Method: m})
				}
				b.ReportMetric(float64(2*n)*float64(b.N)/b.Elapsed().Seconds(), "tokens/s")
				b.ReportMetric(math.Abs(est-exact), "abs-err")
			})
		}
		sa, sb := WShingling(a, 3), WShingling(bb, 3)
		mh := DefaultMinHasher()
		ma, mb := mh.SignatureHashes(sa), mh.SignatureHashes(sb)
		ha, hb := SimHashTokens(a, 3), SimHashTokens(bb, 3)
		b.Run(fmt.Sprintf("exact/stored/%dk", n/1000), func(b *testing.B) {
			for b.Loop() {
				_ = JaccardSorted(sa, sb)
			}
			b.ReportMetric(float64(8*(len(sa)+len(sb))/2), "bytes/doc")
		})
		b.Run(fmt.Sprintf("minhash/stored/%dk", n/1000), func(b *testing.B) {
			for b.Loop() {
				_ = EstimateJaccard(ma, mb)
			}
			b.ReportMetric(float64(8*len(ma)), "bytes/doc")
		})
		b.Run(fmt.Sprintf("simhash/stored/%dk", n/1000), func(b *testing.B) {
			for b.Loop() {
				_ = Hamming(ha, hb)
			}
			b.ReportMetric(8, "bytes/doc")
		})
	}
}

// BenchmarkSketchIndex measures near-duplicate lookups among 100,000
// stored documents: SimHashIndex.Near versus LSH.Candidates.
func BenchmarkSketchIndex(b *testing.B) {
	r := core.NewSplitMix64(9)
	cmp := func(x, y uint32) int { return int(int64(x) - int64(y)) }
	x := NewSimHashIndex[uint32](cmp)
	l, _ := NewLSH[uint32](DefaultLSHBands, DefaultLSHRows, cmp)
	m := DefaultMinHasher()
	sig := make([]uint64, 128)
	for i := range uint32(100_000) {
		x.Add(i, r.Uint64())
		for k := range sig {
			sig[k] = r.Uint64()
		}
		_ = l.Add(i, sig)
	}
	q := m.SignatureHashes([]uint64{1, 2, 3})
	b.Run("simhash", func(b *testing.B) {
		for b.Loop() {
			_ = x.Near(r.Uint64())
		}
	})
	b.Run("lsh", func(b *testing.B) {
		for b.Loop() {
			_, _ = l.Candidates(q)
		}
	})
}
