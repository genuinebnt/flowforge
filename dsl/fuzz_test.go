package dsl

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// seedCorpus adds the spec programs, the conformance sources and a few
// generated programs to a fuzz corpus.
func seedCorpus(f *testing.F) {
	for _, suite := range []string{"parse", "check"} {
		names, _ := filepath.Glob(filepath.Join(testkit.ConformanceDir(f, "dsl/"+suite), "*.ff"))
		for _, n := range names {
			if b, err := os.ReadFile(n); err == nil {
				f.Add(b)
			}
		}
	}
	r := core.NewSplitMix64(7)
	for i := 0; i < 8; i++ {
		f.Add([]byte(genProgram(r, 300)))
		f.Add([]byte(Format(genFile(r))))
	}
	f.Add([]byte(`"unterminated {`))
	f.Add([]byte("/* /* */"))
	f.Add([]byte(`"{"{"{x}"}"}"`))
}

// FuzzDSLLexer: lexing never panics, ends with EOF and yields ordered
// spans inside the source; a clean parse survives a format round trip.
func FuzzDSLLexer(f *testing.F) {
	seedCorpus(f)
	f.Fuzz(func(t *testing.T, src []byte) {
		toks, diags := Lex("fuzz.ff", src)
		if len(toks) == 0 || toks[len(toks)-1].Kind != TokEOF {
			t.Fatal("token stream does not end with EOF")
		}
		prev := 0
		for _, tk := range toks {
			if tk.Span.Start.Offset < prev || tk.Span.End.Offset < tk.Span.Start.Offset || tk.Span.End.Offset > len(src) {
				t.Fatalf("bad token span %+v (prev %d, len %d)", tk.Span, prev, len(src))
			}
			prev = tk.Span.End.Offset
		}
		for _, d := range diags {
			assertSpanInSource(t, src, d)
		}
		if len(diags) == 0 {
			fuzzRoundTrip(t, src)
		}
	})
}

// FuzzDSLParser: parsing never panics; on success, pretty-print and
// re-parse gives the same AST.
func FuzzDSLParser(f *testing.F) {
	seedCorpus(f)
	f.Fuzz(func(t *testing.T, src []byte) {
		fuzzRoundTrip(t, src)
	})
}

func fuzzRoundTrip(t *testing.T, src []byte) {
	t.Helper()
	file, diags := Parse("fuzz.ff", src)
	for _, d := range diags {
		assertSpanInSource(t, src, d)
	}
	if len(diags) > 0 {
		return
	}
	want, err := ASTJSON(file)
	if err != nil {
		t.Fatal(err)
	}
	out := Format(file)
	g, diags := Parse("fuzz2.ff", []byte(out))
	if len(diags) > 0 {
		t.Fatalf("formatted output does not parse: %v\n%s", diags[0], out)
	}
	got, _ := ASTJSON(g)
	if !bytes.Equal(want, got) {
		t.Fatalf("round trip changed the AST\n%s\nwant %s\ngot  %s", out, want, got)
	}
}

// FuzzDSLChecker: checking never panics and every diagnostic span lies
// inside the source.
func FuzzDSLChecker(f *testing.F) {
	seedCorpus(f)
	f.Fuzz(func(t *testing.T, src []byte) {
		for _, pol := range []Policy{{}, {MaxClass: core.ClassPassive, Known: true}} {
			a := Analyze(string(src), Options{Policy: pol, FileName: "fuzz.ff"})
			for _, d := range a.Diagnostics {
				assertSpanInSource(t, src, d)
			}
			if a.OK == HasErrors(a.Diagnostics) {
				t.Fatal("ok disagrees with diagnostics")
			}
		}
	})
}
