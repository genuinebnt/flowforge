package content

import (
	"strings"

	"github.com/genuinebnt/flowforge/jsparse"
)

// semverPrefix returns the leading "MAJOR.MINOR[.PATCH][-pre][+build]"
// of s, or "" when s does not start with one (at least MAJOR.MINOR, all
// numeric parts at most 6 digits).
func semverPrefix(s string) string {
	i, parts := 0, 0
	for parts < 3 {
		j := i
		for j < len(s) && j-i < 6 && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j == i {
			break
		}
		parts++
		i = j
		if parts < 3 && i+1 < len(s) && s[i] == '.' && s[i+1] >= '0' && s[i+1] <= '9' {
			i++
			continue
		}
		break
	}
	if parts < 2 {
		return ""
	}
	if i < len(s) && (s[i] == '-' || s[i] == '+') {
		j := i + 1
		for j < len(s) && (alnum(s[j]) || s[j] == '.' || s[j] == '-') {
			j++
		}
		if j > i+1 {
			i = j
		}
	}
	if i < len(s) && (alnum(s[i]) || s[i] == '_') {
		return "" // "1.2abc" is not a version
	}
	return strings.TrimRight(s[:i], ".-")
}

// libraryName canonicalizes a product name: lower case, ".js"/".min"
// suffixes and punctuation removed, words joined by '-'.
func libraryName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, suf := range []string{".min.js", ".js", ".min"} {
		s = strings.TrimSuffix(s, suf)
	}
	var b strings.Builder
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.') }) {
		w = strings.Trim(w, ".")
		if w == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('-')
		}
		b.WriteString(w)
	}
	return b.String()
}

// bannerSkip are words between a library name and its version that are
// not part of the name.
var bannerSkip = map[string]bool{"v": true, "version": true, "-": true, "|": true, "@license": true,
	"@preserve": true, "license": true, "@version": true, "build": true, "release": true}

// Library is one identified third-party library.
type Library struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Evidence string `json:"evidence"` // "banner" or "global"
}

// libraryBanners reads library banners from comments: block comments
// that start with "/*!" or carry @license/@preserve, and line comments
// among the first four comments of the program. parseBanner reads each.
func libraryBanners(prog *jsparse.Program) []Library {
	var out []Library
	for i, c := range prog.Comments {
		if int(c.Span.End) > len(prog.Source) || c.Span.Start < 0 {
			continue
		}
		text := prog.Source[c.Span.Start:c.Span.End]
		block := c.Kind == jsparse.TBlockComment
		if block && !strings.HasPrefix(text, "/*!") && !strings.Contains(text, "@license") && !strings.Contains(text, "@preserve") {
			continue
		}
		if !block && (c.Kind != jsparse.TLineComment || i > 3) {
			continue
		}
		if lib, ok := parseBanner(text); ok {
			out = append(out, lib)
		}
	}
	return out
}

// parseBanner reads one banner comment. Only its first three lines count.
// The first word that is a version ("v3.7.1", "3.7.1") gives the version
// and the closest of the three words before it that is name-like (not a
// skip word, "(c)" or a URL) gives the name ("/*! jQuery v3.7.1 | (c)
// OpenJS Foundation" → jquery 3.7.1); a word "name@1.2.3" gives both. A
// first version without a name ends the search: later versions in the
// same comment are not tried.
func parseBanner(text string) (Library, bool) {
	text = strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(text, "/*!"), "/**"), "//")
	text = strings.TrimSuffix(text, "*/")
	// Only the first few lines name the library.
	lines := strings.SplitN(text, "\n", 4)
	words := strings.Fields(strings.ReplaceAll(strings.Join(lines[:min(len(lines), 3)], " "), "*", " "))
	for i, w := range words {
		lw := strings.ToLower(w)
		v := semverPrefix(strings.TrimPrefix(lw, "v"))
		if v == "" {
			if at := strings.LastIndexByte(lw, '@'); at > 0 {
				// "name@1.2.3"
				if v = semverPrefix(lw[at+1:]); v != "" {
					if n := libraryName(lw[:at]); n != "" {
						return Library{Name: n, Version: v, Evidence: "banner"}, true
					}
				}
			}
			continue
		}
		for j := i - 1; j >= 0 && j >= i-3; j-- {
			cand := strings.ToLower(words[j])
			if bannerSkip[cand] || strings.HasPrefix(cand, "(") || strings.HasPrefix(cand, "http") {
				continue
			}
			if n := libraryName(cand); len(n) >= 2 && n[0] >= 'a' && n[0] <= 'z' {
				return Library{Name: n, Version: v, Evidence: "banner"}, true
			}
		}
		return Library{}, false
	}
	return Library{}, false
}

// globalVersionKeys maps object keys whose string value is a library's
// version, wherever the bundle puts the object, to the library name.
// jQuery's prototype keeps `jquery: "3.7.1"`. Keys such as Vue's and
// Lodash's `VERSION` are not listed: the key alone does not name the
// library.
var globalVersionKeys = map[string]string{"jquery": "jquery"}

// libraryGlobals reads versions from known property keys.
func libraryGlobals(uses []jsparse.StringUse) []Library {
	var out []Library
	for _, u := range uses {
		if name, ok := globalVersionKeys[u.PropertyKey]; ok {
			if v := semverPrefix(u.Value); v != "" && v == u.Value {
				out = append(out, Library{Name: name, Version: v, Evidence: "global"})
			}
		}
	}
	return out
}
