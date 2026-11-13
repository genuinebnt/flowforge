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

// BenchmarkHTMLImpl compares full parsing by this package ("ours") and
// golang.org/x/net/html ("xnethtml", scripting off like ours) on the same
// inputs: go test -bench HTMLImpl prints the two side by side.
func BenchmarkHTMLImpl(b *testing.B) {
	b.Run("ours", func(b *testing.B) {
		runSizes(b, func(b *testing.B, in []byte) {
			for b.Loop() {
				Parse(in, Options{})
			}
		})
	})
	b.Run("xnethtml", func(b *testing.B) {
		runSizes(b, func(b *testing.B, in []byte) {
			for b.Loop() {
				if _, err := html.ParseWithOptions(bytes.NewReader(in), html.ParseOptionEnableScripting(false)); err != nil {
					b.Fatal(err)
				}
			}
		})
	})
}

// BenchmarkHTMLImplTokenize compares the tokenizers: ours (Next until
// EOF) and x/net/html's (Next until ErrorToken, materializing each
// token as html.Token, which unescapes and allocates like ours).
func BenchmarkHTMLImplTokenize(b *testing.B) {
	b.Run("ours", func(b *testing.B) {
		runSizes(b, func(b *testing.B, in []byte) {
			for b.Loop() {
				tokenizeAll(in, TokenizeOptions{})
			}
		})
	})
	b.Run("xnethtml", func(b *testing.B) {
		runSizes(b, func(b *testing.B, in []byte) {
			for b.Loop() {
				z := html.NewTokenizer(bytes.NewReader(in))
				for z.Next() != html.ErrorToken {
					_ = z.Token()
				}
			}
		})
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

// BenchmarkHTMLCharRef compares the named character reference lookup
// strategies on entity-dense input (tokenizer only).
func BenchmarkHTMLCharRef(b *testing.B) {
	in := charRefInput()
	for _, s := range []struct {
		name string
		st   CharRefStrategy
	}{{"sorted", CharRefSortedSearch}, {"trie", CharRefTrie}} {
		b.Run(s.name, func(b *testing.B) {
			b.SetBytes(int64(len(in)))
			b.ReportAllocs()
			for b.Loop() {
				tokenizeAll(in, TokenizeOptions{CharRefLookup: s.st})
			}
		})
	}
}
