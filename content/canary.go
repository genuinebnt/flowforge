package content

import (
	"slices"
	"time"
)

// canary is one extractor's canary fixture (V2 §12.10): a tiny document
// whose expected features are fixed. Canaries run at startup and in CI;
// an extractor whose canary fails is disabled for withdrawal purposes.
type canary struct {
	extractor string // extractor id
	kind      Kind
	url       string
	body      string
	want      []string // feature ids that must be present
}

const canaryURL = "https://canary.example/"

var canaries = []canary{
	{"html.links@1", KindHTML, canaryURL, `<a href="/about">a</a>`, []string{"link|https://canary.example/about"}},
	{"html.scripts@1", KindHTML, canaryURL, `<script src="/app.0123abcd.js"></script>`, []string{"script|https://canary.example/app.__FF_HASH__.js"}},
	{"html.styles@1", KindHTML, canaryURL, `<link rel="stylesheet" href="/s.css">`, []string{"style|https://canary.example/s.css"}},
	{"html.frames@1", KindHTML, canaryURL, `<iframe src="https://f.example/x"></iframe>`, []string{"frame|https://f.example/x"}},
	{"html.forms@1", KindHTML, canaryURL, `<form method="post" action="/login"><input name="user"></form>`,
		[]string{"form|ep:POST https://canary.example/login", "parameter|ep:POST https://canary.example/login body:user"}},
	{"html.meta@1", KindHTML, canaryURL, `<meta name="generator" content="WordPress 6.4.2">`, []string{"generator|generator"}},
	{"js.endpoints@1", KindJS, canaryURL + "a.js", `fetch("/api/v1/items", {method: "POST"});`, []string{"endpoint|ep:POST https://canary.example/api/v1/items"}},
	{"js.graphql@1", KindJS, canaryURL + "a.js", "const q = `query GetUser { user { id } }`;", []string{"graphql_operation|query GetUser"}},
	{"js.sourcemap@1", KindJS, canaryURL + "a.js", "x();\n//# sourceMappingURL=a.js.map\n", []string{"sourcemap|https://canary.example/a.js.map"}},
	{"js.libraries@1", KindJS, canaryURL + "a.js", "/*! jQuery v3.7.1 | (c) OpenJS Foundation */\nvar a=1;", []string{"library|jquery"}},
	{"js.modules@1", KindJS, canaryURL + "a.js", `import("./chunk.js");`, []string{"module_import|https://canary.example/chunk.js"}},
	{"css.urls@1", KindCSS, canaryURL + "s.css", `@import url("/base.css"); a{background:url(/i.png)}`,
		[]string{"css_import|https://canary.example/base.css", "css_asset|https://canary.example/i.png"}},
	{"headers.security@1", KindHeaders, canaryURL, "Content-Security-Policy: default-src 'self'\nSet-Cookie: sid=1; Secure\n",
		[]string{"header|content-security-policy", "header|set-cookie:sid"}},
	{"wellknown.robots@1", KindRobots, canaryURL + "robots.txt", "User-agent: *\nDisallow: /admin\n", []string{"robots_rule|disallow * /admin"}},
	{"wellknown.securitytxt@1", KindSecurityTxt, canaryURL + ".well-known/security.txt", "Contact: mailto:sec@canary.example\nExpires: 2099-01-01T00:00:00Z\n",
		[]string{"security_field|contact:mailto:sec@canary.example"}},
	{"xml.sitemap@1", KindSitemap, canaryURL + "sitemap.xml", `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"><url><loc>https://canary.example/p</loc></url></urlset>`,
		[]string{"sitemap_entry|https://canary.example/p"}},
	{"sourcemap.sources@1", KindSourceMap, canaryURL + "a.js.map", `{"version":3,"sources":["src/a.ts"],"names":[],"mappings":"AAAA"}`,
		[]string{"sourcemap_source|https://canary.example/src/a.ts"}},
	{"schema.openapi@1", KindJSON, canaryURL + "openapi.json", `{"openapi":"3.0.0","paths":{"/users/{id}":{"get":{}}}}`,
		[]string{"schema_endpoint|ep:GET https://canary.example/users/{id}"}},
	{"schema.graphql@1", KindText, canaryURL + "schema.graphql", "type Query {\n  me: User\n}\n", []string{"graphql_operation|query me"}},
	{"secrets@1", KindText, canaryURL + "x.txt", "key=AKIAIOSFODNN7EXAMPLE\n", []string{"secret|aws_access_key_id:AKIA...LE:20"}},
	{"disclosure@1", KindServerSource, canaryURL + "x.php", "<?php echo 1; ?>", []string{"source_disclosure|php"}},
}

// CanaryResult is the outcome of one extractor's canary.
type CanaryResult struct {
	Extractor string   `json:"extractor"`
	OK        bool     `json:"ok"`
	Missing   []string `json:"missing,omitempty"`
	Grade     Grade    `json:"grade"`
}

// RunCanaries runs every extractor's canary fixture. Every extractor in
// Extractors has exactly one canary (TestContentCanaries checks both).
func RunCanaries() []CanaryResult {
	var out []CanaryResult
	for _, c := range canaries {
		disc := ""
		if c.kind == KindServerSource {
			disc = Sniff([]byte(c.body), "", c.url).Disclosure
		}
		fs := Extract(ExtractInput{URL: c.url, Kind: c.kind, Body: []byte(c.body), Disclosure: disc,
			FetchedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
		ids := fs.FeatureIDs()
		r := CanaryResult{Extractor: c.extractor, Grade: GradeFailed}
		if g, ok := fs.gradeOf(c.extractor); ok {
			r.Grade = g
		}
		for _, w := range c.want {
			if _, ok := slices.BinarySearch(ids, w); !ok {
				r.Missing = append(r.Missing, w)
			}
		}
		r.OK = len(r.Missing) == 0 && r.Grade == GradeComplete
		out = append(out, r)
	}
	return out
}

// DisabledByCanaries returns the ids of extractors whose canary failed,
// for ProcessOptions.DisabledExtractors.
func DisabledByCanaries(rs []CanaryResult) []string {
	var out []string
	for _, r := range rs {
		if !r.OK {
			out = append(out, r.Extractor)
		}
	}
	return out
}
