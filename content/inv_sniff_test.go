package content

import (
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

var anyMIME = []string{"", "text/html", "text/plain", "application/json", "application/javascript", "text/css",
	"application/octet-stream", "image/png", "application/xml", "text/x-php"}

// CONTENT-INV-13: sniffing decides by content. For bodies with a decisive
// signature (an HTML document start, a JSON value, a binary magic, a
// sitemap root, server-side source) the kind is the same whatever the
// Content-Type says; Mismatch is set exactly when a known hint is
// incompatible with the kind; server-side source is a source-disclosure
// finding; and Sniff is total and deterministic on random bytes.
func TestContentInv13_SniffByContent(t *testing.T) {
	decisive := map[string]Kind{
		"<!DOCTYPE html><html><body>x</body></html>":                     KindHTML,
		`{"a": [1, 2, {"b": null}]}`:                                     KindJSON,
		`{"version":3,"sources":["a.ts"],"mappings":"AAAA"}`:             KindSourceMap,
		"\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR":                            KindBinary,
		`<?xml version="1.0"?><urlset xmlns="x"><url><loc>u</loc></url>`: KindSitemap,
		"<?php echo $secret; ?>":                                         KindServerSource,
		"DB_PASSWORD=hunter2\nAPI_KEY=abc\n":                             KindServerSource,
		"<%@ page import=\"java.util.*\" %>":                             KindServerSource,
	}
	for body, want := range decisive {
		for _, mt := range anyMIME {
			r := Sniff([]byte(body), mt, "https://s.example/x")
			if r.Kind != want {
				t.Fatalf("%q with %q: kind %s, want %s", body, mt, r.Kind, want)
			}
			if r.Mismatch != (r.Hint != "" && !compatibleKinds(r.Hint, r.Kind)) {
				t.Fatalf("%q with %q: mismatch %v for hint %s", body, mt, r.Mismatch, r.Hint)
			}
		}
	}
	p := mustProcess(t, capture("https://s.example/config.php", []byte("<?php $db = 'x'; ?>"), []Header{{"Content-Type", "text/plain"}}, t0), ProcessOptions{})
	if p.Sniff.Disclosure != DisclosurePHP {
		t.Fatalf("php source not disclosed: %+v", p.Sniff)
	}
	if _, ok := sortedHas(p.FeatureIDs(), "source_disclosure|php"); !ok {
		t.Fatalf("no source_disclosure feature: %v", p.FeatureIDs())
	}
	r := testkit.Rng(t)
	for range testkit.Cases() {
		b := make([]byte, core.IntN(r, 300))
		for i := range b {
			b[i] = byte(r.Uint64())
		}
		if core.IntN(r, 2) == 0 {
			b = append([]byte(pick(r, []string{"<", "{", "<?", "#!", "User-agent:", "@media", "function"})), b...)
		}
		mt := pick(r, anyMIME)
		a, c := Sniff(b, mt, "https://s.example/a.js"), Sniff(b, mt, "https://s.example/a.js")
		if a != c || a.Kind == "" || a.Reason == "" {
			t.Fatalf("sniff not total/deterministic on %q: %+v %+v", b, a, c)
		}
	}
}
