package content

import (
	"slices"
	"strconv"
	"time"
)

// ExtractorInfo describes one extractor: its versioned name, the kinds it
// reads and the rule families it produces.
type ExtractorInfo struct {
	Name     string   `json:"name"`
	Version  int      `json:"version"`
	Kinds    []Kind   `json:"kinds"`
	Families []string `json:"families"`
}

// ID is "<name>@<version>".
func (e ExtractorInfo) ID() string { return e.Name + "@" + strconv.Itoa(e.Version) }

// Extractor names.
const (
	ExHTMLLinks   = "html.links"
	ExHTMLScripts = "html.scripts"
	ExHTMLStyles  = "html.styles"
	ExHTMLFrames  = "html.frames"
	ExHTMLForms   = "html.forms"
	ExHTMLMeta    = "html.meta"
	ExJSEndpoints = "js.endpoints"
	ExJSGraphQL   = "js.graphql"
	ExJSSourceMap = "js.sourcemap"
	ExJSLibraries = "js.libraries"
	ExJSModules   = "js.modules"
	ExCSSURLs     = "css.urls"
	ExHeaders     = "headers.security"
	ExRobots      = "wellknown.robots"
	ExSecurityTxt = "wellknown.securitytxt"
	ExSitemap     = "xml.sitemap"
	ExSourceMap   = "sourcemap.sources"
	ExOpenAPI     = "schema.openapi"
	ExGraphQLSDL  = "schema.graphql"
	ExSecrets     = "secrets"
	ExDisclosure  = "disclosure"
)

var textKinds = []Kind{KindHTML, KindJS, KindCSS, KindJSON, KindXML, KindSitemap, KindSourceMap, KindRobots,
	KindSecurityTxt, KindTSText, KindText, KindServerSource}

