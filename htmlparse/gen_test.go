package htmlparse

import (
	"strconv"
	"strings"

	"github.com/genuinebnt/flowforge/core"
)

// Generated inputs for the differential, invariant and benchmark tests.
// Everything is drawn from a core.Rng so a failure reproduces from its
// seed (testkit.Rng honours FF_SEED).

var soupTags = []string{
	"html", "head", "body", "p", "div", "span", "a", "b", "i", "em", "strong", "u", "s", "code",
	"font", "nobr", "big", "small", "tt", "table", "tr", "td", "th", "tbody", "thead", "tfoot",
	"caption", "col", "colgroup", "ul", "ol", "li", "dl", "dt", "dd", "h1", "h2", "h3", "form",
	"input", "button", "select", "option", "optgroup", "textarea", "title", "script", "style",
	"pre", "listing", "svg", "math", "path", "circle", "foreignObject", "desc", "mi", "mo",
	"mtext", "annotation-xml", "template", "frameset", "frame", "noscript", "plaintext", "xmp",
	"iframe", "noembed", "noframes", "center", "hr", "br", "img", "image", "area", "applet",
	"marquee", "object", "main", "nav", "section", "article", "aside", "header", "footer",
	"address", "blockquote", "details", "summary", "dialog", "figure", "ruby", "rb", "rt", "rp",
	"rtc", "label", "meta", "link", "base", "param", "embed", "wbr", "keygen", "custom-el", "x",
}

var soupAttrs = []string{"id", "class", "href", "src", "type", "name", "value", "style", "title",
	"xlink:href", "xml:lang", "xmlns", "definitionurl", "viewbox", "encoding", "color", "size", "face",
	"hidden", "selected", "multiple", "data-x"}

var soupText = []string{"hello", "world", " ", "\n", "\t", "a b", "&amp;", "&lt;", "&gt;", "&copy",
	"&notin;", "&#65;", "&#x41;", "&#0;", "&#x80;", "&bogus;", "&", "<", ">", "\"", "'", "=", "x",
	"café", "☃", "1 < 2", "a&b", "--", "]]>"}

// genTagSoup returns a random, frequently malformed HTML document of
// roughly n bytes. It avoids the constructs where golang.org/x/net/html
// is known to deviate from the current standard (processing
// instructions, <search>, <selectedcontent>, template-scoped forms and
// NUL), so the differential test compares like with like.
func genTagSoup(r core.Rng, n int) []byte {
	var b strings.Builder
	if core.IntN(r, 3) == 0 {
		b.WriteString("<!DOCTYPE html>")
	}
	open := []string{}
	for b.Len() < n {
		switch k := core.IntN(r, 20); {
		case k < 7:
			tag := soupTags[core.IntN(r, len(soupTags))]
			if tag == "template" && strings.Contains(b.String(), "form") {
				tag = "div"
			}
			if tag == "form" && strings.Contains(b.String(), "template") {
				tag = "div"
			}
			b.WriteByte('<')
			b.WriteString(randCase(r, tag))
			for range core.IntN(r, 3) {
				b.WriteByte(' ')
				b.WriteString(soupAttrs[core.IntN(r, len(soupAttrs))])
				switch core.IntN(r, 4) {
				case 0:
				case 1:
					b.WriteString("=v" + strconv.Itoa(core.IntN(r, 3)))
				default:
					b.WriteString(`="` + soupText[core.IntN(r, len(soupText))] + `"`)
				}
			}
			if core.IntN(r, 8) == 0 {
				b.WriteByte('/')
			}
			b.WriteByte('>')
			open = append(open, tag)
		case k < 11:
			var tag string
			if len(open) > 0 && core.IntN(r, 4) != 0 {
				tag = open[len(open)-1-core.IntN(r, min(len(open), 3))]
			} else {
				tag = soupTags[core.IntN(r, len(soupTags))]
			}
			b.WriteString("</" + randCase(r, tag) + ">")
			if len(open) > 0 {
				open = open[:len(open)-1]
			}
		case k < 18:
			b.WriteString(soupText[core.IntN(r, len(soupText))])
		case k == 18:
			// x/net/html decodes character references in comment data
			// (oracle deviation D6), so comments here carry none.
			b.WriteString("<!--" + strings.ReplaceAll(soupText[core.IntN(r, len(soupText))], "&", "") + "-->")
		default:
			if strings.Contains(b.String(), "svg") || strings.Contains(b.String(), "math") {
				// No '&' in CDATA either (D6).
				b.WriteString("<![CDATA[" + strings.ReplaceAll(soupText[core.IntN(r, len(soupText))], "&", "") + "]]>")
			} else {
				b.WriteString("<!x>")
			}
		}
	}
	return []byte(b.String())
}

