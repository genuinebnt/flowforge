package cssparse

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

func fuzzSeeds(f *testing.F) {
	for _, s := range []string{
		"", "a{b:c}", "@import url(x.css);", "url( 'a' )", "url(a b)", "'a\nb", "\\", "/*",
		"@font-face{src:url(f.woff)}", "a{b:image-set('x' 1x)}", "1e3px 2.5% -.5", "<!-- -->",
		"#a #1 @-- u+1?", "a:b !important", "((((((", "\x00\xff\r\n\f",
	} {
		f.Add([]byte(s))
	}
}

// FuzzCSSTokenize asserts CSS-INV-01 (tiling), CSS-INV-03 (raw and value
// re-serialization) and CSS-INV-04 (step bound) on arbitrary input.
func FuzzCSSTokenize(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, src []byte) {
		for _, legacy := range []bool{false, true} {
			tk := tokenizeString(string(src), Options{LegacyTokens: legacy})
			checkTiling(t, src, tk.toks, DefaultMaxInputBytes)
			if tk.steps > 4*len(src)+8 {
				t.Fatalf("%d steps for %d bytes", tk.steps, len(src))
			}
		}
		checkReserialization(t, src)
	})
}

// FuzzCSSParse asserts CSS-INV-02 (totality, well-formed trees),
// CSS-INV-05 (lossless component tree), CSS-INV-06 (depth bound) and
// CSS-INV-08 (limits grade partial) on arbitrary input.
func FuzzCSSParse(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, src []byte) {
		for _, parse := range entryPoints {
			d := parse(src, Options{MaxDepth: 4})
			res := d.URLs()
			checkTree(t, d)
			for id := range d.Nodes {
				if k := d.Nodes[id].Kind; (k == NodeSimpleBlock || k == NodeFunction) && d.depth(NodeID(id)) > 4 {
					t.Fatalf("depth bound broken")
				}
			}
			hit := false
			for _, dg := range res.Diags {
				hit = hit || dg.Code.IsLimit()
			}
			if hit != (res.Grade == GradePartial) {
				t.Fatalf("limit %v but grade %s", hit, res.Grade)
			}
		}
		d := ParseComponentValues(src, Options{MaxDepth: 1 << 30})
		used := 0
		for _, n := range d.Nodes {
			if n.Token >= 0 {
				used++
			}
			if n.CloseToken >= 0 {
				used++
			}
		}
		nonComment := 0
		for _, tk := range d.Tokens {
			if tk.Kind != TokenComment {
				nonComment++
			}
		}
		if used != nonComment {
			t.Fatalf("component tree uses %d tokens of %d", used, nonComment)
		}
	})
}

// genStylesheet builds a realistic stylesheet of about n bytes.
func genStylesheet(n int) []byte {
	r := core.NewSplitMix64(0x435353) // "CSS"
	var b bytes.Buffer
	b.WriteString("@charset \"utf-8\";\n@import url(\"base.css\") screen;\n")
	for i := 0; b.Len() < n; i++ {
		switch core.IntN(r, 6) {
		case 0:
			fmt.Fprintf(&b, "@media (min-width: %dpx) {\n  .c%d > a:hover { color: #%06x; margin: %d.%dem auto }\n}\n",
				core.IntN(r, 2000), i, core.IntN(r, 1<<24), core.IntN(r, 9), core.IntN(r, 9))
		case 1:
			fmt.Fprintf(&b, "@font-face { font-family: f%d; src: url(/fonts/f%d.woff2) format(\"woff2\"), local(F%d) }\n", i, i, i)
		case 2:
			fmt.Fprintf(&b, "#id%d .x, .y[data-k=\"v%d\"] { background: url(img/%d.png) no-repeat, image-set(\"a%d.png\" 1x); /* c%d */ }\n", i, i, i, i, i)
		case 3:
			fmt.Fprintf(&b, ".n%d { & .m { transform: translate(%d%%, -%dpx) rotate(%ddeg) !important } }\n", i, core.IntN(r, 100), core.IntN(r, 100), core.IntN(r, 360))
		default:
			fmt.Fprintf(&b, "div.k%d{width:calc(100%% - %dpx);content:\"\\201C x\";--v:{a b}}\n", i, core.IntN(r, 500))
		}
	}
	return b.Bytes()
}

