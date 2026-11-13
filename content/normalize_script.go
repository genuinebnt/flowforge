package content

import (
	"bytes"
	"strings"

	"github.com/genuinebnt/flowforge/jsparse"
)

// jsRules is the js@1 rule table.
var jsRules = []string{
	"parse:ecma-262 15th ed. (jsparse) tolerant; script goal, module goal when only it parses cleanly",
	"emit:jsparse readable printer (one statement per line, canonical spacing, double quotes, comments dropped); 'directives' re-quoted with \"",
	"fallback:on any diagnostic, line normalization (leading space dropped, inner and trailing space runs → one space, lf); grade partial",
	"mask:string literals (not keys, not directives, no '/') that are timestamps/uuids/build ids → class placeholder string",
	"mask:iso-8601 date-times inside string and template literals → timestamp",
	"mask:13-digit epoch-millisecond numeric literals → timestamp identifier",
}

// parseJS parses src tolerantly, as a script, or as a module when the
// script parse has diagnostics and the module parse has none (module-only
// syntax: import/export, top-level await).
func parseJS(src []byte, tokens bool) *jsparse.Program {
	p, _ := jsparse.Parse(src, jsparse.Options{Tolerant: true, Tokens: tokens})
	if len(p.Diagnostics) == 0 {
		return p
	}
	if bytes.Contains(src, []byte("import")) || bytes.Contains(src, []byte("export")) || bytes.Contains(src, []byte("await")) {
		m, _ := jsparse.Parse(src, jsparse.Options{Tolerant: true, Tokens: tokens, SourceType: jsparse.SourceModule})
		if len(m.Diagnostics) == 0 {
			return m
		}
	}
	return p
}

// normalizeJSProgram normalizes a program parsed from body (it masks the
// program in place).
func normalizeJSProgram(prog *jsparse.Program, body []byte, m *masker) normResult {
	if len(prog.Diagnostics) > 0 {
		return normResult{text: jsFallbackLines(body), grade: GradePartial, diags: []string{"js_syntax_fallback"}}
	}
	maskJSLiterals(prog, m)
	out := jsparse.Print(prog, jsparse.PrintOptions{})
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return normResult{text: out, grade: GradeComplete}
}

// maskJSLiterals rewrites volatile literals in place. New strings are
// appended to prog.Strings rather than overwriting shared entries.
func maskJSLiterals(prog *jsparse.Program, m *masker) {
	keys := make(map[jsparse.NodeID]bool)
	for i := range prog.Nodes {
		n := &prog.Nodes[i]
		switch n.Kind {
		case jsparse.KProperty, jsparse.KMethodDefinition, jsparse.KPropertyDefinition:
			if !n.Has(jsparse.NComputed) {
				keys[n.A] = true
			}
		case jsparse.KExpressionStatement:
			if n.Has(jsparse.NDirective) {
				keys[n.A] = true
			}
		case jsparse.KImportDeclaration, jsparse.KExportAllDeclaration, jsparse.KExportNamedDeclaration,
			jsparse.KImportAttribute:
			keys[n.A], keys[n.B] = true, true
		}
	}
	canonicalDirectives(prog)
	newStr := func(s string) jsparse.StrID {
		prog.Strings = append(prog.Strings, s)
		return jsparse.StrID(len(prog.Strings) - 1)
	}
	for i := range prog.Nodes {
		id := jsparse.NodeID(i)
		n := &prog.Nodes[i]
		switch {
		case n.Kind == jsparse.KLiteral && n.Op == jsparse.LitString && !keys[id]:
			v := prog.Strings[n.Str]
			if c, ok := volatileScalar(v); ok {
				m.hit(c, v)
				n.Str = newStr(maskPlaceholder(c))
			} else if !strings.ContainsRune(v, '/') {
				if mv := maskText(v, m); mv != v {
					n.Str = newStr(mv)
				}
			}
		case n.Kind == jsparse.KLiteral && n.Op == jsparse.LitNumber && !keys[id]:
			if n.Num >= minEpochSeconds*1000 && n.Num <= maxEpochSeconds*1000 && n.Num == float64(int64(n.Num)) {
				// An identifier keeps the expression shape; the printer
				// handles it wherever a numeric literal could stand.
				m.hit(MaskTimestamp, jsparse.NumberToString(n.Num))
				*n = jsparse.Node{Kind: jsparse.KIdentifier, Span: n.Span, A: jsparse.NoNode, B: jsparse.NoNode,
					C: jsparse.NoNode, D: jsparse.NoNode, Str: newStr(maskPlaceholder(MaskTimestamp))}
			}
		case n.Kind == jsparse.KTemplateElement:
			raw := prog.Strings[n.Str2]
			if !strings.ContainsRune(raw, '/') {
				if mv := maskText(raw, m); mv != raw {
					n.Str2 = newStr(mv)
					n.Str = newStr(maskText(prog.Strings[n.Str], newMasker()))
				}
			}
		}
	}
}

// canonicalDirectives rewrites single-quoted directives ('use strict')
// to double quotes in prog.Source, which is where the printer copies a
// directive from (its raw text is significant, ECMA-262 §11.2.1). Only
// directives without quotes or backslashes inside are rewritten, so the
// directive's value and every span stay the same.
func canonicalDirectives(prog *jsparse.Program) {
	var src []byte
	for i := range prog.Nodes {
		n := &prog.Nodes[i]
		if n.Kind != jsparse.KExpressionStatement || !n.Has(jsparse.NDirective) || n.A == jsparse.NoNode {
			continue
		}
		sp := prog.Nodes[n.A].Span
		raw := prog.Source[sp.Start:sp.End]
		if len(raw) < 2 || raw[0] != '\'' || raw[len(raw)-1] != '\'' || strings.ContainsAny(raw[1:len(raw)-1], "\"'\\") {
			continue
		}
		if src == nil {
			src = []byte(prog.Source)
		}
		src[sp.Start], src[sp.End-1] = '"', '"'
	}
	if src != nil {
		prog.Source = string(src)
	}
}

// jsFallbackLines is the line normalization used when the parser reports
// diagnostics. It never turns an invalid program into a valid one (white
// space runs shrink but never vanish inside a line, and line breaks stay),
// so normalizing its output takes the same path again (idempotence).
func jsFallbackLines(body []byte) []byte {
	var w lineWriter
	text := strings.ReplaceAll(strings.ReplaceAll(string(body), "\r\n", "\n"), "\r", "\n")
	for line := range strings.SplitSeq(text, "\n") {
		w.line(collapseInner(line))
	}
	out := w.bytes()
	// SplitSeq yields a final empty line after a trailing "\n"; drop the
	// one extra "\n" it produces so the output is a fixed point.
	if bytes.HasSuffix(out, []byte("\n\n")) && strings.HasSuffix(text, "\n") {
		out = out[:len(out)-1]
	}
	return out
}

// collapseInner drops leading blanks and turns every inner or trailing run
// of spaces and tabs into a single space.
func collapseInner(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	space := false
	for ; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' {
			space = true
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteByte(c)
	}
	if space {
		b.WriteByte(' ')
	}
	return b.String()
}
