package content

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/jsparse"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/sourcemap"
)

// Artifact is one normalized, extracted document of a capture: the
// response body, a derived document, or a headers block.
type Artifact struct {
	// DocID is the requested URL, plus "#script-n", "#style-n",
	// "#handlers" or "#src/<path>" for inner documents, "#headers" for the
	// final response's header block, and "#hop/<n>/headers" for the n-th
	// redirect response (1-based). Hops need their own suffix: the first
	// hop's URL is the requested URL, so "<hop url>#headers" would collide
	// with the final block and pair the two in the next version's diff.
	DocID      string `json:"doc_id"`
	Kind       Kind   `json:"kind"`
	BodySHA256 string `json:"body_sha256"`
	// Size is the raw body length in bytes.
	Size int `json:"size"`
	// Normalized holds the normalized text after Redact (V2 §12.11: what
	// is stored, hashed for the index, indexed or logged never carries a
	// credential).
	Normalized Normalized `json:"normalized"`
	// NormalizedSHA256 is the SHA-256 of Normalized.Text.
	NormalizedSHA256 string      `json:"normalized_sha256"`
	Features         FeatureSet  `json:"features"`
	Modules          []Module    `json:"modules,omitempty"`
	Fingerprint      Fingerprint `json:"fingerprint"`
}

