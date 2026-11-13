package textindex

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// Synthetic diff-hunk corpus: lines of JavaScript-like code with
// identifiers from a Zipf-ish vocabulary, string literals holding API
// paths and URLs, numbers and hashes.

var syllables = []string{"get", "set", "user", "id", "fetch", "api", "token", "auth", "load", "data", "item", "cart",
	"order", "pay", "price", "list", "view", "page", "route", "query", "config", "init", "render", "state", "store",
	"event", "handle", "click", "submit", "form", "input", "value", "error", "retry", "cache", "session", "admin",
	"graph", "node", "edge", "file", "upload", "image", "size", "count", "index", "key", "secret", "json", "xml"}

type corpusGen struct{ r core.Rng }

func (g *corpusGen) ident() string {
	n := 1 + core.IntN(g.r, 3)
	var b strings.Builder
	for i := 0; i < n; i++ {
		// Zipf-ish: low indexes much more likely.
		s := syllables[core.IntN(g.r, 1+core.IntN(g.r, len(syllables)))]
		if i > 0 && core.IntN(g.r, 3) > 0 {
			s = strings.ToUpper(s[:1]) + s[1:]
		} else if i > 0 {
			b.WriteByte('_')
		}
		b.WriteString(s)
	}
	if core.IntN(g.r, 8) == 0 {
		b.WriteString(strconv.Itoa(core.IntN(g.r, 100)))
	}
	return b.String()
}

func (g *corpusGen) line() string {
	sign := "+"
	if core.IntN(g.r, 2) == 0 {
		sign = "-"
	}
	switch core.IntN(g.r, 6) {
	case 0:
		return fmt.Sprintf("%s const %s = await fetch('/api/v%d/%s/' + %s, {method: '%s'});", sign, g.ident(), 1+core.IntN(g.r, 3), g.ident(), g.ident(), pick(g.r, []string{"GET", "POST", "PUT"}))
	case 1:
		return fmt.Sprintf("%s %s.%s(%s, %d);", sign, g.ident(), g.ident(), g.ident(), core.IntN(g.r, 10000))
	case 2:
		return fmt.Sprintf("%s import { %s } from 'https://cdn.example.com/%s/%s.%x.js';", sign, g.ident(), g.ident(), g.ident(), g.r.Uint64()&0xffffff)
	case 3:
		return fmt.Sprintf("%s if (%s && %s.%s !== null) { return %s(%s); }", sign, g.ident(), g.ident(), g.ident(), g.ident(), g.ident())
	case 4:
		return fmt.Sprintf("%s <a href=\"https://shop.example.com/%s/%s?ref=%s\">%s</a>", sign, g.ident(), g.ident(), g.ident(), g.ident())
	}
	return fmt.Sprintf("%s %s: '%s_%x',", sign, g.ident(), g.ident(), g.r.Uint64()&0xffffffff)
}

func (g *corpusGen) doc(id uint64) Document {
	var b strings.Builder
	for n := 2 + core.IntN(g.r, 6); n > 0; n-- {
		b.WriteString(g.line())
		b.WriteByte('\n')
	}
	return Document{
		ID:   id,
		Text: []FieldValue{{"text", b.String()}, {"url", "https://shop.example.com/static/" + g.ident() + ".js"}},
		Keyword: []FieldValue{{"program", "p" + strconv.Itoa(core.IntN(g.r, 4))}, {"kind", pick(g.r, genKinds)},
			{"side", pick(g.r, []string{"added", "removed"})}, {"mime", "application/javascript"}},
		Numeric: []NumericValue{{"at", genNow - int64(core.IntN(g.r, 60))*86_400_000}},
	}
}

func benchCorpus(n int) ([]Document, int) {
	g := &corpusGen{r: core.NewSplitMix64(0xD1FF)}
	docs := make([]Document, n)
	raw := 0
	for i := range docs {
		docs[i] = g.doc(uint64(i))
		for _, f := range docs[i].Text {
			raw += len(f.Value)
		}
	}
	return docs, raw
}

const benchDocs = 100_000

func buildBenchIndex(tb testing.TB, docs []Document) *Index {
	ix, err := Open(Config{Schema: DefaultSchema()})
	if err != nil {
		tb.Fatal(err)
	}
	for i, d := range docs {
		if err := ix.Add(d); err != nil {
			tb.Fatal(err)
		}
		if i%10_000 == 9_999 {
			if err := ix.Refresh(); err != nil {
				tb.Fatal(err)
			}
		}
	}
	if err := ix.Refresh(); err != nil {
		tb.Fatal(err)
	}
	return ix
}

