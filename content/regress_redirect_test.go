package content

import (
	"slices"
	"testing"
)

// TestRegress_RedirectBase: a page fetched through http://h/ -> https://h/
// resolves its relative references against the final URL (RFC 3986
// §5.1.3), not the requested one. Found by the v2 lab
// (services/internal/lab OPEN_QUESTIONS 1).
func TestRegress_RedirectBase(t *testing.T) {
	c := capture("http://shop.example/", []byte(`<!doctype html><a href="/cart">c</a><script src="app.js"></script>`),
		[]Header{{"Content-Type", "text/html"}}, t0)
	c.RedirectChain = []Hop{{URL: "http://shop.example/", Status: 301, Headers: []Header{{"Location", "https://shop.example/"}}}}
	p := mustProcess(t, c, ProcessOptions{})
	var keys []string
	for _, f := range p.Features() {
		if f.Kind == FeatLink || f.Kind == FeatScript {
			keys = append(keys, f.Key+" "+f.Ref)
		}
	}
	for _, want := range []string{"https://shop.example/cart", "https://shop.example/app.js"} {
		if !slices.ContainsFunc(keys, func(k string) bool { return containsURL(k, want) }) {
			t.Errorf("no reference resolved to %s; got %q", want, keys)
		}
	}
	for _, k := range keys {
		if containsURL(k, "http://shop.example/") {
			t.Errorf("reference resolved against the requested http URL: %q", k)
		}
	}
}

// TestRegress_HopHeaderDocIDs: every artifact of a capture has a distinct
// doc id, so a redirect hop's header block is never paired with the final
// response's in the next version's diff, and a change to a volatile final
// header is not reported against the hop (services/internal/lab
// OPEN_QUESTIONS 2).
func TestRegress_HopHeaderDocIDs(t *testing.T) {
	mk := func(nonce string) *Processed {
		c := capture("http://shop.example/legacy", []byte(`<!doctype html><p>catalog</p>`),
			[]Header{{"Content-Type", "text/html"}, {"X-Request-Id", nonce}}, t0)
		c.RedirectChain = []Hop{
			{URL: "http://shop.example/legacy", Status: 301, Headers: []Header{{"Location", "https://shop.example/legacy"}}},
			{URL: "https://shop.example/legacy", Status: 302, Headers: []Header{{"Location", "/catalog"}}},
		}
		return mustProcess(t, c, ProcessOptions{})
	}
	a, b := mk("r-1111"), mk("r-2222")
	seen := map[string]bool{}
	for _, art := range a.Artifacts {
		if seen[art.DocID] {
			t.Fatalf("duplicate doc id %q", art.DocID)
		}
		seen[art.DocID] = true
	}
	for _, id := range []string{"http://shop.example/legacy#headers", "http://shop.example/legacy#hop/1/headers", "http://shop.example/legacy#hop/2/headers"} {
		if !seen[id] {
			t.Errorf("missing artifact %q; have %v", id, seen)
		}
	}
	cs, err := Compare(a, b, DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range cs.Artifacts {
		if ch.DocID != "http://shop.example/legacy#headers" && ch.Outcome != DiffNoChange {
			t.Errorf("%s: %s, want %s (only the final response changed)", ch.DocID, ch.Outcome, DiffNoChange)
		}
	}
	if cs.Outcome == DiffStructural || cs.Outcome == DiffChanged {
		t.Errorf("a new request id behind a redirect is reported as %s", cs.Outcome)
	}
}

func containsURL(s, u string) bool {
	for i := 0; i+len(u) <= len(s); i++ {
		if s[i:i+len(u)] == u {
			return true
		}
	}
	return false
}
