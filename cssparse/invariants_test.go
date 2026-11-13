package cssparse

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// fragments drive the generator toward every tokenizer state.
var fragments = []string{
	"a", "Z", "-", "--", "_", "0", "12", "1.5", ".5", "+", "1e3", "2E-2", "%", "px",
	"e", "u", "U+1?", "+.", " ", "\t", "\n", "\r\n", "\r", "\f", "\x00", "\xff",
	"\xc3", "é", " ", "\\", "\\41 ", "\\\n", "\\0", "'", "\"", "url(", "url( ",
	"URL(", "(", ")", "[", "]", "{", "}", ";", ":", ",", "@", "@media", "@import",
	"@font-face", "@namespace", "#", "#a", "#1", "/*", "*/", "/", "*", "<!--", "-->",
	"<", "!", "!important", "image-set(", "src", "src(", "~=", "|", "||", "^=", "$",
	"=", "&", ">",
}

func genCSS(r core.Rng, maxParts int) []byte {
	n := core.IntN(r, maxParts) + 1
	var b []byte
	for i := 0; i < n; i++ {
		if core.IntN(r, 10) == 0 {
			b = append(b, byte(r.Uint64()))
			continue
		}
		b = append(b, fragments[core.IntN(r, len(fragments))]...)
	}
	return b
}

func checkTiling(t testing.TB, src []byte, toks []Token, limit int) {
	t.Helper()
	pos := int32(0)
	for i, tk := range toks {
		if tk.Start != pos || tk.End <= tk.Start {
			t.Fatalf("token %d (%s) span [%d,%d) breaks tiling at %d for %q", i, tk.Kind, tk.Start, tk.End, pos, src)
		}
		pos = tk.End
	}
	want := len(src)
	if want > limit {
		want = int(pos) // truncated input: tiling covers the read prefix
		if want > limit || want < limit-3 {
			t.Fatalf("truncated read ends at %d, limit %d", want, limit)
		}
	}
	if int(pos) != want {
		t.Fatalf("tokens end at %d, input is %d bytes: %q", pos, want, src)
	}
	var raw []byte
	for _, tk := range toks {
		raw = append(raw, src[tk.Start:tk.End]...)
	}
	if !bytes.Equal(raw, src[:want]) {
		t.Fatalf("raw re-serialization differs from input %q", src)
	}
}

// TestCSSInv01_TokenTiling: token spans are non-empty, contiguous and
// cover the input exactly once.
func TestCSSInv01_TokenTiling(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases(); i++ {
		src := genCSS(r, 40)
		for _, legacy := range []bool{false, true} {
			toks, _ := Tokenize(src, Options{LegacyTokens: legacy})
			checkTiling(t, src, toks, DefaultMaxInputBytes)
		}
	}
}

// checkTree verifies arena links and spans of every node reachable from
// root.
func checkTree(t testing.TB, d *Document) {
	t.Helper()
	n := int32(len(d.Src))
	seen := make([]bool, len(d.Nodes))
	type frame struct {
		id    NodeID
		depth int
	}
	stack := []frame{{d.Root, 0}}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[f.id] {
			t.Fatalf("node %d reached twice", f.id)
		}
		seen[f.id] = true
		nd := &d.Nodes[f.id]
		if nd.Start < 0 || nd.End < nd.Start || nd.End > n {
			t.Fatalf("node %d (%s) span [%d,%d) outside [0,%d]", f.id, nd.Kind, nd.Start, nd.End, n)
		}
		prevEnd := nd.Start
		if nd.Token >= 0 && nd.FirstChild != NoNode && nd.Kind != NodeDeclaration {
			prevEnd = d.Tokens[nd.Token].End
		}
		last := NoNode
		for c := nd.FirstChild; c != NoNode; c = d.Nodes[c].NextSibling {
			cn := &d.Nodes[c]
			if cn.Parent != f.id {
				t.Fatalf("child %d of %d has parent %d", c, f.id, cn.Parent)
			}
			if cn.Start < prevEnd || cn.End > nd.End {
				t.Fatalf("child %d [%d,%d) out of order in parent %d [%d,%d) prev end %d", c, cn.Start, cn.End, f.id, nd.Start, nd.End, prevEnd)
			}
			prevEnd = cn.End
			last = c
			stack = append(stack, frame{c, f.depth + 1})
		}
		if last != nd.LastChild {
			t.Fatalf("node %d LastChild %d, list ends at %d", f.id, nd.LastChild, last)
		}
		if nd.Block != NoNode && nd.Kind != NodeBlockContents {
			b := &d.Nodes[nd.Block]
			if b.Parent != f.id || (b.Kind != NodeSimpleBlock && !(b.Kind == NodeError && b.Error == DiagDepthLimit)) {
				t.Fatalf("rule %d block %d malformed", f.id, nd.Block)
			}
			if b.End > nd.End || b.Start < prevEnd {
				t.Fatalf("rule %d block span [%d,%d) outside rule [%d,%d)", f.id, b.Start, b.End, nd.Start, nd.End)
			}
			stack = append(stack, frame{nd.Block, f.depth + 1})
		}
	}
}

