package content

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/genuinebnt/flowforge/jsparse"
)

// NormalizerInfo identifies one normalizer: its name, version and rule
// table. The rule table is a list of portable rule descriptions; its
// SHA-256 (over the lines joined by "\n") changes whenever a rule
// changes, and the version must then be bumped.
type NormalizerInfo struct {
	Name            string   `json:"name"`
	Version         int      `json:"version"`
	Rules           []string `json:"rules"`
	RuleTableSHA256 string   `json:"rule_table_sha256"`
}

// ID is the normalizer_version "<name>@<n>" (V2 §5).
func (n NormalizerInfo) ID() string { return n.Name + "@" + strconv.Itoa(n.Version) }

func ruleTableSHA(lines []string) string {
	s := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(s[:])
}

func info(name string, version int, rules []string) NormalizerInfo {
	return NormalizerInfo{Name: name, Version: version, Rules: rules, RuleTableSHA256: ruleTableSHA(rules)}
}

// Normalizers lists every normalizer with its version and rule-table
// hash, in a fixed order. conformance/content/normalizers pins the table
// (TestContentConformanceNormalizers) and README.md lists it
// (TestContentREADME).
func Normalizers() []NormalizerInfo {
	return []NormalizerInfo{
		info("html", 1, htmlRules), info("js", 1, jsRules), info("css", 1, cssRules), info("json", 1, jsonRules),
		info("xml", 1, xmlRules), info("headers", 1, headerRules), info("text", 1, textRules), info("binary", 1, binaryRules),
	}
}

// NormalizerFor returns the normalizer of a kind.
func NormalizerFor(k Kind) NormalizerInfo {
	all := Normalizers()
	switch k {
	case KindHTML:
		return all[0]
	case KindJS:
		return all[1]
	case KindCSS:
		return all[2]
	case KindJSON, KindSourceMap:
		return all[3]
	case KindXML, KindSitemap:
		return all[4]
	case KindHeaders:
		return all[5]
	case KindBinary:
		return all[7]
	}
	return all[6]
}

// Derived is a document inside a document (V2 §12.7): an inline script,
// an inline style, the page's event handlers, or a source-map original.
// Its id is the parent's id plus Suffix ("#script-1", "#style-2",
// "#handlers", "#src/app.ts").
type Derived struct {
	Suffix string `json:"suffix"`
	Kind   Kind   `json:"kind"`
	Body   []byte `json:"body"`
}

// Normalized is the output of Normalize.
type Normalized struct {
	Kind Kind `json:"kind"`
	// Version is the normalizer_version: "<name>@<n>", or
	// "<name>.learned<k>@<n>" when a learned VolatilitySet k was applied.
	Version         string `json:"version"`
	RuleTableSHA256 string `json:"rule_table_sha256"`
	// Text is the normalized text: UTF-8, LF line ends, one logical unit
	// (element, statement, declaration, member, header) per line, ending
	// in "\n" unless empty.
	Text  []byte `json:"text"`
	Grade Grade  `json:"grade"`
	// Diagnostics are stable codes of what made the grade worse.
	Diagnostics []string `json:"diagnostics,omitempty"`
	// Masks counts masked values by class; MaskDigest is the SHA-256 over
	// the masked originals, so equal texts with different digests are a
	// masked_only change.
	Masks      []MaskCount `json:"masks,omitempty"`
	MaskDigest string      `json:"mask_digest"`
	// Derived lists the inner documents, in document order (at most
	// MaxInnerDocuments).
	Derived []Derived `json:"derived,omitempty"`
}

// NormalizeOptions configures Normalize.
type NormalizeOptions struct {
	// Learned is the frozen learned volatility set (never applied
	// implicitly). It must Validate; an invalid set is ignored and
	// reported as the diagnostic "learned_rules_invalid".
	Learned VolatilitySet
	// Charset is the transport charset (Content-Type parameter) for HTML.
	Charset string
	Limits  Limits
}

// Normalize normalizes a body of the given kind (V2 §5, §12.7, §12.8).
// It is total (any bytes give a result, never a panic), deterministic and
// idempotent: Normalize(k, Normalize(k, x).Text).Text equals
// Normalize(k, x).Text (CONTENT-INV-01). Bodies above
// Limits.MaxResponseBytes are cut there and graded partial.
func Normalize(k Kind, body []byte, opts NormalizeOptions) Normalized {
	return normalizeParsed(k, body, opts, nil)
}

// normalizeParsed is Normalize with an optional JavaScript program already
// parsed from body (the pipeline parses a bundle once for extraction,
// module splitting and normalization; normalization masks the program in
// place, so it runs last).
func normalizeParsed(k Kind, body []byte, opts NormalizeOptions, prog *jsparse.Program) Normalized {
	lim := opts.Limits.withDefaults()
	ni := NormalizerFor(k)
	learned := opts.Learned
	var diags []string
	if err := learned.Validate(); err != nil {
		learned = VolatilitySet{}
		diags = append(diags, "learned_rules_invalid")
	}
	out := Normalized{Kind: k, Grade: GradeComplete}
	rules := ni.Rules
	if !learned.Empty() {
		rules = append(append([]string(nil), ni.Rules...), learned.ruleLines()...)
		out.Version = ni.Name + ".learned" + strconv.Itoa(learned.Version) + "@" + strconv.Itoa(ni.Version)
	} else {
		out.Version = ni.ID()
	}
	out.RuleTableSHA256 = ruleTableSHA(rules)
	if len(body) > lim.MaxResponseBytes {
		body = body[:lim.MaxResponseBytes]
		out.Grade = GradePartial
		diags = append(diags, "input_truncated")
	}
	m := newMasker()
	var r normResult
	switch k {
	case KindHTML:
		r = normalizeHTML(body, opts.Charset, learned, lim, m)
	case KindJS:
		if prog == nil || out.Grade != GradeComplete {
			prog = parseJS(body, false)
		}
		r = normalizeJSProgram(prog, body, m)
	case KindCSS:
		r = normalizeCSS(body, lim, m)
	case KindJSON, KindSourceMap:
		r = normalizeJSON(body, learned, m)
	case KindXML, KindSitemap:
		r = normalizeXML(body, lim)
	case KindHeaders:
		r = normalizeHeaders(body, m)
	case KindBinary:
		r = normalizeBinary(body)
	default:
		r = normalizeText(body, k, m)
	}
	out.Text = r.text
	out.Grade = worseGrade(out.Grade, r.grade)
	out.Diagnostics = append(diags, r.diags...)
	out.Derived = r.derived
	out.Masks, out.MaskDigest = m.result()
	return out
}

// normResult is what a kind-specific normalizer returns.
type normResult struct {
	text    []byte
	grade   Grade
	diags   []string
	derived []Derived
}

// lineWriter accumulates normalized lines.
type lineWriter struct {
	b strings.Builder
}

func (w *lineWriter) line(s string) {
	w.b.WriteString(s)
	w.b.WriteByte('\n')
}

func (w *lineWriter) bytes() []byte { return []byte(w.b.String()) }

// collapseSpace replaces every run of ASCII white space (HTML's "ASCII
// whitespace": TAB, LF, FF, CR, SPACE) by one space and trims both ends.
func collapseSpace(s string) string {
	var b strings.Builder
	space, seen := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\r' {
			space = true
			continue
		}
		if space && seen {
			b.WriteByte(' ')
		}
		space, seen = false, true
		b.WriteByte(c)
	}
	return b.String()
}
