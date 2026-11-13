package content

import (
	"fmt"
	"strings"

	"github.com/genuinebnt/flowforge/core"
)

// Generators for the invariant tests. All draw from a core.SplitMix64, so
// FF_SEED reproduces a failure.

func pick[T any](r core.Rng, xs []T) T { return xs[core.IntN(r, len(xs))] }

func randToken(r core.Rng, n int, alphabet string) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[core.IntN(r, len(alphabet))]
	}
	return string(b)
}

const alnumChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

var genTags = []string{"div", "p", "span", "a", "table", "tr", "td", "ul", "li", "form", "input", "section", "b", "i",
	"em", "select", "option", "textarea", "pre", "button", "label", "img", "br", "h1", "nav", "svg", "path", "template",
	"iframe", "script", "style", "title", "noscript", "dl", "dt", "dd", "xmp", "math", "mi", "code", "font"}

var genText = []string{"hello", "world", "a &amp; b", "x < y", "  spaced   out  ", "\n\t", "2026-11-13T10:00:00Z",
	"&lt;tag&gt;", "café", "\u00a0nbsp", "1762596000", "price: 10 > 9", "&quot;q&quot;", "line\r\nbreak"}

// genHTML builds a random, often malformed, HTML document.
func genHTML(r core.Rng) string {
	var b strings.Builder
	if core.IntN(r, 3) > 0 {
		b.WriteString(pick(r, []string{"<!DOCTYPE html>", "<!doctype html>", "<!DOCTYPE html PUBLIC \"-//W3C//DTD HTML 4.01//EN\">", "<!DOCTYPE foo>", ""}))
	}
	var open []string
	for range 10 + core.IntN(r, 40) {
		switch core.IntN(r, 9) {
		case 0, 1, 2:
			t := pick(r, genTags)
			b.WriteString("<" + t)
			for range core.IntN(r, 4) {
				name := pick(r, []string{"class", "id", "href", "src", "nonce", "data-ts", "onclick", "type", "name", "value", "style", "action", "method", "integrity", "srcset"})
				val := pick(r, []string{"x", "b a b", "/p?v=123", "javascript:go()", "1762596000", "abc\"def", "a&b", "", "hidden", "csrf_token", "https://e.example/x.3f9a1c2b.js"})
				fmt.Fprintf(&b, " %s=\"%s\"", name, strings.ReplaceAll(val, `"`, "&quot;"))
			}
			b.WriteString(">")
			if t == "script" || t == "style" || t == "textarea" || t == "title" || t == "xmp" {
				b.WriteString(pick(r, []string{"var a = 1;", "a{color:red}", "x < y && z", "</p>", ""}))
				b.WriteString("</" + t + ">")
			} else {
				open = append(open, t)
			}
		case 3, 4:
			b.WriteString(pick(r, genText))
		case 5:
			if len(open) > 0 {
				b.WriteString("</" + open[len(open)-1] + ">")
				open = open[:len(open)-1]
			}
		case 6:
			b.WriteString("<!-- " + pick(r, genText) + " -->")
		case 7:
			b.WriteString("</" + pick(r, genTags) + ">")
		case 8:
			b.WriteString(pick(r, []string{"<", "&", "<?php x ?>", "<![CDATA[x]]>", "</>", "<a<b>"}))
		}
	}
	return b.String()
}

// genCSS builds a random stylesheet with comments, at-rules and odd
// spacing.
func genCSS(r core.Rng) string {
	var b strings.Builder
	for range 3 + core.IntN(r, 12) {
		switch core.IntN(r, 5) {
		case 0:
			b.WriteString(pick(r, []string{"@import url(\"/a.css?v=1\");", "@media (max-width : 100px) {", "}", "@font-face{src:url(f.woff2)}", "/* c */"}))
		default:
			sel := pick(r, []string{"a", ".x > .y", "div  p", "a:hover", "#id , .c", "ul ~ li", "*"})
			b.WriteString(sel + pick(r, []string{"{", " {", "\n{"}))
			for range core.IntN(r, 4) {
				fmt.Fprintf(&b, "%s%s%s%s", pick(r, []string{"color", "margin", "background", "font-family", "--v"}),
					pick(r, []string{":", " : ", ":\t"}), pick(r, []string{"red", "0 auto", "url( /i.png?v=9 )", "\"a\" , b", "calc(1px + 2px)", "rgba(0, 0, 0, .5) !important"}),
					pick(r, []string{";", " ;", ";;", ""}))
			}
			b.WriteString(pick(r, []string{"}", " }\n", ";}"}))
		}
	}
	return b.String()
}