var entryPoints = []func([]byte, Options) *Document{
	ParseStylesheet, ParseRuleList, ParseDeclarationList, ParseBlockContentsSource,
	ParseComponentValues, ParseRule, ParseDeclaration, ParseComponentValue,
}

// TestCSSInv02_Totality: every entry point accepts any byte string and
// yields a well-formed arena tree; URL extraction never fails.
func TestCSSInv02_Totality(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases(); i++ {
		src := genCSS(r, 60)
		opts := Options{LegacyTokens: i%2 == 0, MaxDepth: core.IntN(r, 6) + 1}
		for _, parse := range entryPoints {
			d := parse(src, opts)
			checkTree(t, d)
			res := d.URLs()
			checkTree(t, d)
			for _, u := range res.Refs {
				if u.Start < 0 || u.End > int32(len(src)) || u.Start >= u.End {
					t.Fatalf("url ref span [%d,%d) invalid", u.Start, u.End)
				}
			}
		}
	}
}

type tokKey struct {
	Kind   TokenKind
	Flags  TokenFlags
	Value  string
	Number float64
}

func keys(toks []Token) []tokKey {
	var out []tokKey
	for _, tk := range toks {
		if tk.Kind == TokenComment {
			continue
		}
		k := tokKey{Kind: tk.Kind, Flags: tk.Flags &^ FlagUnterminated, Value: tk.Value, Number: tk.Number}
		if n := len(out); n > 0 && k.Kind == TokenWhitespace && out[n-1].Kind == TokenWhitespace {
			continue
		}
		if k.Kind == TokenBadString || k.Kind == TokenBadURL {
			k.Value = ""
		}
		out = append(out, k)
	}
	return out
}

// TestCSSInv03_Reserialization: the raw spans reproduce the input
// byte-for-byte, and the value-based Serialize re-tokenizes to the same
// token kinds, flags, values and numbers (comments dropped). Inputs with a
// bad-string are excluded from the second half: its newline becomes a
// whitespace token on re-tokenization.
func TestCSSInv03_Reserialization(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases(); i++ {
		src := genCSS(r, 40)
		checkReserialization(t, src)
	}
}

func checkReserialization(t testing.TB, src []byte) {
	t.Helper()
	toks, _ := Tokenize(src, Options{})
	checkTiling(t, src, toks, DefaultMaxInputBytes)
	for _, tk := range toks {
		if tk.Kind == TokenBadString {
			return
		}
	}
	out := Serialize(toks)
	again, _ := Tokenize(out, Options{})
	a, b := keys(toks), keys(again)
	if len(a) != len(b) {
		t.Fatalf("serialize(%q) = %q: %d tokens, re-tokenized %d\n%v\n%v", src, out, len(a), len(b), a, b)
	}
	for j := range a {
		if a[j] != b[j] && !(a[j].Number != a[j].Number && b[j].Number != b[j].Number) {
			t.Fatalf("serialize(%q) = %q: token %d %+v became %+v", src, out, j, a[j], b[j])
		}
	}
}

// adversarial returns inputs of about n bytes that stress one state each.
func adversarial(n int) map[string][]byte {
	rep := func(s string) []byte { return bytes.Repeat([]byte(s), n/len(s)+1)[:n] }
	return map[string][]byte{
		"open-parens":    rep("("),
		"functions":      rep("a("),
		"escapes":        rep("\\41"),
		"unclosed-url":   append([]byte("url("), rep("a")...),
		"bad-url":        append([]byte("url(a b"), rep("\\)")...),
		"comment-starts": rep("/*/"),
		"strings":        rep("'\\\n"),
		"numbers":        rep("1.1e1"),
		"nested-rules":   rep("a{"),
		"at-rules":       rep("@a{"),
		"declarations":   []byte("a{" + strings.Repeat("b:c;", n/4) + "}"),
		"ws":             rep(" \r\n"),
		"dashes":         rep("-"),
		"urls":           rep("url(x)"),
	}
}

// TestCSSInv04_LinearSteps: the tokenizer's state-machine step count is at
// most 4 per input byte plus a constant, on random and adversarial input,
// and full parsing plus URL extraction allocates nodes linearly.
func TestCSSInv04_LinearSteps(t *testing.T) {
	r := testkit.Rng(t)
	check := func(name string, src []byte) {
		tk := tokenizeString(string(src), Options{LegacyTokens: true})
		if tk.steps > 4*len(src)+8 {
			t.Fatalf("%s: %d steps for %d bytes", name, tk.steps, len(src))
		}
		d := ParseStylesheet(src, Options{})
		d.URLs()
		if len(d.Nodes) > (DefaultMaxDepth+2)*(len(d.Tokens)+1) {
			t.Fatalf("%s: %d nodes for %d tokens", name, len(d.Nodes), len(d.Tokens))
		}
	}
	for i := 0; i < testkit.Cases(); i++ {
		check("random", genCSS(r, 80))
	}
	for name, src := range adversarial(1 << 16) {
		check(name, src)
	}
}

