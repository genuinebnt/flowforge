package content

import (
	"net/url"
	"strings"

	"github.com/genuinebnt/flowforge/oam"
)

// absTemplate turns a URL or URL template as written in a page or script
// into an absolute template string, resolved against base. It works on the
// text (not net/url), because templates contain "${...}" and "{name}"
// placeholders that a URL parser would escape. Rules:
//
//   - "http://", "https://": already absolute;
//   - "//host/...": the base scheme is prefixed;
//   - "/path": the base origin is prefixed;
//   - "${...}/path" (a dynamic API base): treated as origin-relative,
//     reported through dynamicBase;
//   - anything else: relative to the base directory;
//   - ":name" path segments (Express/React-Router style) become "{name}".
func absTemplate(raw, base string) (abs string, dynamicBase, ok bool) {
	raw = strings.TrimSpace(raw)
	if rest, cut := strings.CutPrefix(raw, "${}"); cut && strings.HasPrefix(rest, "/") {
		raw, dynamicBase = rest, true
	}
	if strings.HasPrefix(raw, "${") && !strings.HasPrefix(raw, "${}") {
		if end := strings.IndexByte(raw, '}'); end > 0 && strings.HasPrefix(raw[end+1:], "/") {
			raw, dynamicBase = raw[end+1:], true
		}
	}
	if raw == "" || strings.ContainsAny(raw, " \t\n\r<>\"'\\") {
		return "", false, false
	}
	b, err := url.Parse(base)
	if err != nil || b.Scheme == "" || b.Host == "" {
		return "", false, false
	}
	lr := strings.ToLower(raw)
	switch {
	case strings.HasPrefix(lr, "http://") || strings.HasPrefix(lr, "https://"):
		abs = raw
	case strings.HasPrefix(raw, "//"):
		abs = b.Scheme + ":" + raw
	case strings.HasPrefix(raw, "/"):
		abs = b.Scheme + "://" + b.Host + raw
	case strings.Contains(raw, ":") && !strings.HasPrefix(raw, ":") && strings.Index(raw, ":") < strings.IndexAny(raw+"/", "/?#"):
		return "", false, false // another scheme (mailto:, data:, javascript:)
	default:
		dir := b.EscapedPath()
		if i := strings.LastIndexByte(dir, '/'); i >= 0 {
			dir = dir[:i+1]
		} else {
			dir = "/"
		}
		abs = b.Scheme + "://" + b.Host + dir + raw
	}
	return expressParams(abs), dynamicBase, true
}

// expressParams rewrites ":name" path segments to "{name}".
func expressParams(abs string) string {
	sep := strings.Index(abs, "://")
	if sep < 0 {
		return abs
	}
	slash := strings.IndexByte(abs[sep+3:], '/')
	if slash < 0 {
		return abs
	}
	head, p := abs[:sep+3+slash], abs[sep+3+slash:]
	q := ""
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p, q = p[:i], p[i:]
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if len(s) > 1 && s[0] == ':' && isIdentLike(s[1:]) {
			segs[i] = "{" + strings.TrimSuffix(s[1:], "?") + "}"
		}
	}
	return head + strings.Join(segs, "/") + q
}

func isIdentLike(s string) bool {
	s = strings.TrimSuffix(s, "?")
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(alnum(c) || c == '_') {
			return false
		}
	}
	return true
}

// endpointOf builds a normalized Endpoint and its key.
func endpointOf(raw, base, method, source string) (oam.Endpoint, string, bool) {
	abs, _, ok := absTemplate(raw, base)
	if !ok {
		return oam.Endpoint{}, "", false
	}
	tpl, err := oam.NormalizeURLTemplate(abs)
	if err != nil {
		return oam.Endpoint{}, "", false
	}
	m, err := oam.NormalizeHTTPMethod(method)
	if err != nil {
		return oam.Endpoint{}, "", false
	}
	ep := oam.Endpoint{Method: m, URLTemplate: tpl, Source: source}
	key, err := ep.Key()
	if err != nil {
		return oam.Endpoint{}, "", false
	}
	return ep, key, true
}

// queryParamNames returns the parameter names of a URL's query, in order,
// without duplicates or placeholders.
func queryParamNames(raw string) []string {
	_, q, ok := strings.Cut(raw, "?")
	if !ok {
		return nil
	}
	q, _, _ = strings.Cut(q, "#")
	var out []string
	seen := map[string]bool{}
	for p := range strings.SplitSeq(q, "&") {
		k, _, _ := strings.Cut(p, "=")
		if k == "" || strings.ContainsAny(k, "${}") || seen[k] || len(k) > 64 {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// staticAssetExt reports a path that names a static file, not an
// endpoint.
func staticAssetExt(p string) bool {
	p, _, _ = strings.Cut(p, "?")
	p, _, _ = strings.Cut(p, "#")
	i := strings.LastIndexByte(p, '.')
	if i < 0 || strings.LastIndexByte(p, '/') > i {
		return false
	}
	switch strings.ToLower(p[i+1:]) {
	case "js", "mjs", "cjs", "css", "map", "png", "jpg", "jpeg", "gif", "svg", "webp", "avif", "ico", "woff",
		"woff2", "ttf", "eot", "otf", "mp4", "webm", "mp3", "wav", "pdf", "txt", "html", "htm", "md", "wasm", "ts", "tsx", "jsx", "vue", "scss", "less":
		return true
	}
	return false
}
