package content

import (
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/testkit"
)

// degradations turns a good capture of the SPA page into each kind of
// degraded or incomplete collection (V2 §12.5).
func degradations(t *testing.T) map[string]func(*Capture, *ProcessOptions) {
	page := site(t, "spa", "v1.index.html")
	good := spaPage(t, "v1", t0)
	prev := &PreviousCapture{Status: 200, FeatureIDs: good.FeatureIDs()}
	return map[string]func(*Capture, *ProcessOptions){
		"login_redirect": func(c *Capture, o *ProcessOptions) {
			c.RedirectChain = []Hop{{URL: c.URL, Status: 302, Headers: []Header{{"Location", "https://sso.shop.example/login?returnUrl=%2F"}}}}
		},
		"challenge": func(c *Capture, o *ProcessOptions) {
			c.Status = 403
			c.Body = []byte(`<html><head><title>Just a moment...</title></head><body><script src="/cdn-cgi/challenge-platform/h/b/orchestrate/jsch/v1"></script></body></html>`)
		},
		"captcha_widget_blocked": func(c *Capture, o *ProcessOptions) {
			c.Status = 429
			c.Body = append([]byte(`<div class="g-recaptcha" data-sitekey="x"></div>`), page...)
		},
		"error_after_success": func(c *Capture, o *ProcessOptions) { c.Status = 500; o.Previous = prev },
		"soft_404": func(c *Capture, o *ProcessOptions) {
			c.Body = []byte(`<html><head><title>Page Not Found</title></head><body>gone</body></html>`)
			o.Previous = prev
		},
		"mass_removal": func(c *Capture, o *ProcessOptions) {
			c.Body = []byte(`<html><body><a href="/products">Products</a></body></html>`)
			o.Previous = prev
		},
		"catch_all": func(c *Capture, o *ProcessOptions) {
			h := BodySimHash(good.Main().Normalized.Text)
			o.HostBodies = []HostBody{{URL: "https://shop.example/a", SimHash: h}, {URL: "https://shop.example/b", SimHash: h ^ 1}}
		},
		"truncated":    func(c *Capture, o *ProcessOptions) { c.Truncated = true },
		"fetch_failed": func(c *Capture, o *ProcessOptions) { c.Status = 0; c.Body = nil },
		"collector_partial": func(c *Capture, o *ProcessOptions) {
			c.Outcome = OutcomePartial
		},
	}
}

// CONTENT-INV-06: a degraded, failed, truncated or partial capture never
// produces coverage complete, and its observation never withdraws
// (authority coverage is not complete). Degraded captures get no diff.
func TestContentInv06_DegradedNeverComplete(t *testing.T) {
	good := spaPage(t, "v1", t0)
	if good.Coverage != oam.CoverageComplete {
		t.Fatalf("control capture is not complete: %+v", good.Integrity)
	}
	for name, mutate := range degradations(t) {
		c := capture("https://shop.example/", site(t, "spa", "v1.index.html"), headerFile(t, "spa", "v1.headers.txt"), t0.AddDate(0, 0, 1))
		var opts ProcessOptions
		mutate(c, &opts)
		p := mustProcess(t, c, opts)
		if p.Coverage == oam.CoverageComplete {
			t.Fatalf("%s: coverage complete (integrity %+v)", name, p.Integrity)
		}
		if p.Integrity.Outcome == OutcomeComplete {
			t.Fatalf("%s: outcome complete", name)
		}
		o, err := Observe(p, ObserveOptions{Hasher: testHasher})
		if err != nil {
			t.Fatalf("%s: observation invalid: %v", name, err)
		}
		if o.Authority == nil || o.Authority.Coverage == oam.CoverageComplete {
			t.Fatalf("%s: observation withdraws: %+v", name, o.Authority)
		}
		if p.Integrity.Degraded() {
			cs, err := Compare(good, p, DiffOptions{})
			if err != nil || cs.Outcome != DiffDegraded || len(cs.Artifacts) != 0 || cs.IsChange() {
				t.Fatalf("%s: degraded capture was diffed: %+v %v", name, cs, err)
			}
		}
	}
}

