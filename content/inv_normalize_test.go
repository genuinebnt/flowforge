package content

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/jsparse"
	"github.com/genuinebnt/flowforge/testkit"
)

// corpus returns, per normalizer kind, the fixture documents plus n
// generated ones.
func corpus(t *testing.T, n int) map[Kind][][]byte {
	r := testkit.Rng(t)
	c := map[Kind][][]byte{}
	for _, v := range []string{"v1", "v2", "v3"} {
		c[KindHTML] = append(c[KindHTML], site(t, "spa", v+".index.html"))
		c[KindHeaders] = append(c[KindHeaders], HeaderBlock(headerFile(t, "spa", v+".headers.txt")))
	}
	for _, b := range append(spaBundles, "index-a1b2c3d4.js", "app-e5f6a7b8.js", "other-0f0f0f0f.js") {
		c[KindJS] = append(c[KindJS], site(t, "spa", b))
	}
	for range n {
		c[KindHTML] = append(c[KindHTML], []byte(genHTML(r)))
		c[KindJS] = append(c[KindJS], []byte(genJS(r)))
		c[KindCSS] = append(c[KindCSS], []byte(genCSS(r)))
		c[KindJSON] = append(c[KindJSON], []byte(genJSON(r, 0)))
		c[KindXML] = append(c[KindXML], []byte(genXML(r)))
		c[KindHeaders] = append(c[KindHeaders], []byte(genHeaders(r)))
		c[KindText] = append(c[KindText], []byte(genTextDoc(r)))
		c[KindTSText] = append(c[KindTSText], []byte(genTextDoc(r)))
		bin := make([]byte, core.IntN(r, 64))
		for i := range bin {
			bin[i] = byte(r.Uint64())
		}
		c[KindBinary] = append(c[KindBinary], bin)
	}
	return c
}

// CONTENT-INV-01: normalization is idempotent for every kind:
// Normalize(k, Normalize(k, x).Text).Text == Normalize(k, x).Text, on
// fixtures and generated (often malformed) documents, with and without a
// learned rule set.
func TestContentInv01_NormalizeIdempotent(t *testing.T) {
	learned := VolatilitySet{Version: 2, Rules: []VolatileRule{{Path: "html:attr:div.data-build"}, {Path: "json:/a0"}}}
	n := max(50, testkit.Cases()/5)
	for k, docs := range corpus(t, n) {
		for i, d := range docs {
			for _, opts := range []NormalizeOptions{{}, {Learned: learned}} {
				first := Normalize(k, d, opts)
				second := Normalize(k, first.Text, opts)
				if !bytes.Equal(first.Text, second.Text) {
					t.Fatalf("%s doc %d not idempotent\ninput: %q\nfirst:  %q\nsecond: %q", k, i, d, first.Text, second.Text)
				}
			}
		}
	}
}

// volatileVariant rewrites the SPA page with fresh volatile values: nonce,
// CSRF token, timestamps (epoch and ISO), cache-busting query, build
// hashes, and formatting (white space, attribute order, comments).
func volatileVariant(r core.Rng, page string) string {
	nonce := randToken(r, 16, alnumChars)
	repl := []string{
		"n0nc3AAA111bbb", nonce,
		"Zx81mQp2LkV0aT3nB7cRq9Wd", randToken(r, 24, alnumChars),
		"1762596000123", fmt.Sprint(1_700_000_000_000 + core.IntN(r, 99_999_999_999)),
		"1762596000", fmt.Sprint(1_700_000_000 + core.IntN(r, 99_999_999)),
		"8f14e45fceea167a5a36dedd4bea2543", randToken(r, 32, "0123456789abcdef") + "a1",
		"2026-11-08T10:00:00Z", fmt.Sprintf("2027-%02d-%02dT%02d:%02d:00Z", 1+core.IntN(r, 12), 1+core.IntN(r, 28), core.IntN(r, 24), core.IntN(r, 60)),
		"main.3f9a1c2b.js", "main." + randToken(r, 8, "0123456789abcdef") + ".js",
		"main.9e8d7c6b.css?v=100", "main." + randToken(r, 8, "0123456789abcdef") + ".css?v=" + randToken(r, 6, "0123456789"),
	}
	out := strings.NewReplacer(repl...).Replace(page)
	if core.IntN(r, 2) == 0 {
		out = strings.ReplaceAll(out, "><", ">\n   <")
	}
	if core.IntN(r, 2) == 0 {
		out = strings.Replace(out, `<script defer src=`, `<!-- deploy `+randToken(r, 5, alnumChars)+` --><script defer   src=`, 1)
	}
	if core.IntN(r, 2) == 0 {
		out = strings.Replace(out, `<input name="q" type="search">`, `<input type="search"   name="q" >`, 1)
	}
	return out
}