func randCase(r core.Rng, s string) string {
	if core.IntN(r, 6) != 0 {
		return s
	}
	return strings.ToUpper(s)
}

// genWellFormed returns a conforming-style document of roughly n bytes:
// properly nested flow and phrasing content, tables with explicit
// sections, lists, forms, comments and entities. Its trees are stable
// under serialize-then-reparse (HTML-INV-03).
func genWellFormed(r core.Rng, n int) []byte {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html><html><head><title>T &amp; t</title><meta charset=\"utf-8\"><style>p { color: red }</style></head><body>")
	for b.Len() < n {
		genBlock(r, &b, 0)
	}
	b.WriteString("</body></html>")
	return []byte(b.String())
}

var wfBlocks = []string{"div", "section", "article", "p", "ul", "table", "pre", "blockquote", "form", "h2"}
var wfInline = []string{"a", "b", "i", "em", "strong", "span", "code", "small"}

func genInline(r core.Rng, b *strings.Builder, depth int) {
	for range 1 + core.IntN(r, 3) {
		switch k := core.IntN(r, 6); {
		case k < 3 || depth > 3:
			b.WriteString([]string{"Lorem ipsum", " dolor sit amet", " café &amp; bar", " 1 &lt; 2", " &copy; 2024", " x y"}[core.IntN(r, 6)])
		case k == 3:
			b.WriteString(`<img src="i.png" alt="a &quot;b&quot;">`)
		default:
			t := wfInline[core.IntN(r, len(wfInline))]
			b.WriteString("<" + t)
			if t == "a" {
				b.WriteString(` href="/p?q=1&amp;r=2"`)
			}
			b.WriteString(` class="c` + strconv.Itoa(core.IntN(r, 5)) + `">`)
			genInline(r, b, depth+1)
			b.WriteString("</" + t + ">")
		}
	}
}

func genBlock(r core.Rng, b *strings.Builder, depth int) {
	t := wfBlocks[core.IntN(r, len(wfBlocks))]
	if depth > 4 {
		t = "p"
	}
	switch t {
	case "p", "h2":
		b.WriteString("<" + t + ">")
		genInline(r, b, 0)
		b.WriteString("</" + t + ">")
	case "pre":
		b.WriteString("<pre>code\n  more &lt;code&gt;</pre>")
	case "ul":
		b.WriteString("<ul>")
		for range 1 + core.IntN(r, 4) {
			b.WriteString("<li>")
			genInline(r, b, 0)
			b.WriteString("</li>")
		}
		b.WriteString("</ul>")
	case "table":
		b.WriteString("<table><thead><tr><th>H</th></tr></thead><tbody>")
		for range 1 + core.IntN(r, 3) {
			b.WriteString("<tr><td>")
			genInline(r, b, 0)
			b.WriteString("</td><td>2</td></tr>")
		}
		b.WriteString("</tbody></table>")
	case "form":
		b.WriteString(`<form action="/s" method="post"><input type="text" name="q" value="v"><select name="s"><option value="1">One</option><option selected>Two</option></select><textarea name="t">text</textarea><button type="submit">Go</button></form>`)
	default:
		b.WriteString("<" + t + ` id="b` + strconv.Itoa(core.IntN(r, 1000)) + `">`)
		for range 1 + core.IntN(r, 3) {
			genBlock(r, b, depth+1)
		}
		b.WriteString("<!-- c -->")
		b.WriteString("</" + t + ">")
	}
}
