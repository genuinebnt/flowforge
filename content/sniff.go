package content

import (
	"bytes"
	"encoding/json"
	"path"
	"strings"
	"unicode/utf8"
)

// Kind is the artifact kind that selects a normalizer and the extractors.
// It is decided by sniffing the bytes, with the media type and the URL
// extension as hints (V2 §12.9).
type Kind string

// Artifact kinds.
const (
	KindHTML        Kind = "html"
	KindJS          Kind = "js"
	KindCSS         Kind = "css"
	KindJSON        Kind = "json"
	KindXML         Kind = "xml"
	KindSitemap     Kind = "sitemap"
	KindSourceMap   Kind = "sourcemap"
	KindRobots      Kind = "robots"
	KindSecurityTxt Kind = "securitytxt"
	// KindTSText is TypeScript, TSX or JSX source: per decision D2 it is
	// normalized and diffed as text and never parsed as JavaScript.
	KindTSText Kind = "ts_text"
	KindText   Kind = "text"
	KindBinary Kind = "binary"
	// KindServerSource is server-side source served as text (PHP, ASP,
	// JSP, a .env file, a server script): itself a source-disclosure
	// finding.
	KindServerSource Kind = "server_source"
	// KindHeaders is the canonical security-header block of one response
	// (V2 §12.8). It is never sniffed; the pipeline creates it.
	KindHeaders Kind = "headers"
)

// Kinds lists every kind in a fixed order.
func Kinds() []Kind {
	return []Kind{KindHTML, KindJS, KindCSS, KindJSON, KindXML, KindSitemap, KindSourceMap, KindRobots,
		KindSecurityTxt, KindTSText, KindText, KindBinary, KindServerSource, KindHeaders}
}

// Source-disclosure languages reported by Sniff.
const (
	DisclosurePHP    = "php"
	DisclosureASP    = "asp"
	DisclosureJSP    = "jsp"
	DisclosureEnv    = "env"
	DisclosureScript = "script" // a server-side interpreter script (#! python, ruby, perl, sh)
)

// SniffResult is the outcome of Sniff.
type SniffResult struct {
	// Kind is the kind decided from the bytes.
	Kind Kind `json:"kind"`
	// Hint is the kind the media type (or, without one, the URL
	// extension) claims; "" when neither says anything.
	Hint Kind `json:"hint,omitempty"`
	// Mismatch is set when Hint is known and incompatible with Kind.
	Mismatch bool `json:"mismatch,omitempty"`
	// Disclosure names the server-side language when the body is (or
	// embeds) unexecuted server-side source.
	Disclosure string `json:"disclosure,omitempty"`
	// Reason names the rule that decided Kind (stable identifiers).
	Reason string `json:"reason"`
}

// sniffWindow is how many bytes the content rules look at, following the
// MIME Sniffing standard's resource header (which uses 1445 bytes) but
// larger, because code heuristics need some statements.
const sniffWindow = 16 << 10

// Sniff decides the kind of a body. The rules are applied in order (the
// first that matches wins) and follow the WHATWG MIME Sniffing standard
// §7.1 "rules for identifying an unknown MIME type" where it has one
// (binary signatures, the HTML tag list, "<?xml"), extended with the
// formats Flowforge reads:
//
//  1. empty body → text;
//  2. binary signatures (§7.1 table: images, PDF, archives, fonts, WASM),
//     a NUL byte in the window, or gzip (a gzip sitemap when the URL or
//     hint says sitemap) → binary;
//  3. server-side source at the start (`<?php`, `<?=`, `<%`, a .env
//     body, a non-node `#!` script) → server_source with Disclosure; a
//     `<?php` later in the window keeps the kind but sets Disclosure;
//  4. markup: `<?xml` (sitemap or xml; XHTML is html), the §7.1 HTML tag
//     list → html, `<urlset`/`<sitemapindex` → sitemap, other tags → xml
//     when the hint is xml, html otherwise;
//  5. valid JSON (an XSSI `)]}'` guard allowed) → sourcemap when it has
//     version and mappings, json otherwise;
//  6. robots.txt and security.txt by path or by directive lines;
//  7. a TypeScript/JSX extension or TS-only syntax → ts_text;
//  8. a JS-versus-CSS indicator vote; ties go to a js/css hint;
//  9. valid UTF-8 → text, otherwise binary.
//
// The media type and URL are hints only; Mismatch records disagreement.
func Sniff(body []byte, contentType, rawURL string) SniffResult {
	p := urlPath(rawURL)
	ext := strings.ToLower(path.Ext(p))
	if strings.HasSuffix(strings.ToLower(p), ".xml.gz") {
		ext = ".xml.gz"
	}
	hint := kindFromMIME(mediaType(contentType))
	if hint == "" {
		hint = kindFromExt(ext, p)
	}
	k, disc, reason := sniffBytes(body, hint, ext, strings.ToLower(p))
	r := SniffResult{Kind: k, Hint: hint, Disclosure: disc, Reason: reason}
	r.Mismatch = hint != "" && !compatibleKinds(hint, k)
	return r
}