func benchSizes(b *testing.B, name string, fn func([]byte)) {
	for _, n := range []int{64 << 10, 256 << 10} {
		src := genStylesheet(n)
		b.Run(fmt.Sprintf("%s/%dKiB", name, n>>10), func(b *testing.B) {
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			for b.Loop() {
				fn(src)
			}
		})
	}
}

// BenchmarkCSS01_Tokenize measures the tokenizer at n and 4n.
func BenchmarkCSS01_Tokenize(b *testing.B) {
	benchSizes(b, "tokenize", func(src []byte) { Tokenize(src, Options{}) })
}

// BenchmarkCSS02_ParseStylesheet measures tokenizing plus parsing.
func BenchmarkCSS02_ParseStylesheet(b *testing.B) {
	benchSizes(b, "stylesheet", func(src []byte) { ParseStylesheet(src, Options{}) })
}

// BenchmarkCSS03_ExtractURLs measures the full URL extraction path.
func BenchmarkCSS03_ExtractURLs(b *testing.B) {
	benchSizes(b, "urls", func(src []byte) { ExtractURLs(src, Options{}) })
}

// perByte returns the best-of-five nanoseconds per byte of fn on src.
func perByte(src []byte, fn func([]byte)) float64 {
	best := time.Duration(1 << 62)
	for i := 0; i < 5; i++ {
		start := time.Now()
		fn(src)
		if d := time.Since(start); d < best {
			best = d
		}
	}
	return float64(best) / float64(len(src))
}

// linearWithin reports whether fn's per-byte time at big stays within
// factor of its per-byte time at small, retrying so one noisy sample on a
// shared machine does not fail the check.
func linearWithin(small, big []byte, factor float64, fn func([]byte)) (float64, float64, bool) {
	var a, b float64
	for attempt := 0; attempt < 3; attempt++ {
		a, b = perByte(small, fn), perByte(big, fn)
		if b <= factor*a+1 {
			return a, b, true
		}
	}
	return a, b, false
}

// TestCSSInv04_LinearTime is the measured half of CSS-INV-04. Allocation:
// URL extraction allocates at most 4.4x as often at 4n as at n
// (deterministic). Time: per-byte cost at 4n stays within 1.5x of n on
// generated stylesheets and 2.5x on adversarial shapes (the design's 20 %
// target is checked on the benchmark baselines; this guard catches
// quadratic behaviour without flaking).
func TestCSSInv04_LinearTime(t *testing.T) {
	small, large := genStylesheet(256<<10), genStylesheet(1<<20)
	fn := func(src []byte) { ExtractURLs(src, Options{}) }
	as := testing.AllocsPerRun(2, func() { fn(small) })
	al := testing.AllocsPerRun(2, func() { fn(large) })
	if al > 4.4*as*float64(len(large))/float64(4*len(small)) {
		t.Fatalf("allocations grew from %.0f to %.0f for 4x input", as, al)
	}
	if testing.Short() || raceEnabled {
		t.Skip("timing half skipped in -short and -race runs")
	}
	if a, b, ok := linearWithin(small, large, 1.5, fn); !ok {
		t.Fatalf("per-byte cost grew from %.2f ns at %d B to %.2f ns at %d B", a, len(small), b, len(large))
	}
	// Sizes start past the CPU caches so the ratio measures the algorithm,
	// not the memory hierarchy.
	for name, src := range adversarial(1 << 18) {
		if a, b, ok := linearWithin(src, adversarial(1 << 20)[name], 2.5, fn); !ok {
			t.Errorf("%s: per-byte cost grew from %.2f ns to %.2f ns", name, a, b)
		}
	}
}