// TestCSSInv05_LosslessComponentTree: in a component-value-list parse
// without a depth limit hit, every non-comment token is exactly one
// node's Token or one block's CloseToken.
func TestCSSInv05_LosslessComponentTree(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases(); i++ {
		src := genCSS(r, 60)
		d := ParseComponentValues(src, Options{LegacyTokens: i%2 == 1})
		used := make([]int, len(d.Tokens))
		var walk func(NodeID)
		walk = func(id NodeID) {
			n := &d.Nodes[id]
			if n.Token >= 0 {
				used[n.Token]++
			}
			if n.CloseToken >= 0 {
				used[n.CloseToken]++
			}
			for c := n.FirstChild; c != NoNode; c = d.Nodes[c].NextSibling {
				walk(c)
			}
		}
		walk(d.Root)
		for j, tk := range d.Tokens {
			want := 1
			if tk.Kind == TokenComment {
				want = 0
			}
			if used[j] != want {
				t.Fatalf("%q: token %d (%s) used %d times", src, j, tk.Kind, used[j])
			}
		}
	}
}

// TestCSSInv06_DepthBound: no block or function in any tree is nested
// deeper than MaxDepth, including blocks re-parsed by URL extraction, and
// exceeding the bound records DiagDepthLimit.
func TestCSSInv06_DepthBound(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases(); i++ {
		max := core.IntN(r, 8) + 1
		src := genCSS(r, 60)
		if i%3 == 0 {
			src = append(bytes.Repeat([]byte("a{(["), core.IntN(r, 5)), src...)
		}
		d := ParseStylesheet(src, Options{MaxDepth: max})
		d.URLs()
		deepest := 0
		for id := range d.Nodes {
			if k := d.Nodes[id].Kind; k == NodeSimpleBlock || k == NodeFunction {
				deepest = max0(deepest, d.depth(NodeID(id)))
			}
		}
		if deepest > max {
			t.Fatalf("%q: depth %d > %d", src, deepest, max)
		}
		// Count the true nesting with an unbounded parse.
		full := ParseComponentValues(src, Options{MaxDepth: 1 << 30})
		trueDepth := 0
		for id := range full.Nodes {
			if k := full.Nodes[id].Kind; k == NodeSimpleBlock || k == NodeFunction {
				trueDepth = max0(trueDepth, full.depth(NodeID(id)))
			}
		}
		cv := ParseComponentValues(src, Options{MaxDepth: max})
		hit := false
		for _, dg := range cv.Diags {
			hit = hit || dg.Code == DiagDepthLimit
		}
		if hit != (trueDepth > max) {
			t.Fatalf("%q: true depth %d, max %d, depth diag %v", src, trueDepth, max, hit)
		}
	}
}

func max0(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// TestCSSInv07_EveryURLTokenReported: every <url-token> of a stylesheet
// that is not inside an elided block appears in URLs() with its span, and
// the references are in source order.
func TestCSSInv07_EveryURLTokenReported(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases(); i++ {
		src := genCSS(r, 60)
		d := ParseStylesheet(src, Options{})
		res := d.URLs()
		if res.Grade != GradeComplete {
			continue
		}
		reported := map[int32]bool{}
		for j, u := range res.Refs {
			if j > 0 && res.Refs[j-1].Start > u.Start {
				t.Fatalf("refs out of order")
			}
			if u.Form == URLFormToken {
				reported[u.Start] = true
			}
		}
		for _, tk := range d.Tokens {
			if tk.Kind == TokenURL && !reported[tk.Start] {
				t.Fatalf("%q: url token at %d (%q) not reported", src, tk.Start, tk.Value)
			}
		}
	}
}

// TestCSSInv08_LimitsGradePartial: a result is Partial exactly when a
// limit (input size, depth, URL count) was hit, never Complete then.
func TestCSSInv08_LimitsGradePartial(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases(); i++ {
		src := genCSS(r, 60)
		opts := Options{MaxDepth: core.IntN(r, 4) + 1, MaxInputBytes: core.IntN(r, len(src)+8) + 1, MaxURLs: core.IntN(r, 3) + 1}
		res := ExtractURLs(src, opts)
		hit := false
		for _, dg := range res.Diags {
			hit = hit || dg.Code.IsLimit()
		}
		if hit && res.Grade != GradePartial {
			t.Fatalf("%q %+v: limit hit but grade %s", src, opts, res.Grade)
		}
		if !hit && res.Grade != GradeComplete {
			t.Fatalf("%q: no limit but grade %s", src, res.Grade)
		}
		if len(src) > opts.MaxInputBytes && !hit {
			t.Fatalf("oversized input not flagged")
		}
		if len(res.Refs) > opts.MaxURLs {
			t.Fatalf("%d refs over cap %d", len(res.Refs), opts.MaxURLs)
		}
	}
}

func ExampleExtractURLs() {
	res := ExtractURLs([]byte(`@import "base.css"; a { background: url(bg.png) }`), Options{})
	for _, u := range res.Refs {
		fmt.Println(u.URL, u.Form, u.Context)
	}
	fmt.Println(res.Grade)
	// Output:
	// base.css string import
	// bg.png token value
	// complete
}