func sniffBytes(body []byte, hint Kind, ext, lpath string) (Kind, string, string) {
	win := body[:min(len(body), sniffWindow)]
	if len(bytes.TrimSpace(win)) == 0 {
		return KindText, "", "empty"
	}
	if k, ok := binarySignature(win); ok {
		if k == "gzip" {
			if hint == KindSitemap || ext == ".xml.gz" {
				return KindSitemap, "", "gzip_sitemap"
			}
			return KindBinary, "", "signature_gzip"
		}
		return KindBinary, "", "signature_" + k
	}
	if bytes.HasPrefix(win, []byte{0xFE, 0xFF}) || bytes.HasPrefix(win, []byte{0xFF, 0xFE}) {
		// UTF-16 BOM (§7.1 lists it as text/plain); the markup readers
		// transcode it, so trust a markup hint.
		switch hint {
		case KindHTML, KindXML, KindSitemap:
			return hint, "", "utf16_bom_hint"
		}
		return KindText, "", "utf16_bom"
	}
	if bytes.IndexByte(win, 0) >= 0 {
		return KindBinary, "", "nul_byte"
	}
	s := bytes.TrimPrefix(win, []byte("\xEF\xBB\xBF"))
	s = bytes.TrimLeft(s, " \t\r\n\f")
	if lang, ok := serverSourceAtStart(s, ext); ok {
		return KindServerSource, lang, "server_source_" + lang
	}
	disc := ""
	if hasFold(win, "<?php") {
		disc = DisclosurePHP
	}
	if len(s) > 0 && s[0] == '<' {
		if k, reason := sniffMarkup(s, hint); k != "" {
			return k, disc, reason
		}
	}
	if len(s) > 0 && (s[0] == '{' || s[0] == '[' || s[0] == ')') {
		if k, ok := sniffJSON(body); ok {
			return k, disc, "json_valid"
		}
	}
	if lpath == "/robots.txt" || looksLikeDirectives(s, robotsKeys, "user-agent") {
		return KindRobots, disc, "robots"
	}
	if strings.HasSuffix(lpath, "/security.txt") || lpath == "/security.txt" || looksLikeDirectives(s, securityKeys, "contact") {
		return KindSecurityTxt, disc, "securitytxt"
	}
	switch ext {
	case ".ts", ".tsx", ".mts", ".cts", ".jsx":
		return KindTSText, disc, "ts_extension"
	}
	if hasTSSyntax(s) {
		return KindTSText, disc, "ts_syntax"
	}
	js, css := codeVotes(s)
	switch {
	case js >= 2 && js > css:
		return KindJS, disc, "js_vote"
	case css >= 2 && css > js:
		return KindCSS, disc, "css_vote"
	case (js > 0 || css > 0) && (hint == KindJS || hint == KindCSS):
		return hint, disc, "code_vote_hint"
	}
	if utf8.Valid(win[:validPrefix(win)]) {
		if hint == KindJS || hint == KindCSS || hint == KindTSText {
			return hint, disc, "text_hint"
		}
		return KindText, disc, "utf8_text"
	}
	return KindBinary, disc, "invalid_utf8"
}

// validPrefix trims an incomplete UTF-8 sequence cut by the window.
func validPrefix(b []byte) int {
	n := len(b)
	for i := 0; i < 3 && n-i > 0; i++ {
		if utf8.RuneStart(b[n-1-i]) {
			if !utf8.FullRune(b[n-1-i:]) {
				return n - 1 - i
			}
			break
		}
	}
	return n
}