// Extractors lists every extractor in a fixed order.
func Extractors() []ExtractorInfo {
	f := func(k ...FeatureKind) []string {
		var out []string
		for _, x := range k {
			out = append(out, FamilyOf(x))
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	html := []Kind{KindHTML}
	js := []Kind{KindJS}
	return []ExtractorInfo{
		{ExHTMLLinks, 1, html, f(FeatLink)},
		{ExHTMLScripts, 1, html, f(FeatScript)},
		{ExHTMLStyles, 1, html, f(FeatStyle)},
		{ExHTMLFrames, 1, html, f(FeatFrame)},
		{ExHTMLForms, 1, html, f(FeatForm, FeatParameter)},
		{ExHTMLMeta, 1, html, f(FeatGenerator)},
		{ExJSEndpoints, 1, js, f(FeatEndpoint, FeatParameter)},
		{ExJSGraphQL, 1, js, f(FeatGraphQLOperation)},
		{ExJSSourceMap, 1, js, f(FeatSourceMap)},
		{ExJSLibraries, 1, js, f(FeatLibrary)},
		{ExJSModules, 1, js, f(FeatModuleImport)},
		{ExCSSURLs, 1, []Kind{KindCSS}, f(FeatCSSImport, FeatCSSAsset)},
		{ExHeaders, 1, []Kind{KindHeaders}, f(FeatHeader)},
		{ExRobots, 1, []Kind{KindRobots}, f(FeatRobotsRule, FeatLink)},
		{ExSecurityTxt, 1, []Kind{KindSecurityTxt}, f(FeatSecurityField)},
		{ExSitemap, 1, []Kind{KindSitemap}, f(FeatSitemapEntry)},
		{ExSourceMap, 1, []Kind{KindSourceMap}, f(FeatSourceMapSource)},
		{ExOpenAPI, 1, []Kind{KindJSON}, f(FeatSchemaEndpoint, FeatParameter)},
		{ExGraphQLSDL, 1, []Kind{KindJSON, KindText}, f(FeatSchemaEndpoint, FeatGraphQLOperation)},
		{ExSecrets, 1, textKinds, f(FeatSecret)},
		{ExDisclosure, 1, append(slices.Clone(textKinds), KindBinary), f(FeatSourceDisclosure)},
	}
}

// ExtractorsFor returns the extractors that read a kind, in table order.
func ExtractorsFor(k Kind) []ExtractorInfo {
	var out []ExtractorInfo
	for _, e := range Extractors() {
		if slices.Contains(e.Kinds, k) {
			out = append(out, e)
		}
	}
	return out
}

// ExtractInput is one document to extract from.
type ExtractInput struct {
	// URL is the document's own URL; BaseURL (default URL) resolves
	// relative references (a page's <base href>, or the page that loads a
	// script, since fetch() resolves against the document).
	URL     string
	BaseURL string
	Kind    Kind
	// Doc is the derived-document suffix ("" for the artifact itself).
	Doc string
	// Body is the raw document (KindHeaders: a HeaderBlock).
	Body    []byte
	Charset string
	// FetchedAt is the capture time (security.txt Expires is checked
	// against it; the clock is never read).
	FetchedAt time.Time
	// Disclosure is the server-side language Sniff found, if any.
	Disclosure string
	// Hasher keys secret features (nil: rule, preview and length).
	Hasher KeyedHasher
	Limits Limits
}

// Extract runs every extractor of in.Kind over one document. It never
// fails: each extractor reports its grade, and "found nothing" (complete,
// zero facts) is distinct from "could not read" (failed). Features are
// sorted and capped at Limits.MaxFactsPerParser per extractor.
func Extract(in ExtractInput) FeatureSet {
	if in.BaseURL == "" {
		in.BaseURL = in.URL
	}
	in.Limits = in.Limits.withDefaults()
	x := &extraction{in: in}
	if len(in.Body) > in.Limits.MaxResponseBytes {
		in.Body = in.Body[:in.Limits.MaxResponseBytes]
		x.in = in
		x.truncated = true
	}
	switch in.Kind {
	case KindHTML:
		x.html()
	case KindJS:
		x.js(parseJS(in.Body, false))
	case KindCSS:
		x.css()
	case KindHeaders:
		x.headers()
	case KindRobots:
		x.robots()
	case KindSecurityTxt:
		x.securityTxt()
	case KindSitemap:
		x.sitemap()
	case KindSourceMap:
		x.sourceMap()
	case KindJSON:
		x.jsonSchemas()
	case KindText:
		x.graphqlSDL(in.Body)
	}
	if slices.Contains(textKinds, in.Kind) {
		x.secrets()
	}
	if in.Kind != KindHeaders {
		x.disclosure()
	}
	return x.result()
}

// extraction accumulates the features and grades of one document.
type extraction struct {
	in        ExtractInput
	truncated bool
	features  []Feature
	grades    []ExtractorGrade
	findings  []SecretFinding
}

// report records one extractor's output: its features (capped) and grade.
func (x *extraction) report(name string, g Grade, reason string, fs []Feature) {
	lim := x.in.Limits.MaxFactsPerParser
	if len(fs) > lim {
		fs = fs[:lim]
		g, reason = worseGrade(g, GradePartial), "fact_limit"
	}
	if x.truncated && g == GradeComplete {
		g, reason = GradePartial, "input_truncated"
	}
	for i := range fs {
		fs[i].Doc = x.in.Doc
	}
	x.features = append(x.features, fs...)
	x.grades = append(x.grades, ExtractorGrade{Extractor: name + "@1", Doc: x.in.Doc, Grade: g, Facts: len(fs), Reason: reason})
}

func (x *extraction) result() FeatureSet {
	return FeatureSet{Features: sortFeatures(x.features), Grades: x.grades, Findings: x.findings}
}

// disclosure reports server-side source (V2 §12.9).
func (x *extraction) disclosure() {
	var fs []Feature
	if x.in.Disclosure != "" {
		fs = append(fs, newFeature(FeatSourceDisclosure, x.in.Disclosure, "server-side source served as content",
			Attr{"language", x.in.Disclosure}))
	}
	x.report(ExDisclosure, GradeComplete, "", fs)
}

// secrets runs the named rules over the raw text. Only high-tier findings
// become features; low-tier (entropy) candidates are kept for redaction
// and audit but are never a finding on their own (V2 §3.3).
func (x *extraction) secrets() {
	all := ScanSecrets(x.in.Body, x.in.Doc)
	var fs []Feature
	for _, f := range all {
		if f.Tier != TierHigh {
			continue
		}
		x.findings = append(x.findings, f)
		fs = append(fs, newFeature(FeatSecret, secretKey(f, x.in.Hasher), f.Preview, Attr{"rule", f.RuleID}))
	}
	x.report(ExSecrets, GradeComplete, "", fs)
}

// secretKey identifies a secret without revealing it: the rule and the
// tenant-keyed hash, or (without a hasher) the rule, preview and length.
func secretKey(f SecretFinding, h KeyedHasher) string {
	if h != nil {
		return f.RuleID + ":" + h.KeyedHash(f.Secret)
	}
	return f.RuleID + ":" + f.Preview + ":" + strconv.Itoa(len(f.Secret))
}
