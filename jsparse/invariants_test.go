package jsparse

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"

	"github.com/genuinebnt/flowforge/testkit"
)

// canonical returns the canonical JSON of a strict-mode parse, failing the
// test on error.
func canonical(t *testing.T, name string, src []byte, st SourceType) string {
	t.Helper()
	prog, err := Parse(src, Options{SourceType: st})
	if err != nil {
		t.Fatalf("%s: %v\n%s", name, err, src)
	}
	return string(prog.JSON(JSONOptions{}))
}

// checkPrintReparse asserts JS-INV-03 for one source in both layouts.
func checkPrintReparse(t *testing.T, name string, src []byte, st SourceType) {
	t.Helper()
	prog, err := Parse(src, Options{SourceType: st})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	want := string(prog.JSON(JSONOptions{}))
	for _, min := range []bool{false, true} {
		out := Print(prog, PrintOptions{Minify: min})
		again, err := Parse(out, Options{SourceType: st})
		if err != nil {
			t.Errorf("%s (minify=%v): printed output does not parse: %v\n--- printed:\n%.2000s", name, min, err, out)
			continue
		}
		if got := string(again.JSON(JSONOptions{})); got != want {
			t.Errorf("%s (minify=%v): AST changed\n--- printed:\n%.1500s\n--- want %.600s\n--- got  %.600s", name, min, out, want, got)
		}
	}
}

// TestJSInv03_PrintReparse: printing then re-parsing gives the same
// canonical AST, for every pass file of the conformance suite (and the
// generated programs, see TestJSInv03_PrintReparseGenerated).
func TestJSInv03_PrintReparse(t *testing.T) {
	for _, f := range readSuiteDir(t, "pass") {
		checkPrintReparse(t, f.name, f.src, sourceTypeOf(f.name))
	}
}

// genCorpus returns n generated programs (scripts and modules) from the
// test's seeded Rng.
func genCorpus(t *testing.T, n int) []genCase {
	t.Helper()
	r := testkit.Rng(t)
	out := make([]genCase, 0, n)
	for i := 0; i < n; i++ {
		module := i%3 == 2
		src, slashes := genProgram(r, module)
		st := SourceScript
		if module {
			st = SourceModule
		}
		out = append(out, genCase{src: src, st: st, slashes: slashes})
	}
	return out
}

type genCase struct {
	src     []byte
	st      SourceType
	slashes []bool
}

// TestJSGenValid checks that the program generator only produces valid
// programs (a precondition of the generated-corpus invariants).
func TestJSGenValid(t *testing.T) {
	for i, c := range genCorpus(t, testkit.Cases()) {
		if _, err := Parse(c.src, Options{SourceType: c.st}); err != nil {
			if out := os.Getenv("FF_GEN_OUT"); out != "" {
				os.WriteFile(out, c.src, 0o644)
			}
			t.Fatalf("case %d (%v): %v\n%s", i, c.st, err, errContext(c.src, err))
		}
	}
}

// errContext returns the source around a parse error, for failure messages.
func errContext(src []byte, err error) string {
	var e *Error
	if !errors.As(err, &e) {
		return string(src)
	}
	lo, hi := int(e.Span.Start)-120, int(e.Span.End)+40
	lo, hi = max(lo, 0), min(hi, len(src))
	return string(src[lo:e.Span.Start]) + "<<<HERE>>>" + string(src[e.Span.Start:hi])
}

// corpus is every input the invariants run on: the suite's pass files and
// generated programs.
func corpus(t *testing.T, generated int) []genCase {
	t.Helper()
	var out []genCase
	for _, f := range readSuiteDir(t, "pass") {
		out = append(out, genCase{src: f.src, st: sourceTypeOf(f.name)})
	}
	return append(out, genCorpus(t, generated)...)
}

// TestJSInv01_TokenTiling: token spans and trivia tile the input exactly,
// for valid programs and (tolerant mode) for every fail/early file.
func TestJSInv01_TokenTiling(t *testing.T) {
	for i, c := range corpus(t, testkit.Cases()/2) {
		prog, err := Parse(c.src, Options{SourceType: c.st, Tokens: true})
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if err := checkTiling(prog); err != nil {
			t.Fatalf("case %d: %v\n%.300s", i, err, c.src)
		}
	}
	for _, sub := range []string{"fail", "early"} {
		for _, f := range readSuiteDir(t, sub) {
			prog, _ := Parse(f.src, Options{SourceType: sourceTypeOf(f.name), Tokens: true, Tolerant: true})
			if err := checkTiling(prog); err != nil {
				t.Fatalf("%s/%s (tolerant): %v\n%s", sub, f.name, err, f.src)
			}
		}
	}
}

