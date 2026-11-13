package jsparse

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// TestRegress_SurrogatePairBracedEscape: "\uD83D\u{DE00}" is the same
// string as "😀" (the same two UTF-16 code units), so both must
// get the same cooked value and the same canonical JSON. The lexer used to
// combine a surrogate pair only when the second escape had four hex
// digits, leaving two WTF-8 surrogates for the braced form; the canonical
// JSON then differed, and printing then re-parsing changed the tree
// (JS-INV-03).
func TestRegress_SurrogatePairBracedEscape(t *testing.T) {
	forms := []string{`"😀"`, `"\uD83D\u{DE00}"`, `"\u{D83D}\u{DE00}"`, `"\u{1F600}"`, "`\\uD83D\\u{DE00}`"}
	var want []byte
	for _, f := range forms {
		p, err := Parse([]byte("x = "+f), Options{})
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		lit := p.Nodes[p.Nodes[p.Nodes[p.List(p.Nodes[p.Root].List)[0]].A].B]
		var got string
		if lit.Kind == KTemplateLiteral {
			got = p.Strings[p.Nodes[p.List(lit.List)[0]].Str]
		} else {
			got = p.Strings[lit.Str]
		}
		if got != "\U0001F600" {
			t.Errorf("%s: cooked value %q, want U+1F600", f, got)
		}
		if lit.Kind == KLiteral {
			j := p.JSON(JSONOptions{})
			if want == nil {
				want = j
			} else if !bytes.Equal(j, want) {
				t.Errorf("%s: canonical JSON\n%s\nwant\n%s", f, j, want)
			}
		}
		p2, err := Parse(Print(p, PrintOptions{}), Options{})
		if err != nil || !bytes.Equal(p2.JSON(JSONOptions{}), p.JSON(JSONOptions{})) {
			t.Errorf("%s: print/reparse changed the tree (err %v)", f, err)
		}
	}
	// A lone surrogate stays one.
	p, err := Parse([]byte(`x = "\uD83D\u{41}"`), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(p.JSON(JSONOptions{}), []byte(`"\ud83dA"`)) {
		t.Errorf("lone surrogate: %s", p.JSON(JSONOptions{}))
	}
}

// TestRegress_RegExpQuantifierHugeBounds: {n,m} with n > m is an early
// error however many digits the bounds have. The validator used to
// saturate both bounds near 2^30, so {20000000001,3000000000} (n is the
// larger) compared as in order and was accepted.
func TestRegress_RegExpQuantifierHugeBounds(t *testing.T) {
	bad := []string{`/a{20000000001,3000000000}/`, `/a{3,2}/`, `/a{0010,9}/`, `/a{99999999999999999999,99999999999999999998}/u`}
	for _, src := range bad {
		if _, err := Parse([]byte(src), Options{}); err == nil {
			t.Errorf("%s: accepted", src)
		}
	}
	good := []string{`/a{3000000000,20000000001}/`, `/a{2,2}/`, `/a{0009,10}/`, `/a{5,}/`, `/a{99999999999999999998,99999999999999999999}/u`}
	for _, src := range good {
		if _, err := Parse([]byte(src), Options{}); err != nil {
			t.Errorf("%s: %v", src, err)
		}
	}
}

// TestRegress_SubscriptChainDepth: member, call and tagged-template
// chains count against MaxDepth. A chain of millions of `.a` used to
// parse (the subscript loop is iterative) and then overflow the
// goroutine stack in the recursive JSON writer and printer, a fatal
// error that no recover can catch.
func TestRegress_SubscriptChainDepth(t *testing.T) {
	_, err := Parse([]byte("x"+strings.Repeat(".a", DefaultMaxDepth+10)), Options{})
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeTooDeep {
		t.Fatalf("long member chain: err %v, want too-deep", err)
	}
	for _, src := range []string{"f" + strings.Repeat("()", DefaultMaxDepth+10), "f" + strings.Repeat("``", DefaultMaxDepth+10)} {
		if _, err := Parse([]byte(src), Options{}); !errors.As(err, &e) || e.Code != CodeTooDeep {
			t.Fatalf("long chain %.10q...: err %v, want too-deep", src, err)
		}
	}
	// Within the bound a chain still parses, prints and serializes.
	src := "x" + strings.Repeat(".a", DefaultMaxDepth/2) + "()"
	p, err := Parse([]byte(src), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := Print(p, PrintOptions{Minify: true}); string(got) != src+";" {
		t.Fatalf("print: %.40q...", got)
	}
	_ = p.JSON(JSONOptions{})
	// Tolerant mode reports the limit and still returns a program.
	p, err = Parse([]byte("x"+strings.Repeat(".a", 3*DefaultMaxDepth)+";y"), Options{Tolerant: true})
	if err != nil || len(p.Diagnostics) == 0 || p.Diagnostics[0].Code != CodeTooDeep {
		t.Fatalf("tolerant: err %v diags %v", err, p.Diagnostics)
	}
	_ = p.JSON(JSONOptions{})
}

// TestRegress_RegExpClassLegacyOctalRange: without u or v, \1-\7 inside a
// class are LegacyOctalEscapeSequences of up to three digits (Annex B
// B.1.2), so [\11-\12] is the valid range U+0009-U+000A. The validator
// consumed one digit per escape, read the class as \1, 1-\1, 2 and
// rejected the pattern.
func TestRegress_RegExpClassLegacyOctalRange(t *testing.T) {
	for _, src := range []string{`/[\11-\12]/`, `/[\1-\7]/`, `/[\101-\132]/`, `/[\8-\9]/`, `/[\47-\50]/`} {
		if _, err := Parse([]byte(src), Options{}); err != nil {
			t.Errorf("%s: %v", src, err)
		}
	}
	for _, src := range []string{`/[\12-\11]/`, `/[\132-\101]/`, `/[\11-\12]/u`} {
		if _, err := Parse([]byte(src), Options{}); err == nil {
			t.Errorf("%s: accepted", src)
		}
	}
}