// minifierVariant re-formats a script the way a different minifier
// configuration would: minified or readable printing, single quotes.
func minifierVariant(r core.Rng, src []byte) []byte {
	prog, err := jsparse.Parse(src, jsparse.Options{})
	if err != nil {
		return src
	}
	out := jsparse.Print(prog, jsparse.PrintOptions{Minify: core.IntN(r, 2) == 0})
	if core.IntN(r, 2) == 0 {
		out = bytes.ReplaceAll(out, []byte(`"`), []byte(`'`))
		if _, err := jsparse.Parse(out, jsparse.Options{}); err != nil {
			out = jsparse.Print(prog, jsparse.PrintOptions{Minify: true})
		}
	}
	return out
}

// CONTENT-INV-02: normalization is invariant under the declared volatile
// transformations (nonce, CSRF token, timestamps, cache-busting query,
// build hashes, mark-up and minifier re-formatting) and NOT invariant under
// a planted semantic change, which the features always detect.
func TestContentInv02_VolatileInvariantSemanticDetected(t *testing.T) {
	r := testkit.Rng(t)
	// The base already carries a cache-busting query, so the variants only
	// change its value.
	page := strings.Replace(string(site(t, "spa", "v1.index.html")), "main.9e8d7c6b.css", "main.9e8d7c6b.css?v=100", 1)
	base := Normalize(KindHTML, []byte(page), NormalizeOptions{})
	baseFeat := Extract(ExtractInput{URL: "https://shop.example/", Kind: KindHTML, Body: []byte(page)})
	bundle := site(t, "spa", spaBundles[0])
	baseJS := Normalize(KindJS, bundle, NormalizeOptions{})
	css := []byte("a { color : red ; background : url( /i.png?v=12 ) }\n/* x */ .b>.c{margin:0}")
	baseCSS := Normalize(KindCSS, css, NormalizeOptions{})
	plants := []struct {
		name string
		old  string
		new  string
		want string // a feature id the change must add
	}{
		{"link", `<a href="/account">Account</a>`, `<a href="/account">Account</a><a href="/admin">Admin</a>`, "link|https://shop.example/admin"},
		{"script", `</head>`, `<script src="https://evil.example/x.js"></script></head>`, "script|https://evil.example/x.js"},
		{"form", `</body>`, `<form method="post" action="/transfer"><input name="amount"></form></body>`, "form|ep:POST https://shop.example/transfer"},
		{"endpoint", `window.__BOOT__=`, `fetch("/api/internal/keys");window.__BOOT__=`, "endpoint|ep:GET https://shop.example/api/internal/keys"},
		{"iframe", `<div id="root"></div>`, `<div id="root"></div><iframe src="https://pay.example/frame"></iframe>`, "frame|https://pay.example/frame"},
	}
	for i := range max(100, testkit.Cases()/10) {
		v := volatileVariant(r, page)
		got := Normalize(KindHTML, []byte(v), NormalizeOptions{})
		if !bytes.Equal(got.Text, base.Text) {
			t.Fatalf("case %d: volatile variant changed the normalized page\n%s", i, firstDiff(base.Text, got.Text))
		}
		if f := Extract(ExtractInput{URL: "https://shop.example/", Kind: KindHTML, Body: []byte(v)}); !equalIDs(f.FeatureIDs(), baseFeat.FeatureIDs()) {
			t.Fatalf("case %d: volatile variant changed the features", i)
		}
		if js := Normalize(KindJS, minifierVariant(r, bundle), NormalizeOptions{}); !bytes.Equal(js.Text, baseJS.Text) {
			t.Fatalf("case %d: minifier variant changed the normalized bundle\n%s", i, firstDiff(baseJS.Text, js.Text))
		}
		cv := strings.ReplaceAll(strings.ReplaceAll(string(css), " ", pick(r, []string{"", " ", "  "})), "v=12", "v="+randToken(r, 4, "0123456789"))
		if c := Normalize(KindCSS, []byte(cv), NormalizeOptions{}); !bytes.Equal(c.Text, baseCSS.Text) {
			t.Fatalf("case %d: css variant %q changed\n%s", i, cv, firstDiff(baseCSS.Text, c.Text))
		}
		p := plants[i%len(plants)]
		if !strings.Contains(page, p.old) {
			t.Fatalf("plant anchor %q missing from the fixture", p.old)
		}
		planted := volatileVariant(r, strings.Replace(page, p.old, p.new, 1))
		pn := Normalize(KindHTML, []byte(planted), NormalizeOptions{})
		pf := mustProcess(t, capture("https://shop.example/", []byte(planted), nil, t0), ProcessOptions{})
		if _, ok := sortedHas(pf.FeatureIDs(), p.want); !ok {
			t.Fatalf("case %d: planted %s not detected; features %v", i, p.name, pf.FeatureIDs())
		}
		if p.name != "endpoint" && bytes.Equal(pn.Text, base.Text) {
			t.Fatalf("case %d: planted %s left the normalized page unchanged", i, p.name)
		}
	}
}