// mutate returns src with a few random byte edits (deletions, insertions
// of JavaScript-significant bytes, duplications).
func mutate(r interface{ Uint64() uint64 }, src []byte) []byte {
	const alphabet = "{}()[];,.=+-*/%<>!?:'\"`\\#@ \n\r\t$_aZ09x é\xff"
	out := append([]byte(nil), src...)
	for k := 1 + int(r.Uint64()%4); k > 0; k-- {
		if len(out) == 0 {
			out = append(out, alphabet[r.Uint64()%uint64(len(alphabet))])
			continue
		}
		i := int(r.Uint64() % uint64(len(out)))
		switch r.Uint64() % 3 {
		case 0:
			out = append(out[:i], out[i+1:]...)
		case 1:
			c := alphabet[r.Uint64()%uint64(len(alphabet))]
			out = append(out[:i], append([]byte{c}, out[i:]...)...)
		default:
			j := i + int(r.Uint64()%16)
			if j > len(out) {
				j = len(out)
			}
			out = append(out[:j], append(append([]byte(nil), out[i:j]...), out[j:]...)...)
		}
	}
	return out
}

// checkTolerant runs one tolerant parse and asserts JS-INV-01, -02 and
// -06 on the result.
func checkTolerant(src []byte, st SourceType) error {
	prog, err := Parse(src, Options{SourceType: st, Tokens: true, Tolerant: true})
	if err != nil {
		return fmt.Errorf("tolerant mode returned an error: %v", err)
	}
	if prog == nil {
		return fmt.Errorf("nil program")
	}
	if err := checkTiling(prog); err != nil {
		return err
	}
	if err := checkArena(prog, false); err != nil {
		return err
	}
	for _, d := range prog.Diagnostics {
		if d.Code == CodeNone || d.Code >= codeCount || d.Span.Start < 0 || d.Span.End > int32(len(src)) || d.Span.Start > d.Span.End {
			return fmt.Errorf("bad diagnostic %v", d)
		}
	}
	strictProg, serr := Parse(src, Options{SourceType: st})
	switch {
	case serr == nil && len(prog.Diagnostics) != 0:
		return fmt.Errorf("strict accepted but tolerant reported %v", prog.Diagnostics[0])
	case serr != nil && len(prog.Diagnostics) == 0:
		return fmt.Errorf("strict rejected (%v) but tolerant reported nothing", serr)
	case serr != nil:
		var e *Error
		if !errors.As(serr, &e) || e.Diagnostic != prog.Diagnostics[0] {
			return fmt.Errorf("strict error %v differs from first tolerant diagnostic %v", serr, prog.Diagnostics[0])
		}
	case strictProg == nil:
		return fmt.Errorf("strict mode returned neither program nor error")
	}
	_ = prog.JSON(JSONOptions{Positions: true, Raw: true})
	_ = Print(prog, PrintOptions{})
	return nil
}

// TestJSInv02_Totality: tolerant mode returns a program for any byte
// string (random bytes and mutated programs), never panics, keeps the
// tiling and arena invariants, and its first diagnostic is the strict-mode
// error (JS-INV-08).
func TestJSInv02_Totality(t *testing.T) {
	r := testkit.Rng(t)
	inputs := corpus(t, 200)
	for i := 0; i < testkit.Cases(); i++ {
		var src []byte
		if i%4 == 0 {
			src = make([]byte, r.Uint64()%64)
			for j := range src {
				src[j] = byte(r.Uint64())
			}
		} else {
			src = mutate(r, inputs[int(r.Uint64()%uint64(len(inputs)))].src)
		}
		st := SourceType(r.Uint64() % 2)
		if err := checkTolerant(src, st); err != nil {
			t.Fatalf("input %q (%v): %v", src, st, err)
		}
	}
}

// TestJSInv03_PrintReparseGenerated: JS-INV-03 on generated programs, in
// readable and minified layouts.
func TestJSInv03_PrintReparseGenerated(t *testing.T) {
	for i, c := range genCorpus(t, testkit.Cases()) {
		checkPrintReparse(t, fmt.Sprintf("generated %d", i), c.src, c.st)
		if t.Failed() {
			t.Fatalf("source:\n%s", c.src)
		}
	}
}

// genBundle builds a minified script bundle of at least size bytes from
// generated programs, each wrapped in its own function like a bundler's
// module wrapper.
func genBundle(seed uint64, size int) []byte { return genBundleOpt(seed, size, false) }

