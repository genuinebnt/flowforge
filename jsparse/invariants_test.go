package jsparse

import (
	"testing"
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
