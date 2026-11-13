package content

import (
	"strings"

	"github.com/genuinebnt/flowforge/cssparse"
)

// cssRules is the css@1 rule table.
var cssRules = []string{
	"tokenize:css-syntax-3 §4 (cssparse); tokens re-emitted from their source text",
	"drop:comments (a comment separates like white space)",
	"space:white-space runs → one space; none next to { } ; , ) or after ( : and function tokens; none around ! and around > + ~ outside parentheses; none before a declaration colon",
	"lines:newline after { ; } and before }; a ; directly before } or ; is dropped",
	"string:re-quoted with \" when the value needs no escape",
	"url:url(value) without inner white space when the value needs no escape",
	"mask:url values → cache_bust query values, build_hash asset names",
}

func normalizeCSS(body []byte, lim Limits, m *masker) normResult {
	toks, diags := cssparse.Tokenize(body, cssparse.Options{MaxInputBytes: lim.MaxResponseBytes})
	r := normResult{grade: GradeComplete}
	for _, d := range diags {
		if d.Code.IsLimit() {
			r.grade = GradePartial
			r.diags = appendOnce(r.diags, "css_"+d.Code.String())
		}
	}
	src := string(body)
	var b strings.Builder
	b.Grow(len(body))
	pending := false   // white space or a comment was seen since the last emitted token
	lineStart := true  // nothing emitted on the current line
	noSpaceAfter := false
	parens, depth := 0, 0
	lineTokens := 0 // tokens emitted on the current line
	// nextSignificant returns the index of the next token that is not
	// white space or a comment.
	nextSignificant := func(i int) int {
		for j := i + 1; j < len(toks); j++ {
			if k := toks[j].Kind; k != cssparse.TokenWhitespace && k != cssparse.TokenComment {
				return j
			}
		}
		return len(toks)
	}
	newline := func() {
		if !lineStart {
			b.WriteByte('\n')
		}
		lineStart, pending, noSpaceAfter = true, false, false
		lineTokens = 0
	}
	for i := 0; i < len(toks); i++ {
		t := &toks[i]
		switch t.Kind {
		case cssparse.TokenWhitespace, cssparse.TokenComment:
			pending = true
			continue
		case cssparse.TokenSemicolon:
			if j := nextSignificant(i); j == len(toks) || toks[j].Kind == cssparse.TokenCloseCurly || toks[j].Kind == cssparse.TokenSemicolon {
				pending = true // a redundant ";" still separates like white space
				continue
			}
			b.WriteByte(';')
			newline()
			continue
		case cssparse.TokenOpenCurly:
			depth++
			b.WriteByte('{')
			lineStart = false
			newline()
			continue
		case cssparse.TokenCloseCurly:
			if depth > 0 {
				depth--
			}
			newline()
			b.WriteByte('}')
			lineStart = false
			newline()
			continue
		}
		text := src[t.Start:t.End]
		tight := false // no space before this token
		after := false // no space after this token
		switch t.Kind {
		case cssparse.TokenComma, cssparse.TokenCloseParen:
			tight = true
			after = t.Kind == cssparse.TokenComma
			if t.Kind == cssparse.TokenCloseParen && parens > 0 {
				parens--
			}
		case cssparse.TokenColon:
			// "color : red" and "color:red" are one declaration: inside a
			// block, a colon right after the line's first token is a
			// declaration colon.
			after = true
			tight = depth > 0 && lineTokens == 1
		case cssparse.TokenOpenParen:
			after = true
			parens++
		case cssparse.TokenFunction:
			after = true
			parens++
		case cssparse.TokenDelim:
			switch {
			case t.Value == "!":
				// "red !important" and "red!important" are one declaration.
				tight, after = true, true
			case parens == 0 && (t.Value == ">" || t.Value == "+" || t.Value == "~"):
				// Combinators. A space after "-" stays: "-->" would
				// re-tokenize as CDC.
				tight, after = !strings.HasSuffix(b.String(), "-"), true
			}
		case cssparse.TokenString:
			text = cssString(t, text, m)
		case cssparse.TokenURL:
			text = cssURL(t, text, m)
		}
		if pending && !lineStart && !tight && !noSpaceAfter {
			b.WriteByte(' ')
		}
		b.WriteString(text)
		lineStart, pending, noSpaceAfter = false, false, after
		lineTokens++
	}
	newline()
	r.text = []byte(b.String())
	return r
}

// cssSafe reports a value that can be written between double quotes, or
// inside url( ), without escapes.
func cssSafe(v string, inURL bool) bool {
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c < 0x20 || c == 0x7f || c == '"' || c == '\\':
			return false
		case inURL && (c == ' ' || c == '\'' || c == '(' || c == ')'):
			return false
		}
	}
	return true
}

func cssString(t *cssparse.Token, text string, m *masker) string {
	if !cssSafe(t.Value, false) {
		return text
	}
	v := t.Value
	if strings.ContainsAny(v, "/?.") {
		v = maskURL(v, m)
	}
	return `"` + v + `"`
}

func cssURL(t *cssparse.Token, text string, m *masker) string {
	if !cssSafe(t.Value, true) || t.Value == "" {
		return text
	}
	return "url(" + maskURL(t.Value, m) + ")"
}