func genBundleOpt(seed uint64, size int, acornSafe bool) []byte {
	r := core.NewSplitMix64(seed)
	var b []byte
	for len(b) < size {
		src, _ := genProgramOpt(r, false, acornSafe)
		prog, err := Parse(src, Options{})
		if err != nil {
			continue
		}
		b = append(b, "!function(){"...)
		b = append(b, Print(prog, PrintOptions{Minify: true})...)
		b = append(b, "}();"...)
	}
	return b
}

// TestJSInv04_LinearTime: parsing a bundle of 4n bytes costs at most 1.6
// times (4 × the cost of n bytes) in time and in allocated bytes, measured
// as the best of five runs.
func TestJSInv04_LinearTime(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	small := genBundle(7, 256<<10)
	large := genBundle(7, 1<<20)
	measure := func(src []byte) (time.Duration, uint64) {
		best := time.Duration(1 << 62)
		var bytes uint64
		for k := 0; k < 5; k++ {
			var m0, m1 runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&m0)
			start := time.Now()
			if _, err := Parse(src, Options{}); err != nil {
				t.Fatal(err)
			}
			d := time.Since(start)
			runtime.ReadMemStats(&m1)
			if d < best {
				best = d
			}
			bytes = m1.TotalAlloc - m0.TotalAlloc
		}
		return best, bytes
	}
	ts, as := measure(small)
	tl, al := measure(large)
	scale := float64(len(large)) / float64(len(small))
	timeRatio := float64(tl) / (float64(ts) * scale)
	allocRatio := float64(al) / (float64(as) * scale)
	t.Logf("n=%d: %v %d B; 4n=%d: %v %d B; per-byte time ratio %.2f, alloc ratio %.2f", len(small), ts, as, len(large), tl, al, timeRatio, allocRatio)
	if timeRatio > 1.6 || allocRatio > 1.6 {
		t.Errorf("not linear: time ratio %.2f, alloc ratio %.2f (bound 1.6)", timeRatio, allocRatio)
	}
}

// slashKinds returns, in order, whether each `/`-initial token of a parse
// is a regular expression literal.
func slashKinds(prog *Program) []bool {
	var out []bool
	for _, t := range prog.Tokens {
		switch t.Kind {
		case TRegExp:
			out = append(out, true)
		case TSlash, TSlashEq:
			out = append(out, false)
		}
	}
	return out
}

// goalCases are hand-written ambiguous `/` contexts with their answers
// (r = RegularExpressionLiteral, d = division), one letter per `/` token.
var goalCases = []struct {
	src, want string
}{
	{"a / b / c", "dd"}, {"x = /re/g", "r"}, {"if (x) /re/.test(y)", "r"}, {"f(x) / 2", "d"},
	{"{} /re/", "r"}, {"({}) / 2", "d"}, {"x = {} / 1", "d"}, {"a\n/b/g", "dd"},
	{"function f(){ return /re/ }", "r"}, {"typeof /re/", "r"}, {"function* g(){ yield /re/ }", "r"},
	{"async function f(){ await /re/ }", "r"}, {"x++ / 2", "d"}, {"++/re/.lastIndex", "r"},
	{"`${a}` / 2", "d"}, {"`${/re/}`", "r"}, {"`${a / b}`", "d"}, {"a ? /x/ : /y/", "rr"},
	{"[/x/]", "r"}, {"this / 2", "d"}, {"void /x/", "r"}, {"x in /x/", "r"}, {"switch(a){case /x/: }", "r"},
	{"() => /x/", "r"}, {"let x = a\n/re/i.test(y)", "dd"}, {"a /= 2", "d"}, {"x = /=/", "r"},
	{"x = a /= 2 / 1", "dd"}, {"(a) / b", "d"}, {"a[0] / 1", "d"}, {"while (0) /x/", "r"},
	{"x = function(){} / 1", "d"}, {"function f(){} /x/", "r"}, {"class A {} /x/", "r"},
	{"x = class {} / 1", "d"}, {"do /x/; while (0)", "r"}, {"x = y => {} \n /z/", "r"},
	{"a = b\n/c/d.e", "dd"}, {"label: /x/", "r"}, {"new /x/()", "r"}, {"1 / 2 / 3", "dd"},
	{"x = `a${1}b` / `c${/d/}`", "dr"}, {"yield / 2", "d"}, {"await / 2", "d"}, {"let / 2", "d"},
	{"async / 2", "d"}, {"of / 2", "d"},
}

