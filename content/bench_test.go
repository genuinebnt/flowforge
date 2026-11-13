package content

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/core"
)

// benchInputs builds roughly size-byte documents of each kind.
func benchInputs(b *testing.B, size int) map[Kind][]byte {
	r := core.NewSplitMix64(1)
	out := map[Kind][]byte{}
	page := site(b, "spa", "v3.index.html")
	var h bytes.Buffer
	for h.Len() < size {
		h.WriteString(`<section class="card b a" data-ts="1762596000"><h2>Item</h2><a href="/p/` + itoa(h.Len()) + `?v=3">x</a><form method="post" action="/cart"><input name="sku"><input type="hidden" name="_t" value="17625960001"></form><p>text   with  spaces &amp; more</p></section>`)
	}
	out[KindHTML] = bytes.Replace(page, []byte(`<div id="root"></div>`), h.Bytes(), 1)
	n := 10
	for len(genWebpackBundle(1, n, 1, nil)) < size {
		n *= 2
	}
	out[KindJS] = []byte(genWebpackBundle(1, n, 1, nil))
	// A realistic stylesheet: balanced rules, a few at-rules and urls.
	var c strings.Builder
	for i := 0; c.Len() < size; i++ {
		fmt.Fprintf(&c, ".c%d > .item:hover, .c%d a { color : #%06x; margin: 0 auto; background: url(/img/i%d.png?v=%d) no-repeat; }\n", i, i, i*7919%0xffffff, i%50, i%9)
		if i%40 == 0 {
			fmt.Fprintf(&c, "@media (max-width: %dpx) { .c%d { display: none !important } }\n/* section %d */\n", 400+i%800, i, i)
		}
	}

	out[KindCSS] = []byte(c.String())
	var j strings.Builder
	j.WriteString("[")
	for j.Len() < size {
		if j.Len() > 1 {
			j.WriteString(",")
		}
		j.WriteString(genJSON(r, 0))
	}
	j.WriteString("]")
	out[KindJSON] = []byte(j.String())
	var x strings.Builder
	x.WriteString(`<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for x.Len() < size {
		fmt.Fprintf(&x, "<url><loc>https://shop.example/p/%d</loc><lastmod>2026-11-01</lastmod></url>\n", x.Len())
	}
	x.WriteString("</urlset>")
	out[KindXML] = []byte(x.String())
	var t strings.Builder
	for t.Len() < size {
		t.WriteString(genTextDoc(r))
	}
	out[KindText] = []byte(t.String())
	return out
}

// BenchmarkContentNormalize measures normalization throughput per kind on
// 1 MiB documents.
func BenchmarkContentNormalize(b *testing.B) {
	in := benchInputs(b, 1<<20)
	for _, k := range []Kind{KindHTML, KindJS, KindCSS, KindJSON, KindXML, KindText} {
		b.Run(string(k), func(b *testing.B) {
			b.SetBytes(int64(len(in[k])))
			b.ReportAllocs()
			for b.Loop() {
				Normalize(k, in[k], NormalizeOptions{})
			}
		})
	}
}

// BenchmarkContentExtract measures extraction throughput per kind on
// 1 MiB documents.
func BenchmarkContentExtract(b *testing.B) {
	in := benchInputs(b, 1<<20)
	for _, k := range []Kind{KindHTML, KindJS, KindCSS, KindJSON, KindXML, KindText} {
		kind := k
		if k == KindXML {
			kind = KindSitemap
		}
		b.Run(string(k), func(b *testing.B) {
			b.SetBytes(int64(len(in[k])))
			b.ReportAllocs()
			for b.Loop() {
				Extract(ExtractInput{URL: "https://shop.example/", Kind: kind, Body: in[k], Hasher: testHasher})
			}
		})
	}
}

// BenchmarkContentPipeline is the end-to-end path on a 2 MB bundle:
// capture → sniff, normalize, redact, extract, split modules, fingerprint
// → diff and classify against the previous deploy (renamed locals, one
// edited module).
func BenchmarkContentPipeline(b *testing.B) {
	n := 10
	for len(genWebpackBundle(3, n, 3, nil)) < 2<<20 {
		n *= 2
	}
	prevBody := []byte(genWebpackBundle(3, n, 3, nil))
	curBody := bytes.Replace([]byte(genWebpackBundle(3, n, 4, nil)), []byte(`credentials:"include"`), []byte(`credentials:"omit"`), 1)
	hs := []Header{{"Content-Type", "application/javascript"}}
	prev := mustProcess(b, capture("https://shop.example/app.js", prevBody, hs, t0), ProcessOptions{PageURL: "https://shop.example/"})
	b.SetBytes(int64(len(curBody)))
	b.ReportAllocs()
	for b.Loop() {
		cur := mustProcess(b, capture("https://shop.example/app.js", curBody, hs, t0), ProcessOptions{PageURL: "https://shop.example/"})
		if _, err := Compare(prev, cur, DiffOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkContentAlgo compares the candidates of each pluggable decision
// (CONVENTIONS "Pluggable algorithms"): go test -run '^$' -bench
// ContentAlgo ./content.
func BenchmarkContentAlgo(b *testing.B) {
	// lineage: fingerprint a 200 KB bundle and match it among 20
	// previous scripts of the page.
	n := 10
	for len(genWebpackBundle(5, n, 5, nil)) < 200<<10 {
		n += 10
	}
	cur := []byte(genWebpackBundle(5, n, 6, nil))
	toks := LineageTokens(KindJS, cur)
	for _, algo := range []LineageAlgo{LineageSimHash, LineageMinHash, LineageJaccard} {
		var prev []LineageCandidate
		for i := range 20 {
			body := []byte(genWebpackBundle(uint64(100+i), n, 7, nil))
			if i == 13 {
				body = []byte(genWebpackBundle(5, n, 5, nil))
			}
			prev = append(prev, LineageCandidate{URL: fmt.Sprintf("https://x.example/%d.%08x.js", i, i), Fingerprint: FingerprintOf(LineageTokens(KindJS, body), len(body), algo)})
		}
		b.Run("lineage/"+algo.String(), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				fp := FingerprintOf(toks, len(cur), algo)
				if m, ok := FindPredecessor("https://x.example/main.ffffffff.js", fp, prev, LineageOptions{Algo: algo}); !ok || m.URL != prev[13].URL {
					b.Fatalf("%s: no match", algo)
				}
			}
		})
	}
	// splitter: split a 2 MB bundle (parsed once).
	nn := 10
	for len(genWebpackBundle(9, nn, 9, nil)) < 2<<20 {
		nn *= 2
	}
	prog := parseJS([]byte(genWebpackBundle(9, nn, 9, nil)), true)
	for _, s := range []SplitStrategy{SplitAuto, SplitWebpack, SplitESBuild, SplitTopLevel} {
		b.Run("splitter/"+s.String(), func(b *testing.B) {
			b.SetBytes(int64(len(prog.Source)))
			b.ReportAllocs()
			for b.Loop() {
				SplitModules(prog, SplitOptions{Strategy: s})
			}
		})
	}
	// volatility: learn over 10,000 counters.
	r := core.NewSplitMix64(11)
	var counters []VolatilityCounter
	for i := range 10_000 {
		c := VolatilityCounter{Path: fmt.Sprintf("html:attr:div.data-x%d", i)}
		for range 12 {
			c = c.Record(core.IntN(r, 10) < 9)
		}
		counters = append(counters, c)
	}
	for _, a := range []VolatilityAlgo{VolatilityRatio, VolatilityStreak} {
		b.Run("volatility/"+a.String(), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				LearnVolatility(VolatilitySet{}, counters, VolatilityPolicy{Algo: a})
			}
		})
	}
	// diff: two 512 KB normalized bundle versions, about 1 % of the
	// modules edited.
	m := 10
	for len(genWebpackBundle(13, m, 13, nil)) < 512<<10/2 {
		m += 20
	}
	oldB := []byte(genWebpackBundle(13, m, 13, nil))
	newB := oldB
	for i := range m / 100 {
		newB = bytes.Replace(newB, []byte(fmt.Sprintf(`-%d"]`, i*97)), []byte(fmt.Sprintf(`-%d-edited"]`, i*97)), 1)
	}
	if bytes.Equal(oldB, newB) {
		b.Fatal("diff benchmark: no edit applied")
	}
	oa := buildArtifact("https://x.example/a.js", "https://x.example/", KindJS, oldB, "", "", t0, ProcessOptions{})
	na := buildArtifact("https://x.example/a.js", "https://x.example/", KindJS, newB, "", "", t0, ProcessOptions{})
	for _, p := range []DiffPolicy{DiffBySize, DiffMyers, DiffHistogram, DiffPatience} {
		b.Run("diff/"+p.String(), func(b *testing.B) {
			b.SetBytes(int64(len(na.Normalized.Text)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := DiffArtifacts(&oa, &na, DiffOptions{Policy: p}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