// BenchmarkIndexing indexes 100,000 synthetic diff hunks (refresh every
// 10,000, size-tiered merges with factor 10, so the result is one
// segment) and reports docs/s, index bytes per doc and index size
// relative to the raw text.
func BenchmarkIndexing(b *testing.B) {
	docs, raw := benchCorpus(benchDocs)
	b.ReportAllocs()
	var st Stats
	for b.Loop() {
		ix := buildBenchIndex(b, docs)
		st = ix.Stats()
		_ = ix.Close()
	}
	b.ReportMetric(float64(benchDocs)*float64(b.N)/b.Elapsed().Seconds(), "docs/s")
	b.ReportMetric(float64(st.Bytes)/benchDocs, "index-B/doc")
	b.ReportMetric(float64(raw)/benchDocs, "raw-B/doc")
	b.ReportMetric(float64(st.Bytes)/float64(raw), "index/raw")
	b.ReportMetric(float64(st.Segments), "segments")
}

var (
	benchOnce sync.Once
	benchIx   *Index
)

func benchIndex(b *testing.B) *Index {
	benchOnce.Do(func() {
		docs, _ := benchCorpus(benchDocs)
		benchIx = buildBenchIndex(b, docs)
	})
	return benchIx
}

func benchQueries(b *testing.B, qs []string) {
	ix := benchIndex(b)
	for _, q := range qs {
		if _, err := ix.Search(q, SearchOptions{K: 10, Now: genNow}); err != nil {
			b.Fatalf("%q: %v", q, err)
		}
	}
	b.ReportAllocs()
	var lat []time.Duration
	i, hits := 0, 0
	for b.Loop() {
		q := qs[i%len(qs)]
		i++
		t0 := time.Now()
		res, _ := ix.Search(q, SearchOptions{K: 10, Now: genNow})
		lat = append(lat, time.Since(t0))
		hits += res.Total
	}
	slices.Sort(lat)
	b.ReportMetric(float64(lat[len(lat)/2].Nanoseconds()), "p50-ns")
	b.ReportMetric(float64(lat[len(lat)*99/100].Nanoseconds()), "p99-ns")
	b.ReportMetric(float64(hits)/float64(i), "matches/op")
}

func BenchmarkQuery_Term(b *testing.B) {
	benchQueries(b, []string{"token", "getUserId", "cart", "session_retry", "upload", "render", "secret", "graphEdge"})
}

func BenchmarkQuery_Phrase(b *testing.B) {
	benchQueries(b, []string{`"await fetch"`, `"api v2"`, `"user id"`, `"cdn example com"`, `"return get"`, `"method post"`})
}

func BenchmarkQuery_Regex(b *testing.B) {
	benchQueries(b, []string{`/fetch\('\/api\/v2\/\w+/`, `/secret[A-Z]\w*/`, `/https:\/\/cdn\.example\.com\/[a-z]+\//`, `/session_?retry/i`, `/[0-9a-f]{6}\.js/`})
}

func BenchmarkQuery_Filtered(b *testing.B) {
	benchQueries(b, []string{"kind:endpoint_added side:added since:7d token", "program:p1 -side:removed cart", "kind:script_added since:3d /upload\\w+/", "program:p2 kind:header_changed since:1d"})
}

// BenchmarkMerge force-merges ten 10,000-document segments with 10 %
// tombstones into one and reports merged docs/s.
func BenchmarkMerge(b *testing.B) {
	docs, _ := benchCorpus(benchDocs)
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		ix, err := Open(Config{Schema: DefaultSchema(), DisableAutoMerge: true})
		if err != nil {
			b.Fatal(err)
		}
		for i, d := range docs {
			_ = ix.Add(d)
			if i%10_000 == 9_999 {
				_ = ix.Refresh()
			}
		}
		for i := 0; i < benchDocs; i += 10 {
			_ = ix.Delete(uint64(i))
		}
		_ = ix.Refresh()
		b.StartTimer()
		if err := ix.ForceMerge(1); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		_ = ix.Close()
		b.StartTimer()
	}
	b.ReportMetric(float64(benchDocs*9/10)*float64(b.N)/b.Elapsed().Seconds(), "docs/s")
}
