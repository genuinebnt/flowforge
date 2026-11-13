package oam

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/genuinebnt/flowforge/core/netnorm"
)

// TypeEndpoint is the Flowforge Endpoint asset (V2 design §3.1): an HTTP
// method and a URL template an application serves.
const TypeEndpoint = "Endpoint"

// Endpoint sources (V2 §3.1): where an endpoint was found.
const (
	EndpointSourceHTMLForm  = "html_form"
	EndpointSourceJSLiteral = "js_literal"
	EndpointSourceJSCall    = "js_call"
	EndpointSourceOpenAPI   = "openapi"
	EndpointSourceGraphQL   = "graphql"
	EndpointSourceSitemap   = "sitemap"
	EndpointSourceObserved  = "observed"
)

// EndpointSources lists the accepted Endpoint.Source values, sorted.
func EndpointSources() []string {
	return []string{EndpointSourceGraphQL, EndpointSourceHTMLForm, EndpointSourceJSCall, EndpointSourceJSLiteral,
		EndpointSourceObserved, EndpointSourceOpenAPI, EndpointSourceSitemap}
}

// URL template placeholders. Every templated path segment of a
// normalized url_template is exactly one of these.
const (
	PlaceholderID   = "{id}"   // all ASCII digits
	PlaceholderUUID = "{uuid}" // 8-4-4-4-12 hex digits
	PlaceholderHex  = "{hex}"  // at least MinHexSegment hex digits
	PlaceholderExpr = "{expr}" // a JS template substitution or any other variable part
)

// MinHexSegment is the shortest all-hex path segment that is templated
// as {hex}.
const MinHexSegment = 16

// MaxURLTemplateLen bounds the input and the output of
// NormalizeURLTemplate, in bytes.
const MaxURLTemplateLen = 4096

// ErrInvalidURLTemplate is wrapped by every NormalizeURLTemplate error.
var ErrInvalidURLTemplate = errors.New("oam: invalid url template")

// Endpoint is an HTTP endpoint: a method and a URL template whose varying
// path segments are placeholders. Key: ep:<METHOD> <url_template>.
type Endpoint struct {
	Method      string `json:"method,omitempty"`
	URLTemplate string `json:"url_template,omitempty"`
	Source      string `json:"source,omitempty"`
}

// AssetType implements Asset.
func (Endpoint) AssetType() string { return TypeEndpoint }

// Key implements Asset.
func (a Endpoint) Key() (string, error) {
	n, err := a.normalized()
	if err != nil {
		return "", err
	}
	return "ep:" + n.Method + " " + n.URLTemplate, nil
}

// Normalize implements Asset: the method is upper-cased
// (NormalizeHTTPMethod), the template normalized (NormalizeURLTemplate)
// and the source checked against EndpointSources.
func (a Endpoint) Normalize() (Asset, error) { return a.normalized() }

func (a Endpoint) normalized() (Endpoint, error) {
	m, err := NormalizeHTTPMethod(a.Method)
	if err != nil {
		return Endpoint{}, assetErr(TypeEndpoint, "method", err)
	}
	if strings.TrimSpace(a.URLTemplate) == "" {
		return Endpoint{}, assetErr(TypeEndpoint, "url_template", nil)
	}
	t, err := NormalizeURLTemplate(a.URLTemplate)
	if err != nil {
		return Endpoint{}, assetErr(TypeEndpoint, "url_template", err)
	}
	if a.Source != "" && !slices.Contains(EndpointSources(), a.Source) {
		return Endpoint{}, assetErr(TypeEndpoint, "source", fmt.Errorf("unknown source %q", a.Source))
	}
	a.Method, a.URLTemplate = m, t
	return a, nil
}

// MarshalJSON encodes the asset with its type discriminator.
func (a Endpoint) MarshalJSON() ([]byte, error) {
	type plain Endpoint
	return marshalTagged(TypeEndpoint, plain(a))
}

// NormalizeHTTPMethod trims and upper-cases an HTTP method. "*" (any
// method) is accepted; otherwise the method must be 1-32 RFC 9110 token
// characters.
func NormalizeHTTPMethod(m string) (string, error) {
	m = strings.ToUpper(strings.TrimSpace(m))
	if m == "*" {
		return m, nil
	}
	if m == "" {
		return "", errors.New("method is required")
	}
	if len(m) > 32 {
		return "", fmt.Errorf("method %q longer than 32 bytes", m)
	}
	for i := 0; i < len(m); i++ {
		if !isTChar(m[i]) {
			return "", fmt.Errorf("method %q is not an HTTP token", m)
		}
	}
	return m, nil
}

