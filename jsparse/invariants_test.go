package jsparse

import (
	"errors"
	"os"
	"testing"

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
