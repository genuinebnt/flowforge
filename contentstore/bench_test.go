package contentstore

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/core"
)

// genBundle returns a webpack-like bundle of n functions; version v edits
// v of them (a changed literal) and adds one endpoint every 10 versions.
func genBundle(n, v int) []byte {
	var b strings.Builder
	b.WriteString("(self.webpackChunk=self.webpackChunk||[]).push([[1],{\n")
	r := core.NewSplitMix64(42)
	for i := range n {
		lit := token(r, 12, alnum)
		if i < v {
			lit = fmt.Sprintf("edited-%d-%d", v, i)
		}
		fmt.Fprintf(&b, "%d:function(e,t,n){var a=n(%d);t.load%d=function(x){return fetch(\"/api/v2/items/\"+x+\"/m%d\",{method:\"GET\"}).then(function(r){return r.json()}).then(function(d){return a.render(d,\"%s\")})}},\n",
			i, (i+1)%n, i, i, lit)
	}
	for k := 0; k < v/10; k++ {
		fmt.Fprintf(&b, "%d:function(e,t){t.admin%d=function(){return fetch(\"/api/v2/admin/k%d\",{method:\"POST\"})}},\n", n+k, k, k)
	}
	b.WriteString("}]);\n")
	return []byte(b.String())
}

func bundleVersion(v int, n int) content.Capture {
	return capture("https://bench.example/static/js/main.js", genBundle(n, v), jsHeaders, t0.Add(time.Duration(v)*time.Hour))
}

// BenchmarkCStoreAlgo compares the store's pluggable decisions
// (docs/ALGORITHMS.md "contentstore"):
//
//	codec/<delta|chunk>/versions50: 50 versions of a ~300 KB bundle with
//	  small edits; reports stored/raw (the 50-version acceptance shape of
//	  V2 §6.1) and ingest time per version;
//	codec/<delta|chunk>/spa: the SPA fixture's nine captures;
//	index/<inline|batched>/spa: ingest of the SPA fixture with the index
//	  updated per capture or once at the end.
func BenchmarkCStoreAlgo(b *testing.B) {
	for _, codec := range []BlobCodec{CodecDelta, CodecChunk} {
		b.Run("codec/"+codec.String()+"/versions50", func(b *testing.B) {
			caps := make([]content.Capture, 50)
			var raw int64
			for v := range caps {
				caps[v] = bundleVersion(v, 1500)
				raw += int64(len(caps[v].Body))
			}
			b.ReportAllocs()
			var stored uint64
			for b.Loop() {
				h := newHarness(b, func(c *Config) { c.Codec = codec; c.Index = nil })
				for _, c := range caps {
					h.ingest(tenantA, c, "")
				}
				m, _ := h.chunks.Metrics(bg, tenantA)
				stored = m.CompressedBytes
			}
			b.ReportMetric(100*float64(stored)/float64(raw), "stored%")
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(caps))/1e6, "ms/version")
		})
		b.Run("codec/"+codec.String()+"/spa", func(b *testing.B) {
			var stored uint64
			b.ReportAllocs()
			for b.Loop() {
				h := newHarness(b, func(c *Config) { c.Codec = codec; c.Index = nil })
				h.ingestSPA(tenantA)
				m, _ := h.chunks.Metrics(bg, tenantA)
				stored = m.CompressedBytes
			}
			b.ReportMetric(100*float64(stored)/71825, "stored%")
		})
	}
	for _, mode := range []IndexMode{IndexInline, IndexBatched} {
		b.Run("index/"+mode.String()+"/spa", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				h := newHarness(b, func(c *Config) { c.Index.Mode = mode })
				h.ingestSPA(tenantA)
				if mode == IndexBatched {
					if _, err := h.store.IndexPending(bg, tenantA, programP); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}

// BenchmarkCStoreIngest is ingest throughput: one new version of a
// ~300 KB bundle per op (process, diff, encode, store, index inline).
func BenchmarkCStoreIngest(b *testing.B) {
	h := newHarness(b)
	h.ingest(tenantA, bundleVersion(0, 1500), "")
	size := len(genBundle(1500, 1))
	v := 1
	b.SetBytes(int64(size))
	b.ReportAllocs()
	for b.Loop() {
		h.ingest(tenantA, bundleVersion(v, 1500), "")
		v++
	}
}

// BenchmarkCStoreSearch is search latency over the index of 30 bundle
// versions and the SPA fixture: a term, a field query, a phrase and a
// regex per op.
func BenchmarkCStoreSearch(b *testing.B) {
	h := newHarness(b)
	h.ingestSPA(tenantA)
	for v := range 30 {
		h.ingest(tenantA, bundleVersion(v, 500), "")
	}
	qs := []string{"admin", "kind:endpoint_added", `"api v2 items"`, `/admin\/k[0-9]/`}
	b.ReportAllocs()
	for b.Loop() {
		for _, q := range qs {
			if _, err := h.store.Search(bg, tenantA, programP, q, SearchOptions{K: 20}); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(qs))/1e3, "µs/query")
}