func isTChar(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

// tplPiece is a run of literal text or one placeholder.
type tplPiece struct {
	lit string
	ph  string // canonical placeholder ("{id}", ...) when non-empty
}

// NormalizeURLTemplate returns the canonical form of an endpoint URL
// template (V2 §3.1). The rules, in order:
//
//  1. Leading and trailing white space is trimmed; empty input, input
//     longer than MaxURLTemplateLen bytes and control characters are
//     rejected.
//  2. Placeholders are recognized left to right: a JS template
//     substitution "${...}" (braces nest; an unterminated one is an
//     error) becomes {expr}; "{name}" with name matching
//     [A-Za-z_][A-Za-z0-9_.-]* is a named placeholder, kept as {id},
//     {uuid}, {hex} or {expr} when it is one of those and mapped to
//     {expr} otherwise. Any other brace is literal text (and ends up
//     percent-encoded). Percent-encoded braces are never placeholders.
//  3. The scheme and host must be concrete: the text before the first
//     placeholder must contain "scheme://authority" followed by "/",
//     "?", "#" or the end. Hosts are never templated: a placeholder in or
//     directly after the scheme or authority is an error. Relative and
//     protocol-relative templates are errors (an endpoint without a host
//     belongs to the page's origin, which the caller resolves first).
//  4. The query and fragment (from the first literal "?" or "#") are
//     dropped; query parameters are ParameterProperty tags, not part of
//     the endpoint's identity.
//  5. A path segment that contains a placeholder becomes that
//     placeholder when it is exactly one, and {expr} otherwise.
//  6. The concrete URL (placeholder segments replaced by unique
//     sentinels) is normalized by netnorm.ParseURL: http(s) only,
//     lower-case scheme and host, IDNA A-labels, default port dropped,
//     userinfo rejected, unreserved escapes decoded, remaining escapes
//     upper-cased, other characters percent-encoded and "." / ".."
//     segments removed (a ".." after a placeholder removes it).
//  7. Every concrete segment that is all ASCII digits becomes {id}; one
//     shaped 8-4-4-4-12 hex digits becomes {uuid}; one of at least
//     MinHexSegment hex digits becomes {hex}. The comparison is on the
//     segment after step 6, so "%31%32" is numeric.
//
// Empty segments and a trailing slash are kept (RFC 3986 paths are
// distinct with and without one); the root path is "/". The result is a
// fixed point: NormalizeURLTemplate(NormalizeURLTemplate(s)) ==
// NormalizeURLTemplate(s).
func NormalizeURLTemplate(s string) (string, error) {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrInvalidURLTemplate}, args...)...)
	}
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", bad("empty")
	case len(s) > MaxURLTemplateLen:
		return "", bad("longer than %d bytes", MaxURLTemplateLen)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return "", bad("control character at byte %d", i)
		}
	}
	pieces, err := tokenizeTemplate(s)
	if err != nil {
		return "", bad("%v", err)
	}
	// 3. Concrete scheme and authority.
	pre := ""
	if pieces[0].ph == "" {
		pre = pieces[0].lit
	}
	sep := strings.Index(pre, "://")
	if sep <= 0 || !isScheme(pre[:sep]) {
		return "", bad("no concrete scheme and host")
	}
	authEnd := strings.IndexAny(pre[sep+3:], "/?#")
	if authEnd < 0 {
		if len(pieces) > 1 {
			return "", bad("placeholder in or after the host (hosts are never templated)")
		}
		authEnd = len(pre)
	} else {
		authEnd += sep + 3
	}
	if authEnd == sep+3 {
		return "", bad("empty host")
	}
	origin := pre[:authEnd]
	rest := append([]tplPiece{{lit: pre[authEnd:]}}, pieces[1:]...)
	// 4. Drop the query and fragment; 5. split the path into segments.
	type segment struct {
		lit string
		ph  []string
	}
	segs := []segment{{}}
	cut := false
	for _, p := range rest {
		if cut {
			break
		}
		if p.ph != "" {
			last := &segs[len(segs)-1]
			last.ph = append(last.ph, p.ph)
			continue
		}
		lit := p.lit
		if i := strings.IndexAny(lit, "?#"); i >= 0 {
			lit, cut = lit[:i], true
		}
		parts := strings.Split(lit, "/")
		segs[len(segs)-1].lit += parts[0]
		for _, q := range parts[1:] {
			segs = append(segs, segment{lit: q})
		}
	}
	// segs[0] is what precedes the first "/" (empty: the authority ended
	// at "/", "?", "#" or the end).
	if segs[0].lit != "" || len(segs[0].ph) > 0 {
		return "", bad("placeholder in or after the host (hosts are never templated)")
	}
	segs = segs[1:]
	// 6. Concrete URL with sentinels for placeholder segments.
	sentinel := sentinelBase(s)
	var b strings.Builder
	b.WriteString(origin)
	phOf := map[string]string{}
	for i, sg := range segs {
		b.WriteByte('/')
		if len(sg.ph) == 0 {
			b.WriteString(sg.lit)
			continue
		}
		name := PlaceholderExpr
		if len(sg.ph) == 1 && sg.lit == "" {
			name = sg.ph[0]
		}
		tok := sentinel + strconv.Itoa(i) + "z"
		phOf[tok] = name
		b.WriteString(tok)
	}
	u, err := netnorm.ParseURL(b.String())
	if err != nil {
		return "", bad("%v", err)
	}
	// 7. Classify concrete segments.
	path := u.Path
	u.Path = ""
	out := strings.Builder{}
	out.WriteString(u.String())
	for _, sg := range strings.Split(path[1:], "/") {
		out.WriteByte('/')
		if name, ok := phOf[sg]; ok {
			out.WriteString(name)
			continue
		}
		out.WriteString(classifySegment(sg))
	}
	res := out.String()
	if len(res) > MaxURLTemplateLen {
		return "", bad("normalized template longer than %d bytes", MaxURLTemplateLen)
	}
	return res, nil
}