// binarySignature matches the byte patterns of MIME Sniffing §6 (image,
// audio/video, font, archive types) and §7.1 (PDF, PostScript), plus
// WebAssembly.
func binarySignature(b []byte) (string, bool) {
	sigs := []struct {
		name string
		sig  string
	}{
		{"png", "\x89PNG\r\n\x1a\n"}, {"gif", "GIF87a"}, {"gif", "GIF89a"}, {"jpeg", "\xFF\xD8\xFF"},
		{"pdf", "%PDF-"}, {"postscript", "%!PS-Adobe-"}, {"zip", "PK\x03\x04"}, {"gzip", "\x1F\x8B\x08"},
		{"rar", "Rar!\x1A\x07"}, {"wasm", "\x00asm"}, {"woff", "wOFF"}, {"woff2", "wOF2"},
		{"ico", "\x00\x00\x01\x00"}, {"ogg", "OggS\x00"}, {"id3", "ID3"},
	}
	for _, s := range sigs {
		if bytes.HasPrefix(b, []byte(s.sig)) {
			return s.name, true
		}
	}
	if len(b) >= 12 && string(b[:4]) == "RIFF" && (string(b[8:12]) == "WEBP" || string(b[8:12]) == "WAVE" || string(b[8:12]) == "AVI ") {
		return "riff", true
	}
	return "", false
}

// serverSourceAtStart recognizes unexecuted server-side source at the
// start of the body.
func serverSourceAtStart(s []byte, ext string) (string, bool) {
	switch {
	case hasPrefixFold(s, "<?php") || bytes.HasPrefix(s, []byte("<?=")):
		return DisclosurePHP, true
	case bytes.HasPrefix(s, []byte("<%")):
		if hasPrefixFold(s, "<%@ page") || hasPrefixFold(s, "<%@page") || hasPrefixFold(s, "<%@ taglib") || ext == ".jsp" {
			return DisclosureJSP, true
		}
		return DisclosureASP, true
	case bytes.HasPrefix(s, []byte("#!")):
		line, _, _ := bytes.Cut(s, []byte("\n"))
		if !bytes.Contains(line, []byte("node")) && !bytes.Contains(line, []byte("deno")) && !bytes.Contains(line, []byte("bun")) {
			return DisclosureScript, true
		}
	}
	if isEnvBody(s) {
		return DisclosureEnv, true
	}
	return "", false
}

// isEnvBody reports a dotenv file: at least two KEY=value lines and every
// non-blank, non-comment line of the window of that shape (an optional
// "export " prefix allowed).
func isEnvBody(s []byte) bool {
	n := 0
	for line := range bytes.SplitSeq(s, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		line = bytes.TrimPrefix(line, []byte("export "))
		eq := bytes.IndexByte(line, '=')
		if eq <= 0 {
			return false
		}
		for _, c := range line[:eq] {
			if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
				return false
			}
		}
		if line[0] >= '0' && line[0] <= '9' {
			return false
		}
		n++
	}
	return n >= 2
}

// htmlSniffTags is the MIME Sniffing §7.1 list of HTML tag prefixes
// (each followed by a tag-terminating byte: space or ">"), extended with
// the elements a page commonly starts with.
var htmlSniffTags = []string{"!doctype html", "html", "head", "script", "iframe", "h1", "div", "font", "table",
	"a", "style", "title", "b", "body", "br", "p", "!--", "meta", "link", "noscript", "span", "form", "section",
	"header", "nav", "main", "template", "svg"}

func sniffMarkup(s []byte, hint Kind) (Kind, string) {
	if hasPrefixFold(s, "<?xml") {
		if bytes.Contains(s, []byte("<urlset")) || bytes.Contains(s, []byte("<sitemapindex")) {
			return KindSitemap, "xml_sitemap_root"
		}
		if hasFold(s, "<html") && (hint == KindHTML || hasFold(s, "xhtml")) {
			return KindHTML, "xhtml"
		}
		return KindXML, "xml_declaration"
	}
	if hasPrefixFold(s, "<urlset") || hasPrefixFold(s, "<sitemapindex") {
		return KindSitemap, "sitemap_root"
	}
	if hasPrefixFold(s, "<!--") {
		// A leading comment: decide on what follows it.
		if end := bytes.Index(s, []byte("-->")); end >= 0 {
			rest := bytes.TrimLeft(s[end+3:], " \t\r\n\f")
			if len(rest) > 0 && rest[0] == '<' {
				if k, r := sniffMarkup(rest, hint); k != "" {
					return k, r
				}
			}
		}
		return KindHTML, "html_comment"
	}
	for _, t := range htmlSniffTags {
		if len(s) > len(t)+1 && hasPrefixFold(s[1:], t) {
			c := s[1+len(t)]
			if c == ' ' || c == '>' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '/' || t == "!--" {
				if t == "svg" && hint != KindHTML {
					return KindXML, "svg_root"
				}
				return KindHTML, "html_tag"
			}
		}
	}
	if len(s) > 1 && (s[1] >= 'a' && s[1] <= 'z' || s[1] >= 'A' && s[1] <= 'Z') {
		if hint == KindXML || hint == KindSitemap || bytes.Contains(s[:min(len(s), 512)], []byte("xmlns")) {
			return KindXML, "xml_element"
		}
		return KindHTML, "markup_element"
	}
	return "", ""
}

