package jsparse

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// Generated minified bundles (seed 1) of about 100 KB, 1 MB and 10 MB.
var bundleSizes = []struct {
	name string
	size int
}{{"100KB", 100 << 10}, {"1MB", 1 << 20}, {"10MB", 10 << 20}}

var bundles sync.Map // size → []byte

func bundle(size int) []byte {
	if b, ok := bundles.Load(size); ok {
		return b.([]byte)
	}
	b := genBundle(1, size)
	bundles.Store(size, b)
	return b
}

// goalSequence returns the goal the parser chose for each significant
// token of src, so the lexer benchmark lexes exactly the parser's stream.
func goalSequence(tb testing.TB, src []byte) []Goal {
	prog, err := Parse(src, Options{Tokens: true})
	if err != nil {
		tb.Fatal(err)
	}
	var goals []Goal
	for _, t := range prog.Tokens {
		if !t.Kind.IsTrivia() {
			goals = append(goals, t.Goal)
		}
	}
	return goals
}

func BenchmarkJSLex(b *testing.B) {
	for _, s := range bundleSizes {
		b.Run(s.name, func(b *testing.B) {
			src := bundle(s.size)
			goals := goalSequence(b, src)
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			for b.Loop() {
				l := NewLexer(src, false)
				for _, g := range goals {
					l.Next(g)
				}
			}
		})
	}
}