// ExtractorIDs returns the sorted extractor ids that graded this
// artifact (the "parser" side of cause detection).
func (a *Artifact) ExtractorIDs() []string {
	var out []string
	for _, g := range a.Features.Grades {
		out = append(out, g.Extractor)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Redirect is one observed redirect hop.
type Redirect struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Status int    `json:"status"`
}

// Processed is a capture turned into artifacts.
type Processed struct {
	URL       string      `json:"url"`
	Profile   string      `json:"profile"`
	FetchedAt time.Time   `json:"fetched_at"`
	Status    int         `json:"status"`
	Sniff     SniffResult `json:"sniff"`
	// Artifacts: [0] the body, then its derived documents in document
	// order, then the header blocks (final response, then each hop).
	Artifacts []Artifact      `json:"artifacts"`
	Redirects []Redirect      `json:"redirects,omitempty"`
	Integrity IntegrityReport `json:"integrity"`
	// Coverage and CoveredRules are the authority of this reading (V2
	// §3.4): complete only when the capture is complete and every
	// extractor that ran graded complete.
	Coverage     oam.Coverage `json:"coverage"`
	CoveredRules []string     `json:"covered_rules,omitempty"`
	// SimHash is BodySimHash of the normalized body (pages, JSON, XML
	// and text only; 0 otherwise).
	SimHash   uint64 `json:"simhash"`
	ErrorPage bool   `json:"error_page,omitempty"`
}

// Main returns the body artifact.
func (p *Processed) Main() *Artifact { return &p.Artifacts[0] }

// Features returns every feature of the capture's body and derived
// documents (not of hop header blocks), sorted and unique by id.
func (p *Processed) Features() []Feature {
	var fs []Feature
	for i := range p.Artifacts {
		a := &p.Artifacts[i]
		if a.Kind == KindHeaders && a.DocID != p.URL+"#headers" {
			continue
		}
		fs = append(fs, a.Features.Features...)
	}
	// One fact reported by several documents (a secret in an inline
	// script is in the page too) is one feature: the first in sorted
	// order, which is the one with the shortest doc suffix.
	return slices.CompactFunc(sortFeatures(fs), func(a, b Feature) bool { return a.ID() == b.ID() && a.sameContent(b) })
}

// FeatureIDs returns the sorted, unique ids of Features.
func (p *Processed) FeatureIDs() []string {
	fs := p.Features()
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.ID())
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Findings returns every high-tier secret finding of the capture.
func (p *Processed) Findings() []SecretFinding {
	var out []SecretFinding
	seen := map[string]bool{}
	for i := range p.Artifacts {
		for _, f := range p.Artifacts[i].Features.Findings {
			k := f.RuleID + "\x00" + string(f.Secret)
			if !seen[k] {
				seen[k] = true
				out = append(out, f)
			}
		}
	}
	return out
}

// ProcessOptions configures Process.
type ProcessOptions struct {
	Learned VolatilitySet
	Hasher  KeyedHasher
	Limits  Limits
	// Previous and HostBodies feed the integrity checks.
	Previous   *PreviousCapture
	HostBodies []HostBody
	// DisabledExtractors are extractor ids whose canary failed
	// (DisabledByCanaries): they run, but their families are never
	// covered, so they cannot withdraw.
	DisabledExtractors []string
	Splitter           SplitOptions
	Lineage            LineageAlgo
	// PageURL is the page that loads a script capture: relative fetch()
	// URLs resolve against it. Empty: the capture URL.
	PageURL string
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Process sniffs, normalizes, extracts and checks one capture. It returns
// an error only for an invalid capture (ErrInvalidCapture); unreadable
// content is graded, never fatal.
func Process(c *Capture, opts ProcessOptions) (*Processed, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	prof, _ := c.Profile.Normalize()
	lim := opts.Limits.withDefaults()
	opts.Limits = lim
	body := c.Body
	if len(body) > lim.MaxResponseBytes {
		body = body[:lim.MaxResponseBytes]
	}
	ct, _ := c.Header("content-type")
	_, params, _ := strings.Cut(ct, ";")
	charset := ""
	for p := range strings.SplitSeq(params, ";") {
		if k, v, ok := strings.Cut(strings.TrimSpace(p), "="); ok && strings.EqualFold(k, "charset") {
			charset = strings.Trim(v, `"`)
		}
	}
	p := &Processed{URL: c.URL, Profile: prof.Name, FetchedAt: c.FetchedAt, Status: c.Status}
	p.Sniff = Sniff(body, ct, c.URL)
	// RFC 3986 §5.1.3: the base URI of a retrieved document is the URI it
	// was retrieved from, which after redirects is the final one, not the
	// requested one (http://h/ -> https://h/ must resolve "/a" against
	// https). A script's references resolve against the page that loads it.
	base := opts.PageURL
	if base == "" {
		base = c.FinalURL()
	}
	main := buildArtifact(c.URL, base, p.Sniff.Kind, body, charset, p.Sniff.Disclosure, c.FetchedAt, opts)
	if len(c.Body) > lim.MaxResponseBytes || c.Truncated {
		main.Normalized.Grade = worseGrade(main.Normalized.Grade, GradePartial)
	}
	p.Artifacts = append(p.Artifacts, main)
	derived := main.Normalized.Derived
	if p.Sniff.Kind == KindSourceMap {
		derived = append(derived, sourceMapOriginals(body, lim)...)
	}
	for i, d := range derived {
		if i >= lim.MaxInnerDocuments {
			break
		}
		p.Artifacts = append(p.Artifacts, buildArtifact(c.URL+d.Suffix, base, d.Kind, d.Body, "", "", c.FetchedAt, opts))
	}
	p.Artifacts = append(p.Artifacts, buildArtifact(c.URL+"#headers", base, KindHeaders, HeaderBlock(c.Headers), "", "", c.FetchedAt, opts))
	for i, h := range c.RedirectChain {
		p.Artifacts = append(p.Artifacts, buildArtifact(c.URL+"#hop/"+strconv.Itoa(i+1)+"/headers", h.URL, KindHeaders, HeaderBlock(h.Headers), "", "", c.FetchedAt, opts))
		if loc, ok := headerValue(h.Headers, "location"); ok {
			if to, ok := resolveRef(h.URL, loc); ok {
				p.Redirects = append(p.Redirects, Redirect{From: h.URL, To: to, Status: h.Status})
			}
		}
	}
	switch main.Kind {
	case KindHTML, KindJSON, KindText, KindXML:
		// Catch-all and soft-404 detection compare page-like bodies
		// across a host's URLs; scripts and styles are not answered by a
		// catch-all route, so their (large) sketch is not computed.
		p.SimHash = BodySimHash(main.Normalized.Text)
	}
	p.ErrorPage = LooksLikeErrorPage(body)
	p.Integrity = CheckIntegrity(IntegrityInput{Capture: c, Body: body, FeatureIDs: p.FeatureIDs(), SimHash: p.SimHash,
		Previous: opts.Previous, HostBodies: opts.HostBodies})
	p.Coverage, p.CoveredRules = coverage(p, opts.DisabledExtractors)
	return p, nil
}

// buildArtifact normalizes, redacts and extracts one document.
func buildArtifact(docID, base string, k Kind, body []byte, charset, disclosure string, at time.Time, opts ProcessOptions) Artifact {
	a := Artifact{DocID: docID, Kind: k, BodySHA256: sha256Hex(body), Size: len(body)}
	nopts := NormalizeOptions{Learned: opts.Learned, Charset: charset, Limits: opts.Limits}
	finish := func(prog *jsparse.Program) {
		a.Normalized = normalizeParsed(k, body, nopts, prog)
		a.Normalized.Text, _ = Redact(a.Normalized.Text)
		a.NormalizedSHA256 = sha256Hex(a.Normalized.Text)
	}
	suffix := ""
	if i := strings.IndexByte(docID, '#'); i >= 0 {
		suffix = docID[i:]
	}
	url := strings.TrimSuffix(docID, suffix)
	in := ExtractInput{URL: url, BaseURL: base, Kind: k, Doc: suffix, Body: body, Charset: charset, FetchedAt: at,
		Disclosure: disclosure, Hasher: opts.Hasher, Limits: opts.Limits}
	if k == KindJS {
		prog := parseJS(body, true)
		x := &extraction{in: in}
		x.in.Limits = opts.Limits
		x.js(prog)
		x.secrets()
		x.disclosure()
		a.Features = x.result()
		sp := splitter{prog: prog, bindings: bindingNames(prog)}
		a.Modules = SplitModules(prog, opts.Splitter)
		a.Fingerprint = FingerprintOf(sp.canonicalTokens(0, len(prog.Source), nil, true), len(body), opts.Lineage)
		finish(prog) // last: normalization masks the program in place
		return a
	}
	finish(nil)
	a.Features = Extract(in)
	if k == KindCSS || k == KindTSText {
		a.Fingerprint = FingerprintOf(LineageTokens(k, a.Normalized.Text), len(body), opts.Lineage)
	}
	return a
}

// sourceMapOriginals returns the embedded original sources of a source
// map as derived documents "#src/<path>" (V2 §12.6 "diff original
// sources"). By decision D2, .ts/.tsx/.jsx/.mts/.cts originals are text;
// .js originals are JavaScript; .css are CSS.
func sourceMapOriginals(body []byte, lim Limits) []Derived {
	m, _ := sourcemap.Parse(body, sourcemap.Options{MaxInputBytes: lim.MaxResponseBytes})
	if m == nil {
		return nil
	}
	var out []Derived
	for _, s := range m.Sources {
		if !s.HasContent || s.Null || len(out) >= lim.MaxInnerDocuments {
			continue
		}
		k := KindText
		switch strings.ToLower(path.Ext(s.URL)) {
		case ".ts", ".tsx", ".jsx", ".mts", ".cts":
			k = KindTSText
		case ".js", ".mjs", ".cjs":
			k = KindJS
		case ".css":
			k = KindCSS
		}
		name := strings.TrimLeft(strings.ReplaceAll(s.URL, "webpack://", ""), "/")
		out = append(out, Derived{Suffix: "#src/" + name, Kind: k, Body: []byte(s.Content)})
	}
	return out
}

// coverage decides the authority of a reading (V2 §3.4, §12.1, §12.10).
func coverage(p *Processed, disabled []string) (oam.Coverage, []string) {
	switch {
	case p.Integrity.Outcome == OutcomeFailed || p.Integrity.Outcome == OutcomeDegraded:
		return oam.CoverageFailed, nil
	case p.Sniff.Kind == KindBinary:
		return oam.CoverageUnsupported, nil
	}
	main := p.Main()
	// Families this reading is responsible for: those of the body's
	// extractors, plus (for a page) those of its inline scripts and
	// styles, whose full set the page parse determines.
	kinds := []Kind{main.Kind, KindHeaders}
	if main.Kind == KindHTML {
		kinds = append(kinds, KindJS, KindCSS)
	}
	fam := map[string]bool{}
	bad := map[string]bool{}
	for _, k := range kinds {
		for _, e := range ExtractorsFor(k) {
			for _, f := range e.Families {
				fam[f] = true
				if slices.Contains(disabled, e.ID()) {
					bad[f] = true
				}
			}
		}
	}
	allComplete := p.Integrity.Outcome == OutcomeComplete && main.Normalized.Grade == GradeComplete
	for i := range p.Artifacts {
		a := &p.Artifacts[i]
		for _, g := range a.Features.Grades {
			if g.Grade == GradeComplete && !slices.Contains(disabled, g.Extractor) {
				continue
			}
			allComplete = false
			for _, e := range Extractors() {
				if e.ID() == g.Extractor {
					for _, f := range e.Families {
						bad[f] = true
					}
				}
			}
		}
	}
	fam[oam.FamilyRedirects] = true
	var covered []string
	for f := range fam {
		if !bad[f] {
			covered = append(covered, f)
		}
	}
	slices.Sort(covered)
	if allComplete && len(bad) == 0 {
		return oam.CoverageComplete, covered
	}
	return oam.CoveragePartial, covered
}
