package htmlparse

import (
	"bytes"
	"sort"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Differential oracle: golang.org/x/net/html (test-only dependency). This
// file renders its tree in the same html5lib dump format as Dump.

// oracleParse parses input with x/net/html (scripting off, matching our
// default) and returns its dump.
func oracleParse(input []byte) (string, bool) {
	n, err := html.ParseWithOptions(bytes.NewReader(input), html.ParseOptionEnableScripting(false))
	if err != nil {
		return "", false
	}
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		oracleDump(&b, c, 0)
	}
	return strings.TrimSuffix(b.String(), "\n"), true
}

// oracleParseFragment parses a fragment with x/net/html.
func oracleParseFragment(input []byte, ctx ContextElement) (string, bool) {
	ns := ""
	switch ctx.Namespace {
	case SVGNamespace:
		ns = "svg"
	case MathMLNamespace:
		ns = "math"
	}
	c := &html.Node{Type: html.ElementNode, Data: ctx.Name, Namespace: ns, DataAtom: atom.Lookup([]byte(ctx.Name))}
	nodes, err := html.ParseFragmentWithOptions(bytes.NewReader(input), c, html.ParseOptionEnableScripting(false))
	if err != nil {
		return "error: " + err.Error(), false
	}
	var b strings.Builder
	for _, n := range nodes {
		oracleDump(&b, n, 0)
	}
	return strings.TrimSuffix(b.String(), "\n"), true
}

func oracleDump(b *strings.Builder, n *html.Node, depth int) {
	dumpIndent(b, depth)
	switch n.Type {
	case html.DoctypeNode:
		b.WriteString("<!DOCTYPE ")
		b.WriteString(n.Data)
		var pub, sys string
		for _, a := range n.Attr {
			switch a.Key {
			case "public":
				pub = a.Val
			case "system":
				sys = a.Val
			}
		}
		if pub != "" || sys != "" {
			b.WriteString(` "` + pub + `" "` + sys + `"`)
		}
		b.WriteString(">\n")
	case html.TextNode:
		b.WriteString(`"` + n.Data + "\"\n")
	case html.CommentNode:
		b.WriteString("<!-- " + n.Data + " -->\n")
	case html.ElementNode:
		b.WriteString("<")
		if n.Namespace != "" {
			b.WriteString(n.Namespace + " ")
		}
		b.WriteString(n.Data + ">\n")
		names := make([]string, len(n.Attr))
		vals := map[string]string{}
		for i, a := range n.Attr {
			name := a.Key
			if a.Namespace != "" {
				name = a.Namespace + " " + a.Key
			}
			names[i] = name
			vals[name] = a.Val
		}
		sort.Strings(names)
		for _, name := range names {
			dumpIndent(b, depth+1)
			b.WriteString(name + `="` + vals[name] + "\"\n")
		}
		if n.Namespace == "" && n.DataAtom == atom.Template {
			dumpIndent(b, depth+1)
			b.WriteString("content\n")
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				oracleDump(b, c, depth+2)
			}
			return
		}
	default:
		b.WriteString("#unknown\n")
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		oracleDump(b, c, depth+1)
	}
}