func BenchmarkJSParse(b *testing.B) {
	for _, s := range bundleSizes {
		b.Run(s.name, func(b *testing.B) {
			src := bundle(s.size)
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := Parse(src, Options{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// nonASCIIBundle is a bundle whose identifiers are mostly non-ASCII, the
// input where identifier classification matters.
func nonASCIIBundle() []byte {
	var out []byte
	for i := 0; len(out) < 1<<20; i++ {
		n := strconv.Itoa(i)
		out = append(out, "var été"+n+"=λx"+n+"+Δ"+n+"*données"+n+";function 名前"+n+"(π){return π.значение}"...)
	}
	return out
}

// algoCandidates lists every selectable configuration.
var algoCandidates = []struct {
	name string
	alg  Algorithms
}{
	{"default", Algorithms{}},
	{"keyword-map", Algorithms{Keywords: KeywordMap}},
	{"keyword-perfect-hash", Algorithms{Keywords: KeywordPerfectHash}},
	{"bmp-bitmap", Algorithms{IdentClass: IdentBMPBitmap}},
	{"ascii-branches", Algorithms{ASCIIClass: ASCIIBranches}},
	{"cascade", Algorithms{Binary: BinaryCascade}},
	{"all-alternatives", Algorithms{Keywords: KeywordMap, IdentClass: IdentBMPBitmap, ASCIIClass: ASCIIBranches, Binary: BinaryCascade}},
	{"all-alternatives-phash", Algorithms{Keywords: KeywordPerfectHash, IdentClass: IdentBMPBitmap, ASCIIClass: ASCIIBranches, Binary: BinaryCascade}},
}

// BenchmarkJSAlgo compares the candidates of each decision point
// (docs/CONVENTIONS.md "Pluggable algorithms"; README "Design choices
// measured"): BenchmarkJSAlgo/<decision>/<candidate>/<input>.
func BenchmarkJSAlgo(b *testing.B) {
	ascii := bundle(1 << 20)
	uni := nonASCIIBundle()
	run := func(b *testing.B, src []byte, alg Algorithms) {
		b.SetBytes(int64(len(src)))
		b.ReportAllocs()
		for b.Loop() {
			if _, err := Parse(src, Options{Algorithms: alg}); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.Run("keyword/switch/bundle-1MB", func(b *testing.B) { run(b, ascii, Algorithms{}) })
	b.Run("keyword/map/bundle-1MB", func(b *testing.B) { run(b, ascii, Algorithms{Keywords: KeywordMap}) })
	b.Run("keyword/perfect-hash/bundle-1MB", func(b *testing.B) { run(b, ascii, Algorithms{Keywords: KeywordPerfectHash}) })
	// The lexer alone (the parser's token goals replayed) isolates the
	// keyword lookup and ASCII classification, a few percent of a full
	// parse: every IdentifierName is classified and looked up once.
	goals := goalSequence(b, ascii)
	lex := func(b *testing.B, alg Algorithms) {
		b.SetBytes(int64(len(ascii)))
		b.ReportAllocs()
		for b.Loop() {
			l := NewLexer(ascii, false)
			l.alt = alg
			for _, g := range goals {
				l.Next(g)
			}
		}
	}
	b.Run("keyword-lex/switch/bundle-1MB", func(b *testing.B) { lex(b, Algorithms{}) })
	b.Run("keyword-lex/map/bundle-1MB", func(b *testing.B) { lex(b, Algorithms{Keywords: KeywordMap}) })
	b.Run("keyword-lex/perfect-hash/bundle-1MB", func(b *testing.B) { lex(b, Algorithms{Keywords: KeywordPerfectHash}) })
	b.Run("idclass/unicode-tables/nonascii-1MB", func(b *testing.B) { run(b, uni, Algorithms{}) })
	b.Run("idclass/bmp-bitmap/nonascii-1MB", func(b *testing.B) { run(b, uni, Algorithms{IdentClass: IdentBMPBitmap}) })
	b.Run("asciiclass/table/bundle-1MB", func(b *testing.B) { run(b, ascii, Algorithms{}) })
	b.Run("asciiclass/branches/bundle-1MB", func(b *testing.B) { run(b, ascii, Algorithms{ASCIIClass: ASCIIBranches}) })
	b.Run("asciiclass-lex/table/bundle-1MB", func(b *testing.B) { lex(b, Algorithms{}) })
	b.Run("asciiclass-lex/branches/bundle-1MB", func(b *testing.B) { lex(b, Algorithms{ASCIIClass: ASCIIBranches}) })
	b.Run("binary/climbing/bundle-1MB", func(b *testing.B) { run(b, ascii, Algorithms{}) })
	b.Run("binary/cascade/bundle-1MB", func(b *testing.B) { run(b, ascii, Algorithms{Binary: BinaryCascade}) })
}

// TestJSKeywordPerfectHash: the perfect hash gives every word of
// keywordNames its own slot and agrees with the switch on every word,
// every proper prefix and extension of one, and every ASCII and non-ASCII
// identifier of the corpus; the ASCII classifiers agree on all 128 bytes.
func TestJSKeywordPerfectHash(t *testing.T) {
	slots := map[uint64]string{}
	for _, w := range keywordNames {
		if o, dup := slots[keywordSlot(w)]; dup {
			t.Fatalf("%s and %s share slot %d", w, o, keywordSlot(w))
		}
		slots[keywordSlot(w)] = w
	}
	words := []string{"", "a", "x", "ifx", "Await", "constructors", "__proto_", "lett", "étais"}
	for _, w := range keywordNames {
		words = append(words, w, w[:len(w)-1], w+"s", "_"+w)
	}
	for _, c := range corpus(t, 200) {
		l := NewLexer(c.src, false)
		for tok := l.Next(GoalDiv); tok.Kind != TEOF; tok = l.Next(GoalDiv) {
			if tok.Kind == TIdentifier {
				words = append(words, tok.Value)
			}
		}
	}
	for _, w := range words {
		if a, b := lookupKeyword(w), perfectHashKeyword(w); a != b || keywordMap[w] != a {
			t.Fatalf("%q: switch %v, perfect hash %v, map %v", w, a, b, keywordMap[w])
		}
	}
	for c := range byte(0x80) {
		t1, b1 := Algorithms{}, Algorithms{ASCIIClass: ASCIIBranches}
		if t1.asciiIDStart(c) != b1.asciiIDStart(c) || t1.asciiIDPart(c) != b1.asciiIDPart(c) {
			t.Fatalf("byte %#x: table and branches disagree", c)
		}
	}
}

// TestJSAlgoEquivalent: every candidate gives the same tree on valid
// input and the same diagnostics on the fail and early suites.
func TestJSAlgoEquivalent(t *testing.T) {
	inputs := append(corpus(t, 200), genCase{src: nonASCIIBundle()[:4096]})
	for _, sub := range []string{"fail", "early"} {
		for _, f := range readSuiteDir(t, sub) {
			inputs = append(inputs, genCase{src: f.src, st: sourceTypeOf(f.name)})
		}
	}
	for i, c := range inputs {
		var want string
		for k, cand := range algoCandidates {
			prog, err := Parse(c.src, Options{SourceType: c.st, Tolerant: true, Algorithms: cand.alg})
			if err != nil {
				t.Fatal(err)
			}
			got := string(prog.JSON(JSONOptions{Positions: true})) + fmt.Sprint(prog.Diagnostics)
			if k == 0 {
				want = got
			} else if got != want {
				t.Fatalf("case %d: candidate %s differs from default\n%.300s", i, cand.name, c.src)
			}
		}
	}
}

// TestJSBundleDump writes acorn-compatible variants of the benchmark
// bundles to FF_JSPARSE_BUNDLE_DIR for
// conformance/jsparse/tools/acorn_bench.js and logs jsparse's throughput
// on the same files (skipped otherwise).
func TestJSBundleDump(t *testing.T) {
	dir := os.Getenv("FF_JSPARSE_BUNDLE_DIR")
	if dir == "" {
		t.Skip("set FF_JSPARSE_BUNDLE_DIR to dump the benchmark bundles")
	}
	// The vendored pass suite's scripts, each in a function wrapper: real
	// code that heuristic lexers also accept.
	var suite []byte
	for _, f := range readSuiteDir(t, "pass") {
		if sourceTypeOf(f.name) == SourceScript {
			suite = append(suite, "(function(){\n"...)
			suite = append(suite, f.src...)
			suite = append(suite, "\n})();\n"...)
		}
	}
	if _, err := Parse(suite, Options{}); err != nil {
		t.Fatal(err)
	}
	inputs := map[string][]byte{"suite-pass.js": suite}
	for _, s := range bundleSizes {
		inputs["bundle-"+s.name+".js"] = genBundleOpt(1, s.size, true)
	}
	names := []string{"suite-pass.js", "bundle-100KB.js", "bundle-1MB.js", "bundle-10MB.js"}
	for _, name := range names {
		src := inputs[name]
		if err := os.WriteFile(filepath.Join(dir, name), src, 0o644); err != nil {
			t.Fatal(err)
		}
		r := testing.Benchmark(func(b *testing.B) {
			b.SetBytes(int64(len(src)))
			for b.Loop() {
				Parse(src, Options{})
			}
		})
		t.Logf("%s\t%d B\tjsparse %.1f MB/s", name, len(src), float64(len(src))/1e6/(float64(r.NsPerOp())/1e9))
	}
}
