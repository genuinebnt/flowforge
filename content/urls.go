package content

import (
	"net/url"
	"strings"
)

// urlPath returns the path of a URL ("" when it does not parse).
func urlPath(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return u.Path
}

// resolveRef resolves ref against base (RFC 3986 §5.2) and returns an
// absolute http(s) URL without fragment. It fails for other schemes
// (javascript:, data:, mailto:) and unparsable input.
func resolveRef(base, ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", false
	}
	b, err := url.Parse(base)
	if err != nil {
		return "", false
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", false
	}
	u := b.ResolveReference(r)
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return "", false
	}
	u.Fragment, u.RawFragment = "", ""
	return u.String(), true
}

// sameOrigin reports whether two absolute URLs share scheme and host
// (port included).
func sameOrigin(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	return err1 == nil && err2 == nil && strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host)
}

// hashPlaceholder replaces a build-hash part of a file name, both in the
// lineage key and in normalized text.
const hashPlaceholder = "__FF_HASH__"

// isHashLike reports a file-name part that a bundler generated: at least
// 8 characters of [0-9a-z] (case-insensitive) that is all hex, or mixes
// letters and digits (webpack [contenthash], vite base64url-ish hashes,
// esbuild [hash]).
func isHashLike(s string) bool {
	if len(s) < 8 || len(s) > 64 || s == hashPlaceholder {
		return false
	}
	hex, letters, digits := true, false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			digits = true
		case c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F':
			letters = true
		case c >= 'g' && c <= 'z' || c >= 'G' && c <= 'Z' || c == '_' || c == '-':
			letters, hex = true, false
		default:
			return false
		}
	}
	if hex {
		return digits
	}
	return letters && digits
}

// stripHashSegments rewrites the last path segment of p, replacing the
// hash-like parts of the file name with hashPlaceholder. The name is split
// on '.', '-' and '_'; the extension (after the last '.') is never
// replaced; the first part is replaced only when it is all digits (a
// webpack chunk id) or all hex with a digit (a [contenthash] name), so
// "analytics2024.js" keeps its name; later parts are replaced when
// isHashLike.
func stripHashSegments(p string) string {
	slash := strings.LastIndexByte(p, '/')
	dir, name := p[:slash+1], p[slash+1:]
	extAt := strings.LastIndexByte(name, '.')
	if name == "" || extAt < 0 || !assetExts[strings.ToLower(name[extAt+1:])] {
		// Only static assets carry build hashes; an id in /users/12345678
		// is data, not a build artifact.
		return p
	}
	var b strings.Builder
	b.Grow(len(name))
	start, part := 0, 0
	for i := 0; i <= len(name); i++ {
		if i < len(name) && name[i] != '.' && name[i] != '-' && name[i] != '_' {
			continue
		}
		seg := name[start:i]
		isExt := extAt >= 0 && start == extAt+1
		switch {
		case isExt || seg == "":
			b.WriteString(seg)
		case part == 0 && (isAllDigits(seg) || isHashLike(seg) && isHexString(seg)):
			b.WriteString(hashPlaceholder)
		case part > 0 && isHashLike(seg):
			b.WriteString(hashPlaceholder)
		default:
			b.WriteString(seg)
		}
		if i < len(name) {
			b.WriteByte(name[i])
		}
		start = i + 1
		part++
	}
	return dir + b.String()
}

// assetExts are the extensions of bundler-emitted static assets.
var assetExts = map[string]bool{"js": true, "mjs": true, "cjs": true, "css": true, "map": true, "png": true, "jpg": true,
	"jpeg": true, "gif": true, "svg": true, "webp": true, "avif": true, "woff": true, "woff2": true, "ttf": true,
	"eot": true, "wasm": true, "json": true, "ico": true}

func isHexString(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return s != ""
}

// LineageKey is the file-level lineage key of a script URL (V2 §5): the
// URL without query and fragment, scheme and host lower-cased, and every
// hash-like part of the file name replaced, so main.3f9a1c2b.js and
// main.77e0d41a.js share a key.
func LineageKey(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return raw
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + stripHashSegments(u.EscapedPath())
}

// cacheBustParams are query parameters whose values bust caches.
var cacheBustParams = map[string]bool{"v": true, "ver": true, "version": true, "t": true, "ts": true, "_": true,
	"cb": true, "cachebust": true, "cache": true, "rev": true, "hash": true, "build": true, "bust": true,
	"nocache": true, "timestamp": true, "_t": true, "_ts": true, "h": true, "_v": true}

// maskURL masks the volatile parts of a URL in normalized text: values
// of cache-busting query parameters (and any timestamp-valued
// parameter), and build hashes in the file name. It works on the text as
// written, so relative and absolute URLs are handled alike.
func maskURL(s string, m *masker) string {
	pathPart, query, hasQ := strings.Cut(s, "?")
	frag := ""
	if hasQ {
		if i := strings.IndexByte(query, '#'); i >= 0 {
			query, frag = query[:i], query[i:]
		}
	} else if i := strings.IndexByte(pathPart, '#'); i >= 0 {
		pathPart, frag = pathPart[:i], pathPart[i:]
	}
	if np := stripHashSegments(pathPart); np != pathPart {
		m.hit(MaskBuildHash, pathPart)
		pathPart = np
	}
	if !hasQ {
		return pathPart + frag
	}
	parts := strings.Split(query, "&")
	for i, p := range parts {
		k, v, ok := strings.Cut(p, "=")
		if !ok || v == "" || v == maskPlaceholder(MaskCacheBust) {
			continue
		}
		if cacheBustParams[strings.ToLower(k)] && isShortToken(v) || isTimestampDigits(v) {
			m.hit(MaskCacheBust, v)
			parts[i] = k + "=" + maskPlaceholder(MaskCacheBust)
		}
	}
	return pathPart + "?" + strings.Join(parts, "&") + frag
}

// isShortToken: a cache-bust value is a short run of [A-Za-z0-9._-].
func isShortToken(v string) bool {
	if len(v) > 64 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