// TestJSInv05_LexicalGoals: the regex/division decision matches the
// grammar on the hand-written ambiguous contexts and on every `/` of the
// generated corpus (whose kind the generator recorded).
func TestJSInv05_LexicalGoals(t *testing.T) {
	for _, c := range goalCases {
		prog, err := Parse([]byte(c.src), Options{Tokens: true})
		if err != nil {
			t.Errorf("%q: %v", c.src, err)
			continue
		}
		var got []byte
		for _, k := range slashKinds(prog) {
			if k {
				got = append(got, 'r')
			} else {
				got = append(got, 'd')
			}
		}
		if string(got) != c.want {
			t.Errorf("%q: got %s want %s", c.src, got, c.want)
		}
	}
	for i, c := range genCorpus(t, testkit.Cases()) {
		prog, err := Parse(c.src, Options{SourceType: c.st, Tokens: true})
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		got := slashKinds(prog)
		if fmt.Sprint(got) != fmt.Sprint(c.slashes) {
			t.Fatalf("case %d: slash kinds %v, generator recorded %v\n%s", i, got, c.slashes, c.src)
		}
		for _, tk := range prog.Tokens {
			if tk.Kind == TRegExp && !tk.Goal.allowsRegExp() {
				t.Fatalf("case %d: regexp token lexed under %v", i, tk.Goal)
			}
		}
	}
}

// TestJSInv06_ArenaIntegrity: on every valid input, child ids are in
// range, children are not shared, parent spans contain child spans, holes
// appear only in arrays, and every node is reachable from the root.
func TestJSInv06_ArenaIntegrity(t *testing.T) {
	for i, c := range corpus(t, testkit.Cases()/2) {
		prog, err := Parse(c.src, Options{SourceType: c.st})
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if err := checkArena(prog, true); err != nil {
			t.Fatalf("case %d: %v\n%.400s", i, err, c.src)
		}
	}
}

// TestJSInv07_TolerantAgrees: on valid input tolerant mode produces the
// same tree as strict mode and no diagnostics; on invalid input (fail and
// early files) its first diagnostic equals the strict-mode error
// (JS-INV-08) and the tolerant tree satisfies JS-INV-01/06.
func TestJSInv07_TolerantAgrees(t *testing.T) {
	for i, c := range corpus(t, 100) {
		a, _ := Parse(c.src, Options{SourceType: c.st})
		b, err := Parse(c.src, Options{SourceType: c.st, Tolerant: true})
		if err != nil || len(b.Diagnostics) != 0 || string(a.JSON(JSONOptions{Positions: true})) != string(b.JSON(JSONOptions{Positions: true})) {
			t.Fatalf("case %d: tolerant differs (err %v, diags %v)", i, err, b.Diagnostics)
		}
	}
	for _, sub := range []string{"fail", "early"} {
		for _, f := range readSuiteDir(t, sub) {
			if err := checkTolerant(f.src, sourceTypeOf(f.name)); err != nil {
				t.Errorf("%s/%s: %v", sub, f.name, err)
			}
		}
	}
}

// TestJSInv09_CanonicalJSON: the canonical JSON is valid JSON whose
// objects have strictly sorted keys, and is identical across parses.
func TestJSInv09_CanonicalJSON(t *testing.T) {
	for i, c := range corpus(t, 100) {
		p1, _ := Parse(c.src, Options{SourceType: c.st})
		p2, _ := Parse(c.src, Options{SourceType: c.st})
		j1 := p1.JSON(JSONOptions{Positions: true, Raw: true})
		if string(j1) != string(p2.JSON(JSONOptions{Positions: true, Raw: true})) {
			t.Fatalf("case %d: JSON not deterministic", i)
		}
		if err := checkSortedJSON(j1); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
	}
}

// checkSortedJSON decodes b token by token and checks key order.
func checkSortedJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	type frame struct {
		obj     bool
		lastKey string
		expect  bool // next string token is a key
	}
	var stack []frame
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{':
				stack = append(stack, frame{obj: true, expect: true})
			case '[':
				stack = append(stack, frame{})
			default:
				stack = stack[:len(stack)-1]
				if n := len(stack); n > 0 && stack[n-1].obj {
					stack[n-1].expect = true
				}
			}
			continue
		}
		if n := len(stack); n > 0 && stack[n-1].obj {
			f := &stack[n-1]
			if f.expect {
				k := tok.(string)
				if f.lastKey != "" && k <= f.lastKey {
					return fmt.Errorf("key %q after %q", k, f.lastKey)
				}
				f.lastKey, f.expect = k, false
			} else {
				f.expect = true
			}
		}
	}
}

func newTestRng(t *testing.T) *core.SplitMix64 { return testkit.Rng(t) }