func sniffJSON(body []byte) (Kind, bool) {
	b := bytes.TrimPrefix(body, []byte("\xEF\xBB\xBF"))
	b = bytes.TrimLeft(b, " \t\r\n")
	if bytes.HasPrefix(b, []byte(")]}'")) {
		// ECMA-426 XSSI guard: only source maps use it in practice.
		_, rest, _ := bytes.Cut(b, []byte("\n"))
		if json.Valid(rest) && looksLikeSourceMap(rest) {
			return KindSourceMap, true
		}
		return "", false
	}
	if !json.Valid(b) {
		return "", false
	}
	if looksLikeSourceMap(b) {
		return KindSourceMap, true
	}
	return KindJSON, true
}

func looksLikeSourceMap(b []byte) bool {
	var probe struct {
		Version  json.RawMessage `json:"version"`
		Mappings json.RawMessage `json:"mappings"`
		Sections json.RawMessage `json:"sections"`
	}
	if b[0] != '{' || json.Unmarshal(b, &probe) != nil {
		return false
	}
	return string(probe.Version) == "3" && (probe.Mappings != nil || probe.Sections != nil)
}

var robotsKeys = []string{"user-agent", "disallow", "allow", "sitemap", "crawl-delay", "host"}
var securityKeys = []string{"contact", "expires", "encryption", "acknowledgments", "policy", "preferred-languages",
	"canonical", "hiring", "csaf", "-----begin pgp signed message-----", "hash"}

// looksLikeDirectives reports a "Key: value" file whose lines are all
// directives from keys (comments and blank lines aside), with at least
// one line using the required key.
func looksLikeDirectives(s []byte, keys []string, required string) bool {
	seen, lines := false, 0
	for line := range bytes.SplitSeq(s, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		k, _, ok := bytes.Cut(line, []byte(":"))
		lk := strings.ToLower(strings.TrimSpace(string(k)))
		if !ok && !strings.HasPrefix(lk, "-----") {
			return false
		}
		known := false
		for _, key := range keys {
			if lk == key || strings.HasPrefix(lk, "-----") {
				known = true
				break
			}
		}
		if !known {
			return false
		}
		if lk == required {
			seen = true
		}
		lines++
	}
	return seen && lines > 0
}

// hasTSSyntax recognizes TypeScript-only declarations at the start of a
// line (type-only imports and exports, interfaces, enums, declare).
func hasTSSyntax(s []byte) bool {
	for line := range bytes.SplitSeq(s, []byte("\n")) {
		line = bytes.TrimSpace(line)
		for _, p := range []string{"import type ", "export type ", "export interface ", "interface ", "declare module ", "export enum ", "declare const "} {
			if bytes.HasPrefix(line, []byte(p)) && bytes.ContainsAny(line, "{=") {
				return true
			}
		}
	}
	return false
}

var jsIndicators = []string{"function", "=>", "var ", "let ", "const ", "return ", "this.", ".prototype", "window.",
	"document.", "require(", "import(", "export ", "\"use strict\"", "'use strict'", "webpackChunk",
	"__webpack_require__", "!function", "typeof ", "new "}
var cssIndicators = []string{"@media", "@import", "@font-face", "@keyframes", "!important", "px;", "px}", "color:",
	"margin:", "padding:", "display:", "font-family:", "background:", "@charset", "{color", "border:"}

