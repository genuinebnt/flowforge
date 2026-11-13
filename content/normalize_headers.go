package content

import (
	"cmp"
	"slices"
	"strings"
)

// headerRules is the headers@1 rule table (V2 §12.8).
var headerRules = []string{
	"input:one \"name: value\" line per field (HeaderBlock); lines without ':' ignored",
	"keep:security-relevant headers only (securityHeaders); they are level-3 paths, so no learned rule masks them",
	"name:lower case; value: white-space runs → one space, trimmed",
	"csp:directives lower-cased and sorted; sources sorted and deduped; 'nonce-…' sources removed → nonce",
	"set-cookie:value dropped (name kept); attribute names lower-cased, sorted; expires dropped → timestamp",
	"hsts:directives lower-cased and sorted",
	"location:query values masked → random_id",
	"order:lines sorted by (name, value)",
}

// securityHeaders are the response headers of the canonical block: the
// security-relevant ones of V2 §3.3 (CSP, HSTS, CORS, framing, MIME
// sniffing, referrer and permissions policy, cross-origin isolation,
// cookies, server banners, redirects and authentication challenges).
// Everything else (Date, Age, ETag, request ids, caching) is volatile or
// not security-relevant and is dropped.
var securityHeaders = map[string]bool{
	"content-security-policy": true, "content-security-policy-report-only": true,
	"strict-transport-security": true, "x-frame-options": true, "x-content-type-options": true,
	"referrer-policy": true, "permissions-policy": true, "feature-policy": true,
	"cross-origin-opener-policy": true, "cross-origin-embedder-policy": true, "cross-origin-resource-policy": true,
	"access-control-allow-origin": true, "access-control-allow-credentials": true,
	"access-control-allow-methods": true, "access-control-allow-headers": true,
	"access-control-expose-headers": true, "set-cookie": true, "server": true, "x-powered-by": true,
	"x-aspnet-version": true, "x-aspnetmvc-version": true, "x-generator": true, "x-xss-protection": true,
	"x-permitted-cross-domain-policies": true, "content-type": true, "location": true, "www-authenticate": true,
	"clear-site-data": true, "origin-agent-cluster": true, "x-dns-prefetch-control": true, "timing-allow-origin": true,
}

// SecurityHeaders returns the names kept in the canonical header block,
// sorted.
func SecurityHeaders() []string {
	out := make([]string, 0, len(securityHeaders))
	for k := range securityHeaders {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// HeaderBlock renders raw headers as the input of the headers normalizer:
// one "Name: value" line per field, in wire order.
func HeaderBlock(hs []Header) []byte {
	var b strings.Builder
	for _, h := range hs {
		b.WriteString(strings.ReplaceAll(h.Name, "\n", " ") + ": " + strings.NewReplacer("\r", " ", "\n", " ").Replace(h.Value) + "\n")
	}
	return []byte(b.String())
}

// parseHeaderBlock is the inverse of HeaderBlock (names lower-cased).
func parseHeaderBlock(body []byte) []Header {
	var out []Header
	for line := range strings.SplitSeq(string(body), "\n") {
		name, value, ok := strings.Cut(line, ":")
		name = strings.ToLower(strings.TrimSpace(name))
		if !ok || name == "" || strings.ContainsAny(name, " \t") {
			continue
		}
		out = append(out, Header{Name: name, Value: collapseSpace(value)})
	}
	return out
}

func normalizeHeaders(body []byte, m *masker) normResult {
	var hs []Header
	for _, h := range parseHeaderBlock(body) {
		if !securityHeaders[h.Name] {
			continue
		}
		v := canonicalHeaderValue(h.Name, h.Value, m)
		if v == "" && h.Name != "set-cookie" {
			continue
		}
		hs = append(hs, Header{Name: h.Name, Value: v})
	}
	slices.SortFunc(hs, func(a, b Header) int { return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Value, b.Value)) })
	var w lineWriter
	for _, h := range hs {
		w.line(h.Name + ": " + h.Value)
	}
	return normResult{text: w.bytes(), grade: GradeComplete}
}