// CONTENT-INV-07: an extractor grade other than complete makes coverage
// not complete, and the families of that extractor are not covered.
func TestContentInv07_PartialGradeNotComplete(t *testing.T) {
	r := testkit.Rng(t)
	cases := map[string]*Capture{
		"inline_js_syntax_error": capture("https://p.example/", []byte(`<a href="/x">x</a><script>var a = ;fetch("/api/y")</script>`), nil, t0),
		"bundle_syntax_error":    capture("https://p.example/a.js", []byte(`fetch("/api/y"); var = 1;`), []Header{{"Content-Type", "application/javascript"}}, t0),
		"html_attribute_limit":   capture("https://p.example/", []byte(`<div `+strings.Repeat(`a=1 `, 0)+manyAttrs(1100)+`></div><a href="/z">z</a>`), nil, t0),
		"fact_limit":             capture("https://p.example/", []byte(manyLinks(r, MaxFactsPerParser+5)), nil, t0),
		"bad_sourcemap":          capture("https://p.example/a.js.map", []byte(`{"version":3,"sources":["a.ts"],"mappings":"!!!"}`), nil, t0),
	}
	for name, c := range cases {
		p := mustProcess(t, c, ProcessOptions{})
		var bad []string
		for _, a := range p.Artifacts {
			for _, g := range a.Features.Grades {
				if g.Grade != GradeComplete {
					bad = append(bad, g.Extractor)
				}
			}
		}
		if len(bad) == 0 {
			t.Fatalf("%s: no extractor graded below complete", name)
		}
		if p.Coverage == oam.CoverageComplete {
			t.Fatalf("%s: coverage complete with partial grades %v", name, bad)
		}
		for _, b := range bad {
			for _, e := range Extractors() {
				if e.ID() != b {
					continue
				}
				for _, f := range e.Families {
					if contains(p.CoveredRules, f) {
						t.Fatalf("%s: family %s covered although %s graded partial", name, f, b)
					}
				}
			}
		}
	}
}

func manyAttrs(n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString("a" + itoa(i) + "=1 ")
	}
	return b.String()
}

func manyLinks(r core.Rng, n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString(`<a href="/p/` + itoa(i) + randToken(r, 2, "ab") + `">x</a>`)
	}
	return b.String()
}

// CONTENT-INV-14: coverage is complete exactly when the capture outcome is
// complete, the body normalized completely, every extractor that ran
// graded complete and none is disabled by a failed canary.
func TestContentInv14_CoverageCompleteIff(t *testing.T) {
	r := testkit.Rng(t)
	for range 60 {
		body := []byte(genHTML(r))
		var disabled []string
		if core.IntN(r, 3) == 0 {
			disabled = []string{pick(r, Extractors()).ID()}
		}
		p := mustProcess(t, capture("https://g.example/", body, nil, t0), ProcessOptions{DisabledExtractors: disabled})
		want := p.Integrity.Outcome == OutcomeComplete && p.Main().Normalized.Grade == GradeComplete
		for _, a := range p.Artifacts {
			for _, g := range a.Features.Grades {
				want = want && g.Grade == GradeComplete
			}
		}
		kinds := []Kind{p.Main().Kind, KindHeaders}
		if p.Main().Kind == KindHTML {
			kinds = append(kinds, KindJS, KindCSS)
		}
		for _, d := range disabled {
			for _, k := range kinds {
				for _, e := range ExtractorsFor(k) {
					if e.ID() == d {
						want = false
					}
				}
			}
		}
		if got := p.Coverage == oam.CoverageComplete; got != want {
			t.Fatalf("coverage complete = %v, want %v (disabled %v, outcome %s)", got, want, disabled, p.Integrity.Outcome)
		}
	}
}

// CONTENT-INV-16: every extractor has exactly one canary, every canary
// passes, and a disabled extractor's families are never covered.
func TestContentInv16_Canaries(t *testing.T) {
	results := RunCanaries()
	seen := map[string]int{}
	for _, r := range results {
		seen[r.Extractor]++
		if !r.OK {
			t.Errorf("canary %s failed: missing %v, grade %s", r.Extractor, r.Missing, r.Grade)
		}
	}
	for _, e := range Extractors() {
		if seen[e.ID()] != 1 {
			t.Errorf("extractor %s has %d canaries", e.ID(), seen[e.ID()])
		}
	}
	if len(DisabledByCanaries(results)) != 0 {
		t.Fatal("canaries disabled extractors")
	}
	p := mustProcess(t, capture("https://shop.example/", site(t, "spa", "v1.index.html"), nil, t0),
		ProcessOptions{DisabledExtractors: []string{"html.links@1"}})
	if p.Coverage == oam.CoverageComplete || contains(p.CoveredRules, oam.FamilyLinks) {
		t.Fatalf("disabled extractor still covered: %s %v", p.Coverage, p.CoveredRules)
	}
}
