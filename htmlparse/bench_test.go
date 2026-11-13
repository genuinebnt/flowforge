package htmlparse

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"golang.org/x/net/html"
)

// Benchmarks. Inputs are generated deterministically (seed 42) by
// genWellFormed: realistic nested markup with tables, lists, forms,
// entities and comments. Each size is generated once per process.

var benchSizes = []struct {
	name string
	n    int
}{{"10KB", 10 << 10}, {"1MB", 1 << 20}, {"10MB", 10 << 20}}

var (
	benchMu     sync.Mutex
	benchInputs = map[int][]byte{}
)

func benchInput(n int) []byte {
	benchMu.Lock()
	defer benchMu.Unlock()
	if in, ok := benchInputs[n]; ok {
		return in
	}
	in := genWellFormed(core.NewSplitMix64(42), n)
	benchInputs[n] = in
	return in
}

func runSizes(b *testing.B, f func(b *testing.B, in []byte)) {
	for _, s := range benchSizes {
		b.Run(s.name, func(b *testing.B) {
			in := benchInput(s.n)
			b.SetBytes(int64(len(in)))
			b.ReportAllocs()
			f(b, in)
		})
	}
}

func tokenizeAll(in []byte, opts TokenizeOptions) int {
	t := NewTokenizer(in, opts)
	n := 0
	for t.Next().Kind != EOFToken {
		n++
	}
	return n
}

// BenchmarkHTMLTokenize measures the tokenizer alone (tokens are consumed
// one at a time, not collected).
func BenchmarkHTMLTokenize(b *testing.B) {
	runSizes(b, func(b *testing.B, in []byte) {
		for b.Loop() {
			tokenizeAll(in, TokenizeOptions{})
		}
	})
}

// BenchmarkHTMLParse measures tokenizer + tree construction into the
// arena.
func BenchmarkHTMLParse(b *testing.B) {
	runSizes(b, func(b *testing.B, in []byte) {
		for b.Loop() {
			Parse(in, Options{})
		}
	})
}

// charRefInput is entity-dense text: every named reference of the table
// once (with and without its semicolon where both exist), plus near
// misses that force the longest-match search to back off.
func charRefInput() []byte {
	var b strings.Builder
	for _, n := range entityNames {
		b.WriteString("&" + n + " x ")
	}
	for _, n := range entityNames[:500] {
		b.WriteString("&" + n[:len(n)/2+1] + "Q; ")
	}
	return []byte(strings.Repeat(b.String(), 4))
}

// queryStringInput is links whose query strings put long parameter names
// after '&' (href="/s?q=1&utm_campaign_identifier=…"), the common case
// where '&' starts no reference and the search must give up: a strategy
// that probes every prefix length pays for the whole name.
func queryStringInput() []byte {
	params := []string{"utm_campaign_identifier", "returnUrlAfterLogin", "sessionTrackingToken", "pageNumber", "sortOrderDescending", "x"}
	var b strings.Builder
	for i := 0; b.Len() < 1<<20; i++ {
		b.WriteString(`<a href="/search?q=` + strings.Repeat("w", i%7))
		for j := range 4 {
			b.WriteString("&" + params[(i+j)%len(params)] + "=v")
		}
		b.WriteString(`">result</a>` + "\n")
	}
	return []byte(b.String())
}

// BenchmarkHTMLAlgo compares the candidates of each decision point
// (docs/CONVENTIONS.md "Pluggable algorithms"; docs/ALGORITHMS.md
// "htmlparse"): BenchmarkHTMLAlgo/<decision>/<candidate>/<input>.
//
//   - charref/<trie|sorted|map>/<entities|urls|page-1MB>: the named
//     character reference lookup, tokenizer only, on entity-dense text
//     (every name of the table, plus near misses that make the
//     longest-match search back off), on links with long query-string
//     parameter names after '&', and on the generated 1 MB page.
//   - parse/<ours|xnethtml>/<size> and tokenize/<ours|xnethtml>/<size>:
//     this package against golang.org/x/net/html (scripting off like ours;
//     its tokens materialized as html.Token, which unescapes and allocates
//     like ours). x/net/html is a comparison and test oracle only, never a
//     selectable implementation.
func BenchmarkHTMLAlgo(b *testing.B) {
	inputs := []struct {
		name string
		in   []byte
	}{{"entities", charRefInput()}, {"urls", queryStringInput()}, {"page-1MB", benchInput(1 << 20)}}
	for _, st := range CharRefStrategies() {
		for _, in := range inputs {
			b.Run("charref/"+st.String()+"/"+in.name, func(b *testing.B) {
				b.SetBytes(int64(len(in.in)))
				b.ReportAllocs()
				for b.Loop() {
					tokenizeAll(in.in, TokenizeOptions{CharRefLookup: st})
				}
			})
		}
	}
	for _, s := range benchSizes {
		in := benchInput(s.n)
		run := func(name string, f func()) {
			b.Run(name+"/"+s.name, func(b *testing.B) {
				b.SetBytes(int64(len(in)))
				b.ReportAllocs()
				for b.Loop() {
					f()
				}
			})
		}
		run("parse/ours", func() { Parse(in, Options{}) })
		run("parse/xnethtml", func() {
			if _, err := html.ParseWithOptions(bytes.NewReader(in), html.ParseOptionEnableScripting(false)); err != nil {
				b.Fatal(err)
			}
		})
		run("tokenize/ours", func() { tokenizeAll(in, TokenizeOptions{}) })
		run("tokenize/xnethtml", func() {
			z := html.NewTokenizer(bytes.NewReader(in))
			for z.Next() != html.ErrorToken {
				_ = z.Token()
			}
		})
	}
}