// CONTENT-INV-03: masks never hide level-3 features. Features come from
// the raw document, so any learned rule set leaves them unchanged; a rule
// on a level-3 feature path is refused by Validate and never frozen by
// LearnVolatility; and the built-in masks never touch a path-like value.
func TestContentInv03_MasksNeverHideFeatures(t *testing.T) {
	r := testkit.Rng(t)
	var paths []string
	for k := range featureAttrs {
		paths = append(paths, "html:attr:"+k)
	}
	paths = append(paths, "html:attr:*.href", "html:attr:button.onclick", "json:/paths/~1users/get", "header:content-security-policy",
		"header:set-cookie", "html:attr:div.data-build", "html:attr:span.data-x", "json:/meta/requestTime", "header:x-request-id")
	var counters []VolatilityCounter
	for _, p := range paths {
		counters = append(counters, VolatilityCounter{Path: p, Changes: 50, Stable: 0, Streak: 50})
	}
	for _, algo := range []VolatilityAlgo{VolatilityRatio, VolatilityStreak} {
		set, refused := LearnVolatility(VolatilitySet{}, counters, VolatilityPolicy{Algo: algo})
		if err := set.Validate(); err != nil {
			t.Fatalf("%s: learned set invalid: %v", algo, err)
		}
		for _, rule := range set.Rules {
			if IsFeaturePath(rule.Path) {
				t.Fatalf("%s: froze a feature path %q", algo, rule.Path)
			}
		}
		for _, p := range paths {
			if IsFeaturePath(p) && !contains(refused, p) {
				t.Fatalf("%s: feature path %q not refused", algo, p)
			}
		}
		bad := VolatilitySet{Version: 1, Rules: []VolatileRule{{Path: "html:attr:a.href"}}}
		if err := bad.Validate(); !errors.Is(err, ErrMaskOnFeaturePath) {
			t.Fatalf("Validate on a feature path: %v", err)
		}
		page := site(t, "spa", "v3.index.html")
		plain := mustProcess(t, capture("https://shop.example/", page, nil, t0), ProcessOptions{})
		withRules := mustProcess(t, capture("https://shop.example/", page, nil, t0), ProcessOptions{Learned: set})
		if !equalIDs(plain.FeatureIDs(), withRules.FeatureIDs()) {
			t.Fatalf("%s: learned rules changed the features", algo)
		}
		if withRules.Main().Normalized.Version == plain.Main().Normalized.Version {
			t.Fatalf("%s: learned rules did not change normalizer_version", algo)
		}
	}
	for range testkit.Cases() {
		v := "/" + randToken(r, 1+core.IntN(r, 20), alnumChars+"/-_.")
		if c, ok := volatileScalar(v); ok {
			t.Fatalf("path-like %q masked as %s", v, c)
		}
	}
}

// CONTENT-INV-04: hidden form fields keep only short numeric or boolean
// values in normalized (indexable) output (V2 §12.11).
func TestContentInv04_HiddenFieldsRedacted(t *testing.T) {
	r := testkit.Rng(t)
	for range testkit.Cases() {
		v := randToken(r, 1+core.IntN(r, 30), alnumChars)
		page := `<form><input type="hidden" name="` + randToken(r, 5, "abcdefgh") + `" value="` + v + `"></form>`
		out := string(Normalize(KindHTML, []byte(page), NormalizeOptions{}).Text)
		kept := strings.Contains(out, `value="`+v+`"`)
		if kept != shortScalar(v) {
			t.Fatalf("hidden value %q kept=%v (short scalar %v)\n%s", v, kept, shortScalar(v), out)
		}
	}
}

func equalIDs(a, b []string) bool { return strings.Join(a, "\n") == strings.Join(b, "\n") }

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func sortedHas(xs []string, x string) (int, bool) {
	for i, y := range xs {
		if y == x {
			return i, true
		}
	}
	return -1, false
}

func firstDiff(a, b []byte) string {
	la, lb := strings.Split(string(a), "\n"), strings.Split(string(b), "\n")
	for i := 0; i < min(len(la), len(lb)); i++ {
		if la[i] != lb[i] {
			return fmt.Sprintf("line %d:\n- %s\n+ %s", i+1, la[i], lb[i])
		}
	}
	return fmt.Sprintf("length %d vs %d lines", len(la), len(lb))
}