// tokenizeTemplate splits s into literal runs and placeholders.
func tokenizeTemplate(s string) ([]tplPiece, error) {
	var out []tplPiece
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			out = append(out, tplPiece{lit: lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < len(s); {
		switch {
		case s[i] == '$' && i+1 < len(s) && s[i+1] == '{':
			depth, j := 1, i+2
			for ; j < len(s) && depth > 0; j++ {
				switch s[j] {
				case '{':
					depth++
				case '}':
					depth--
				}
			}
			if depth > 0 {
				return nil, fmt.Errorf("unterminated ${ at byte %d", i)
			}
			flush()
			out = append(out, tplPiece{ph: PlaceholderExpr})
			i = j
		case s[i] == '{':
			j := i + 1
			for j < len(s) && isPlaceholderNameByte(s[j], j == i+1) {
				j++
			}
			if j > i+1 && j < len(s) && s[j] == '}' {
				flush()
				out = append(out, tplPiece{ph: canonicalPlaceholder(s[i+1 : j])})
				i = j + 1
				continue
			}
			lit.WriteByte(s[i])
			i++
		default:
			lit.WriteByte(s[i])
			i++
		}
	}
	flush()
	return out, nil
}

func isPlaceholderNameByte(c byte, first bool) bool {
	if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_' {
		return true
	}
	return !first && (c >= '0' && c <= '9' || c == '.' || c == '-')
}

func canonicalPlaceholder(name string) string {
	switch name {
	case "id":
		return PlaceholderID
	case "uuid":
		return PlaceholderUUID
	case "hex":
		return PlaceholderHex
	}
	return PlaceholderExpr
}

func isScheme(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.')) {
			return false
		}
	}
	return true
}

// sentinelBase returns a token prefix ("ffph", "ffphq", ...) that occurs
// nowhere in s, even after percent-decoding and lower-casing, so a
// sentinel segment in netnorm's output always comes from a placeholder.
func sentinelBase(s string) string {
	dec := strings.ToLower(percentDecodeAll(s))
	base := "ffph"
	for strings.Contains(dec, base) {
		base += "q"
	}
	return base
}

func percentDecodeAll(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// classifySegment templates an id-like concrete segment (rule 7).
func classifySegment(seg string) string {
	switch {
	case seg == "":
		return seg
	case isDigits(seg):
		return PlaceholderID
	case isUUIDShape(seg):
		return PlaceholderUUID
	case len(seg) >= MinHexSegment && isHex(seg):
		return PlaceholderHex
	}
	return seg
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

func isUUIDShape(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if s[i] != '-' {
				return false
			}
			continue
		}
		if !isHex(s[i : i+1]) {
			return false
		}
	}
	return true
}
