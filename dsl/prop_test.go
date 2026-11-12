package dsl

import (
	"bytes"
	"testing"

	"github.com/genuinebnt/flowforge/testkit"
)

// TestDSLProp_PrintParseRoundTrip: pretty-print then parse returns the
// same AST, for generated syntax trees and generated programs (spec 25.4).
func TestDSLProp_PrintParseRoundTrip(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases(); i++ {
		f := genFile(r)
		want, err := ASTJSON(f)
		if err != nil {
			t.Fatal(err)
		}
		out := Format(f)
		g, diags := Parse("rt.ff", []byte(out))
		if len(diags) > 0 {
			t.Fatalf("case %d: formatted tree does not parse: %v\n%s", i, diags[0], out)
		}
		got, _ := ASTJSON(g)
		if !bytes.Equal(want, got) {
			t.Fatalf("case %d: round trip changed the tree\n%s\nwant %s\ngot  %s", i, out, want, got)
		}
		// Formatting is idempotent.
		if again := Format(g); again != out {
			t.Fatalf("case %d: format not idempotent\n%s\n---\n%s", i, out, again)
		}
	}
}

// TestDSLProp_WellTypedChecks: generated well-typed programs check with
// no errors and no warnings, and still do after a format round trip.
func TestDSLProp_WellTypedChecks(t *testing.T) {
	r := testkit.Rng(t)
	n := testkit.Cases() / 4
	if n < 25 {
		n = 25
	}
	for i := 0; i < n; i++ {
		src := genProgram(r, 600)
		f, diags := Parse("gen.ff", []byte(src))
		if len(diags) > 0 {
			t.Fatalf("case %d: parse: %v\n%s", i, diags[0], src)
		}
		if _, diags := Check(f, Options{}); len(diags) > 0 {
			t.Fatalf("case %d: check: %v\n%s", i, diags, src)
		}
		formatted := Format(f)
		g, _ := Parse("gen2.ff", []byte(formatted))
		if _, diags := Check(g, Options{}); len(diags) > 0 {
			t.Fatalf("case %d: check after format: %v\n%s", i, diags, formatted)
		}
	}
}