// codeVotes counts distinct JS and CSS indicators in the window.
func codeVotes(s []byte) (js, css int) {
	for _, t := range jsIndicators {
		if bytes.Contains(s, []byte(t)) {
			js++
		}
	}
	for _, t := range cssIndicators {
		if bytes.Contains(s, []byte(t)) {
			css++
		}
	}
	return js, css
}

// kindFromMIME maps a media type to the kind it claims.
func kindFromMIME(mt string) Kind {
	switch {
	case mt == "":
		return ""
	case mt == "text/html" || mt == "application/xhtml+xml":
		return KindHTML
	case mt == "text/javascript" || mt == "application/javascript" || mt == "application/x-javascript" ||
		mt == "text/ecmascript" || mt == "application/ecmascript" || mt == "module":
		return KindJS
	case mt == "text/css":
		return KindCSS
	case mt == "application/json" || strings.HasSuffix(mt, "+json") || mt == "text/json":
		if mt == "application/sourcemap+json" {
			return KindSourceMap
		}
		return KindJSON
	case mt == "application/xml" || mt == "text/xml" || strings.HasSuffix(mt, "+xml"):
		return KindXML
	case mt == "application/typescript" || mt == "text/typescript":
		return KindTSText
	case strings.HasPrefix(mt, "text/"):
		return KindText
	case strings.HasPrefix(mt, "image/") || strings.HasPrefix(mt, "audio/") || strings.HasPrefix(mt, "video/") ||
		strings.HasPrefix(mt, "font/") || mt == "application/octet-stream" || mt == "application/pdf" ||
		mt == "application/zip" || mt == "application/gzip" || mt == "application/wasm":
		return KindBinary
	case mt == "application/x-httpd-php" || mt == "application/x-php":
		return KindServerSource
	}
	return ""
}

// kindFromExt maps a URL extension (and well-known paths) to a kind.
func kindFromExt(ext, p string) Kind {
	lp := strings.ToLower(p)
	switch {
	case lp == "/robots.txt":
		return KindRobots
	case strings.HasSuffix(lp, "/security.txt"):
		return KindSecurityTxt
	}
	switch ext {
	case ".html", ".htm", ".xhtml":
		return KindHTML
	case ".js", ".mjs", ".cjs":
		return KindJS
	case ".css":
		return KindCSS
	case ".json":
		return KindJSON
	case ".map":
		return KindSourceMap
	case ".xml", ".rss", ".atom":
		if strings.Contains(lp, "sitemap") {
			return KindSitemap
		}
		return KindXML
	case ".xml.gz":
		return KindSitemap
	case ".ts", ".tsx", ".mts", ".cts", ".jsx":
		return KindTSText
	case ".txt", ".md":
		return KindText
	case ".php", ".asp", ".aspx", ".jsp", ".env":
		return KindServerSource
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".pdf", ".zip", ".gz", ".woff", ".woff2", ".wasm", ".mp4":
		return KindBinary
	}
	return ""
}

// compatibleKinds reports whether a hinted kind agrees with the sniffed
// one: equal, or a refinement (xml→sitemap, json→sourcemap, text→robots,
// js→ts_text ...).
func compatibleKinds(hint, k Kind) bool {
	if hint == k {
		return true
	}
	switch hint {
	case KindXML:
		return k == KindSitemap || k == KindHTML
	case KindJSON:
		return k == KindSourceMap
	case KindSourceMap:
		return k == KindJSON
	case KindText:
		return k == KindRobots || k == KindSecurityTxt || k == KindTSText
	case KindJS:
		return k == KindTSText
	case KindTSText:
		return k == KindJS || k == KindText
	case KindRobots, KindSecurityTxt:
		return k == KindText
	case KindSitemap:
		return k == KindXML
	}
	return false
}

func hasPrefixFold(s []byte, p string) bool {
	return len(s) >= len(p) && strings.EqualFold(string(s[:len(p)]), p)
}

// hasFold reports whether s contains the ASCII string p, ignoring case.
func hasFold(s []byte, p string) bool {
	if len(p) == 0 {
		return true
	}
	lp := strings.ToLower(p)
	first, upper := lp[0], lp[0]
	if first >= 'a' && first <= 'z' {
		upper = first - 32
	}
	for i := 0; i+len(p) <= len(s); i++ {
		c := s[i]
		if c != first && c != upper {
			continue
		}
		if strings.EqualFold(string(s[i:i+len(p)]), p) {
			return true
		}
	}
	return false
}