// genJSON builds a random JSON value.
func genJSON(r core.Rng, depth int) string {
	if depth > 3 {
		return pick(r, []string{"1", "\"s\"", "null", "true", "2.50", "-0"})
	}
	switch core.IntN(r, 4) {
	case 0:
		var parts []string
		for i := range core.IntN(r, 4) {
			parts = append(parts, fmt.Sprintf("%q:%s", pick(r, []string{"a", "z", "ts", "requestId", "nonce", "id", "k\"q"})+fmt.Sprint(i), genJSON(r, depth+1)))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case 1:
		var parts []string
		for range core.IntN(r, 4) {
			parts = append(parts, genJSON(r, depth+1))
		}
		return "[" + strings.Join(parts, " , ") + "]"
	default:
		return pick(r, []string{"1", "\"s\\n\"", "null", "false", "1e3", "\"2026-01-01T00:00:00Z\"", "\"\\u00e9\""})
	}
}

// genXML builds a random XML document (sometimes not well-formed).
func genXML(r core.Rng) string {
	var b strings.Builder
	if core.IntN(r, 2) == 0 {
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	}
	b.WriteString(`<root xmlns="urn:x" xmlns:p="urn:p" b="2" a="1">`)
	for range core.IntN(r, 10) {
		switch core.IntN(r, 5) {
		case 0:
			b.WriteString(`<p:item z="&amp;" y='q'>` + pick(r, []string{"t", " sp  ace ", "a&lt;b", "<![CDATA[x<y]]>"}) + `</p:item>`)
		case 1:
			b.WriteString("<!-- c -->")
		case 2:
			b.WriteString("<e/>")
		case 3:
			b.WriteString("<?pi data?>")
		case 4:
			b.WriteString(pick(r, []string{"text", "\n  ", "<bad", "&unknown;"}))
		}
	}
	b.WriteString("</root>")
	return b.String()
}

// genHeaders builds a random raw header block.
func genHeaders(r core.Rng) string {
	var b strings.Builder
	for range core.IntN(r, 8) {
		b.WriteString(pick(r, []string{
			"Content-Security-Policy: script-src 'self' 'nonce-" + randToken(r, 12, alnumChars) + "' https://cdn;  default-src 'none'",
			"Set-Cookie: sid=" + randToken(r, 16, alnumChars) + "; Path=/; HttpOnly; Expires=Wed, 21 Oct 2026 07:28:00 GMT",
			"Date: Mon, 01 Jan 2026 00:00:00 GMT", "X-Frame-Options: DENY", "Server:  nginx/1.25 ", "Location: /login?next=" + randToken(r, 6, alnumChars),
			"Strict-Transport-Security: Max-Age=100; includeSubDomains", "X-Request-Id: " + randToken(r, 8, alnumChars), "bad line", "Content-Type: Text/HTML",
		}))
		b.WriteString("\n")
	}
	return b.String()
}

// genText builds random text lines.
func genTextDoc(r core.Rng) string {
	var b strings.Builder
	for range core.IntN(r, 12) {
		b.WriteString(pick(r, []string{"line", "  indented", "trailing  \t", "", "\r", "at 2026-11-13T10:00:00+01:00 ok", "\xEF\xBB\xBFbom", "interface X { a: string }"}))
		b.WriteString(pick(r, []string{"\n", "\r\n", "\n\n\n"}))
	}
	return b.String()
}

// genJS builds a small JavaScript program, sometimes with a syntax error.
func genJS(r core.Rng) string {
	var b strings.Builder
	for range 1 + core.IntN(r, 8) {
		b.WriteString(pick(r, []string{
			"var a = 1;", "let t = 1762596000123;", "fetch('/api/x/' + id, {method: 'POST'});", "function f(a,b){return a+b}",
			"const s = `t ${x} 2026-11-13T10:00:00Z`;", "x = {\"k\": \"8f14e45fceea167a5a36dedd4bea2543\"};", "if (a) { b() } else c()",
			"var re = /ab+c/g;", "import('./m.js')", "/* c */", "// line", "a = b\n(c)", "class K { m() { return this } }",
			"var x = ;", "}", "'unterminated", "label: for(;;) break label;", "export default 1;",
		}))
		b.WriteString(pick(r, []string{"\n", " ", ";", "\r\n"}))
	}
	return b.String()
}
