package content

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// testHasher is a fixed-key KeyedHasher for tests.
var testHasher = TenantKeyHasher{Key: []byte("content-test-tenant-key")}

var t0 = time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)

// site reads conformance/content/sites/<site>/<name>.
func site(t testing.TB, siteName, name string) []byte {
	t.Helper()
	return testkit.ReadFile(t, "content/sites/"+siteName, name)
}

// headerFile parses a "Name: value" fixture into raw headers.
func headerFile(t testing.TB, siteName, name string) []Header {
	t.Helper()
	var hs []Header
	for _, l := range strings.Split(strings.TrimSpace(string(site(t, siteName, name))), "\n") {
		k, v, _ := strings.Cut(l, ":")
		hs = append(hs, Header{Name: strings.TrimSpace(k), Value: strings.TrimSpace(v)})
	}
	return hs
}

func capture(u string, body []byte, hs []Header, at time.Time) *Capture {
	return &Capture{URL: u, Profile: DefaultProfile(), Status: 200, Headers: hs, Body: body, FetchedAt: at, Outcome: OutcomeComplete}
}

func mustProcess(t testing.TB, c *Capture, opts ProcessOptions) *Processed {
	t.Helper()
	if opts.Hasher == nil {
		opts.Hasher = testHasher
	}
	p, err := Process(c, opts)
	if err != nil {
		t.Fatalf("Process(%s): %v", c.URL, err)
	}
	return p
}

// spaPage processes version v ("v1", "v2", "v3") of the SPA fixture page.
func spaPage(t testing.TB, v string, at time.Time) *Processed {
	return mustProcess(t, capture("https://shop.example/", site(t, "spa", v+".index.html"), headerFile(t, "spa", v+".headers.txt"), at), ProcessOptions{})
}

// spaBundles are the three deploys of the SPA's webpack chunk.
var spaBundles = []string{"main.3f9a1c2b.js", "main.77e0d41a.js", "main.5b6c7d8e.js"}

// spaBundle processes a bundle captured under the stable doc id of the
// first deploy (as the store does once lineage linked them).
func spaBundle(t testing.TB, i int) *Processed {
	u := "https://shop.example/static/js/" + spaBundles[i]
	p := mustProcess(t, capture(u, site(t, "spa", spaBundles[i]), []Header{{"Content-Type", "application/javascript"}}, t0.AddDate(0, 0, 7*i)),
		ProcessOptions{PageURL: "https://shop.example/"})
	return relabel(p, "https://shop.example/static/js/"+spaBundles[0])
}

// relabel moves a processed capture to another URL (lineage).
func relabel(p *Processed, u string) *Processed {
	old := p.URL
	p.URL = u
	for i := range p.Artifacts {
		p.Artifacts[i].DocID = u + strings.TrimPrefix(p.Artifacts[i].DocID, old)
	}
	return p
}

// genWebpackBundle generates a webpack 5 chunk of n modules with seeded
// identifier names and endpoint literals. rename picks another seed for
// the local names only (what a minifier does between builds); order
// permutes the modules.
func genWebpackBundle(seed uint64, n int, renameSeed uint64, order []int) string {
	r := core.NewSplitMix64(seed)
	names := core.NewSplitMix64(renameSeed)
	letters := "abcdefghijklmnopqrstuvwxyz"
	// "_" keeps two-letter names clear of keywords (do, if, in).
	local := func() string {
		return "_" + string(letters[core.IntN(names, 26)]) + string(letters[core.IntN(names, 26)])
	}
	words := []string{"users", "orders", "cart", "items", "search", "profile", "billing", "session", "feed", "notify", "audit", "export"}
	mods := make([]string, n)
	for i := range n {
		a, b, c, d := local(), local(), local(), local()
		for b == a {
			b = local()
		}
		for c == a || c == b {
			c = local()
		}
		for d == a || d == b || d == c {
			d = local()
		}
		w := words[core.IntN(r, len(words))]
		var body strings.Builder
		fmt.Fprintf(&body, "%d:function(%s,%s,%s){\"use strict\";%s.r(%s);var %s=%s(%d);", 1000+i, a, b, c, c, b, d, c, 1000+core.IntN(r, n))
		for j := range 3 + core.IntN(r, 4) {
			fmt.Fprintf(&body, "%s.f%d=function(%s){if(!%s||%s.length<%d)return null;return fetch(\"/api/v%d/%s/\"+%s.id,{method:\"%s\",credentials:\"include\"}).then(function(%s){return %s.json()})};",
				b, j, a, a, a, j+1, 1+core.IntN(r, 3), w, a, []string{"GET", "POST", "PUT"}[core.IntN(r, 3)], d, d)
		}
		fmt.Fprintf(&body, "%s.k=[%d,%d,\"%s-%d\"]}", b, core.IntN(r, 1000), core.IntN(r, 1000), w, i)
		mods[i] = body.String()
	}
	if order == nil {
		order = make([]int, n)
		for i := range order {
			order[i] = i
		}
	}
	var b strings.Builder
	b.WriteString(`(self.webpackChunkapp=self.webpackChunkapp||[]).push([[42],{`)
	for k, i := range order {
		if k > 0 {
			b.WriteByte(',')
		}
		b.WriteString(mods[i])
	}
	b.WriteString(`},function(e){e(e.s=1000)}]);` + "\n")
	return b.String()
}

// writeExpected writes a conformance file when FF_CONTENT_UPDATE=1; the
// conformance tests otherwise only compare.
func writeExpected(t *testing.T, suite, name string, data []byte) {
	t.Helper()
	if os.Getenv("FF_CONTENT_UPDATE") != "1" {
		return
	}
	p := filepath.Join(testkit.ConformanceDir(t, suite), name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", p)
}
