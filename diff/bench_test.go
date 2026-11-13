package diff

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/core"
)

// benchSeq draws n tokens from a log-uniform distribution over a 50,000
// token vocabulary, so a few tokens (like "{" or "return" in code) repeat
// a lot and most are rare.
func benchSeq(r core.Rng, n int) []uint32 {
	s := make([]uint32, n)
	for i := range s {
		s[i] = uint32(math.Pow(50_000, core.Float64(r)))
	}
	return s
}

// benchEdits applies n*rate single-token edits.
func benchEdits(r core.Rng, a []uint32, rate float64) []uint32 {
	return editSeq(r, a, int(float64(len(a))*rate), 50_000)
}

// benchSkip says which algorithm/size/rate combinations are not run:
// Myers keeps an O(D^2) trace, Reference an O(NM) table, and both Myers
// variants cost O(ND), which is hours at 1M tokens with 10 % edits.
func benchSkip(alg Algorithm, n int, rate float64) bool {
	edits := float64(n) * rate
	switch alg {
	case Myers:
		return edits > 2_500
	case MyersLinearSpace:
		return edits > 20_000
	case Reference:
		return n*n > MaxReferenceCells
	}
	return false
}

// BenchmarkDiffAlgo compares every Algorithm on the same inputs:
// go test -run '^$' -bench DiffAlgo ./diff. Metrics: tokens/s (old+new),
// script-tokens (edit distance realised; lower is closer to minimal) and
// hunks (at context 3; fewer usually reads better).
func BenchmarkDiffAlgo(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000, 1_000_000} {
		for _, rate := range []float64{0.001, 0.01, 0.1} {
			r := core.NewSplitMix64(uint64(n) ^ math.Float64bits(rate))
			old := benchSeq(r, n)
			nw := benchEdits(r, old, rate)
			for _, alg := range Algorithms() {
				name := fmt.Sprintf("%s/%s/%g%%", alg, sizeName(n), rate*100)
				b.Run(name, func(b *testing.B) {
					if benchSkip(alg, n, rate) {
						b.Skip("outside the algorithm's practical bounds")
					}
					b.ReportAllocs()
					var res Result
					for b.Loop() {
						var err error
						if res, err = Diff(old, nw, Options{Algorithm: alg}); err != nil {
							b.Fatal(err)
						}
					}
					b.ReportMetric(float64(len(old)+len(nw))*float64(b.N)/b.Elapsed().Seconds(), "tokens/s")
					b.ReportMetric(float64(res.Script.Distance()), "script-tokens")
					b.ReportMetric(float64(len(Hunks(res.Script, DefaultContext))), "hunks")
				})
			}
		}
	}
}

func sizeName(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%dM", n/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%dk", n/1_000)
	}
	return fmt.Sprint(n)
}

// BenchmarkDiffCapped compares MyersLinearSpace with and without the
// default 10,000-edit cap on an input whose distance is far above it.
func BenchmarkDiffCapped(b *testing.B) {
	r := core.NewSplitMix64(5)
	old := benchSeq(r, 200_000)
	nw := benchEdits(r, old, 0.1)
	for _, c := range []struct {
		name string
		cap  int
	}{{"uncapped", 0}, {"cap10000", DefaultMaxEditDistance}, {"cap1000", 1_000}} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			var res Result
			for b.Loop() {
				res, _ = Diff(old, nw, Options{Algorithm: MyersLinearSpace, MaxEditDistance: c.cap})
			}
			b.ReportMetric(float64(len(old)+len(nw))*float64(b.N)/b.Elapsed().Seconds(), "tokens/s")
			capped := 0.0
			if res.Capped {
				capped = 1
			}
			b.ReportMetric(capped, "capped")
		})
	}
}

// BenchmarkDiffBounded runs the §5 fallback chain on a 1 MiB text with
// default bounds (token Myers, capped) and with a small edit cap (falls
// back to histogram on lines).
func BenchmarkDiffBounded(b *testing.B) {
	r := core.NewSplitMix64(6)
	var sb strings.Builder
	for sb.Len() < 1<<20 {
		fmt.Fprintf(&sb, "const v%d = fetch('/api/%d');\n", core.IntN(r, 5000), core.IntN(r, 100))
	}
	old := sb.String()
	lines := SplitLines([]byte(old))
	for i := 0; i < len(lines); i += 97 {
		lines[i] = "changed();\n"
	}
	nw := strings.Join(lines, "")
	for _, c := range []struct {
		name string
		b    Bounds
	}{{"default", Bounds{}}, {"cap100", Bounds{MaxEditDistance: 100}}} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(old) + len(nw)))
			var res BoundedResult
			for b.Loop() {
				res, _ = BoundedDiff([]byte(old), []byte(nw), c.b)
			}
			b.ReportMetric(float64(res.Level), "level")
		})
	}
}

// BenchmarkDiffJSON diffs a large document (an array of 20,000 records)
// with 1 % of records changed, added or removed.
func BenchmarkDiffJSON(b *testing.B) {
	r := core.NewSplitMix64(7)
	rec := func(i int) Value {
		return ObjectValue(
			Member{Key: "id", Value: NumberValue(fmt.Sprint(i))},
			Member{Key: "host", Value: StringValue(fmt.Sprintf("h%d.example.com", i))},
			Member{Key: "ports", Value: ArrayValue(NumberValue("80"), NumberValue("443"))},
		)
	}
	var ai, bi []Value
	for i := range 20_000 {
		ai = append(ai, rec(i))
		switch core.IntN(r, 300) {
		case 0:
			v := rec(i)
			v.Members[1].Value = StringValue("moved.example.com")
			bi = append(bi, v)
		case 1:
		case 2:
			bi = append(bi, rec(i), rec(100_000+i))
		default:
			bi = append(bi, rec(i))
		}
	}
	a, bv := ArrayValue(ai...), ArrayValue(bi...)
	size := len(EncodeJSON(a)) + len(EncodeJSON(bv))
	b.ReportAllocs()
	b.SetBytes(int64(size))
	var ops []PatchOp
	for b.Loop() {
		ops = JSONDiff(a, bv, JSONOptions{})
	}
	b.ReportMetric(float64(len(ops)), "ops")
}

// BenchmarkDiffTree runs Zhang–Shasha on two DOM-like trees of 400 nodes.
func BenchmarkDiffTree(b *testing.B) {
	r := core.NewSplitMix64(8)
	x, y := randTree(r, 400, 8), randTree(r, 400, 8)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := TreeDiff(x, y, TreeOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}
