package content

import (
	"strings"

	"github.com/genuinebnt/flowforge/cssparse"
)

// cssRules is the css@2 rule table (css@1 masked an all-hex asset-name
// part only when it had a digit, see isHashLike).
var cssRules = []string{
	"tokenize:css-syntax-3 §4 (cssparse); tokens re-emitted from their source text",
	"drop:comments (a comment separates like white space)",
	"space:white-space runs → one space; none next to { } ; , ) or after ( : and function tokens; none around ! and around > + ~ outside parentheses; none before a declaration colon",
	"lines:newline after { ; } and before }; a ; directly before } or ; is dropped",
	"string:re-quoted with \" when the value needs no escape",
	"url:url(value) without inner white space when the value needs no escape",
	"invalid:strings cut by eof are closed; an escape cut by eof is dropped; a final hex escape gets its space; bad-string and bad-url tokens become fixed placeholders",
	"mask:url values → cache_bust query values, build_hash asset names (8-64 of [0-9A-Za-z_-], all hex or letters and digits)",
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
	if n := len(toks); n > 0 {
		// Only the last token can end in an escape cut by EOF. When that
		// escape is all it is, it reads as nothing once a newline
		// follows, so treat it as white space from the start.
		last := &toks[n-1]
		if txt := src[last.Start:last.End]; last.Kind != cssparse.TokenDelim && last.Kind != cssparse.TokenString &&
			strings.TrimRight(txt, `\`) == "" && len(txt)%2 == 1 {
			last.Kind = cssparse.TokenWhitespace
		}
	}
	var b strings.Builder
	b.Grow(len(body))
	pending := false  // white space or a comment was seen since the last emitted token
	lineStart := true // nothing emitted on the current line
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
			lineStart = false
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
		case cssparse.TokenBadString, cssparse.TokenBadURL:
			// Invalid tokens swallow what follows them differently once
			// re-spaced; a fixed placeholder keeps the output stable.
			if t.Kind == cssparse.TokenBadString {
				text = `"__FF_BAD_STRING__"`
			} else {
				text = "url(__FF_BAD_URL__)"
			}
		}
		if t.Kind != cssparse.TokenDelim && t.Kind != cssparse.TokenString && strings.HasSuffix(text, `\`) &&
			(len(text)-len(strings.TrimRight(text, `\`)))%2 == 1 {
			// An escape cut by EOF ("\" + EOF is a valid escape of U+FFFD,
			// §4.3.8) stops being one once a newline follows; drop it.
			text = text[:len(text)-1]
			if text == "" {
				pending = true
				continue
			}
		}
		if pending && !lineStart && !tight && !noSpaceAfter {
			b.WriteByte(' ')
		}
		if endsInHexEscape(text) {
			// CSS Syntax §4.3.7: a hex escape consumes one following white
			// space. The source ended right after it (EOF); write that
			// space now so the space or newline the normalizer adds next
			// is not absorbed on re-normalization.
			text += " "
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
		if t.Flags&cssparse.FlagUnterminated != 0 && len(text) > 0 {
			// Close a string cut by EOF (CSS Syntax §4.3.5: EOF ends it),
			// so the newline the normalizer appends cannot extend it. A
			// final lone backslash is dropped as EOF drops it.
			body := text
			if n := len(body) - len(strings.TrimRight(body, `\`)); n%2 == 1 {
				body = body[:len(body)-1]
			}
			return body + text[:1]
		}
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

// endsInHexEscape reports text ending in "\" + 1..6 hex digits, the
// backslash itself not escaped.
func endsInHexEscape(text string) bool {
	i := len(text)
	for i > 0 && len(text)-i < 6 && isHexString(text[i-1:i]) {
		i--
	}
	if i == len(text) || i == 0 || text[i-1] != '\\' {
		return false
	}
	n := 0
	for j := i - 1; j >= 0 && text[j] == '\\'; j-- {
		n++
	}
	return n%2 == 1
}