func canonicalHeaderValue(name, v string, m *masker) string {
	switch name {
	case "content-security-policy", "content-security-policy-report-only":
		return canonicalCSP(v, m)
	case "set-cookie":
		return cookieSummary(v, m)
	case "strict-transport-security":
		ds := splitTrim(strings.ToLower(v), ";")
		slices.Sort(ds)
		return strings.Join(ds, "; ")
	case "content-type":
		return strings.ToLower(v)
	case "location":
		p, q, ok := strings.Cut(v, "?")
		if !ok {
			return v
		}
		parts := strings.Split(q, "&")
		for i, kv := range parts {
			k, val, has := strings.Cut(kv, "=")
			if has && val != "" && val != maskPlaceholder(MaskRandomID) {
				m.hit(MaskRandomID, val)
				parts[i] = k + "=" + maskPlaceholder(MaskRandomID)
			}
		}
		return p + "?" + strings.Join(parts, "&")
	}
	return v
}

func splitTrim(s, sep string) []string {
	var out []string
	for p := range strings.SplitSeq(s, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// CSPDirective is one parsed Content-Security-Policy directive.
type CSPDirective struct {
	Name    string
	Sources []string
}

// ParseCSP parses a policy (CSP3 §2.2.1 "parse a serialized CSP"):
// directives split on ';', the first token is the name (lower-cased),
// the rest are the sources. A repeated directive is ignored, as the
// specification requires.
func ParseCSP(v string) []CSPDirective {
	var out []CSPDirective
	seen := map[string]bool{}
	for _, d := range splitTrim(v, ";") {
		fs := strings.Fields(d)
		name := strings.ToLower(fs[0])
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, CSPDirective{Name: name, Sources: fs[1:]})
	}
	return out
}

func canonicalCSP(v string, m *masker) string {
	ds := ParseCSP(v)
	slices.SortFunc(ds, func(a, b CSPDirective) int { return cmp.Compare(a.Name, b.Name) })
	parts := make([]string, 0, len(ds))
	for _, d := range ds {
		var src []string
		for _, s := range d.Sources {
			if strings.HasPrefix(strings.ToLower(s), "'nonce-") {
				m.hit(MaskNonce, s)
				continue
			}
			src = append(src, s)
		}
		slices.Sort(src)
		src = slices.Compact(src)
		parts = append(parts, strings.Join(append([]string{d.Name}, src...), " "))
	}
	return strings.Join(parts, "; ")
}

// CookieSummary is a Set-Cookie value reduced to the cookie name and its
// attributes (V2 §3.3: cookie values never enter the graph).
type CookieSummary struct {
	Name     string
	Secure   bool
	HTTPOnly bool
	SameSite string // lower case, "" when absent
	Attrs    []string
}

// ParseSetCookie reduces a Set-Cookie value (RFC 6265 §5.2): the name
// before '=', attributes after ';'. Expires is dropped (it is a date that
// changes on every response).
func ParseSetCookie(v string) CookieSummary {
	items := strings.Split(v, ";")
	first := strings.TrimSpace(items[0])
	name, _, _ := strings.Cut(first, "=")
	c := CookieSummary{Name: strings.TrimSpace(name)}
	for _, it := range items[1:] {
		k, val, has := strings.Cut(strings.TrimSpace(it), "=")
		k = strings.ToLower(strings.TrimSpace(k))
		val = strings.TrimSpace(val)
		switch {
		case k == "":
			continue
		case k == "expires":
			continue
		case k == "secure":
			c.Secure = true
		case k == "httponly":
			c.HTTPOnly = true
		case k == "samesite":
			c.SameSite = strings.ToLower(val)
		}
		if has {
			if k == "domain" || k == "samesite" {
				val = strings.ToLower(val)
			}
			c.Attrs = append(c.Attrs, k+"="+val)
		} else {
			c.Attrs = append(c.Attrs, k)
		}
	}
	slices.Sort(c.Attrs)
	c.Attrs = slices.Compact(c.Attrs)
	return c
}

// String renders "name; attr; attr=value".
func (c CookieSummary) String() string {
	return strings.Join(append([]string{c.Name}, c.Attrs...), "; ")
}

func cookieSummary(v string, m *masker) string {
	for _, it := range strings.Split(v, ";")[1:] {
		k, val, _ := strings.Cut(strings.TrimSpace(it), "=")
		if strings.EqualFold(strings.TrimSpace(k), "expires") {
			m.hit(MaskTimestamp, strings.TrimSpace(val))
		}
	}
	return ParseSetCookie(v).String()
}
