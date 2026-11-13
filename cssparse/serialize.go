package cssparse

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Serialize writes tokens back to CSS from their values (not their source
// text), dropping comments, following CSS Syntax 3 §9 "Serialization": an
// empty comment is inserted between two tokens that would otherwise
// re-tokenize differently. Re-tokenizing the output gives the same kinds,
// values and flags (CSS-INV-03), except that bad-string and bad-url
// tokens lose nothing (they carry no value) and unterminated strings and
// urls come back terminated.
func Serialize(toks []Token) []byte {
	var b strings.Builder
	prev := TokenInvalid
	prevDelim := ""
	for i := range toks {
		t := &toks[i]
		if t.Kind == TokenComment {
			continue
		}
		if needsComment(prev, prevDelim, t) {
			b.WriteString("/**/")
		}
		switch t.Kind {
		case TokenIdent:
			writeIdent(&b, t.Value)
		case TokenFunction:
			writeIdent(&b, t.Value)
			b.WriteByte('(')
		case TokenAtKeyword:
			b.WriteByte('@')
			writeIdent(&b, t.Value)
		case TokenHash:
			b.WriteByte('#')
			if t.Flags&FlagHashID != 0 {
				writeIdent(&b, t.Value)
			} else {
				writeName(&b, t.Value)
			}
		case TokenString:
			writeString(&b, t.Value)
		case TokenBadString:
			b.WriteString("\"\n")
		case TokenURL:
			b.WriteString("url(")
			writeURL(&b, t.Value)
			b.WriteByte(')')
		case TokenBadURL:
			b.WriteString("url(())")
		case TokenDelim:
			if t.Value == "\\" {
				b.WriteString("\\\n")
			} else {
				b.WriteString(t.Value)
			}
		case TokenNumber, TokenPercentage, TokenDimension:
			b.WriteString(formatNumber(t))
			switch t.Kind {
			case TokenPercentage:
				b.WriteByte('%')
			case TokenDimension:
				writeUnit(&b, t.Value)
			}
		case TokenWhitespace:
			if prev == TokenDelim && prevDelim == "\\" {
				// The "\" delim already wrote the newline that makes it a
				// delim; a newline merges with it.
			} else {
				b.WriteByte(' ')
			}
		case TokenUnicodeRange:
			b.WriteString("U+")
			b.WriteString(strconv.FormatUint(uint64(t.RangeStart), 16))
			if t.RangeEnd != t.RangeStart {
				b.WriteByte('-')
				b.WriteString(strconv.FormatUint(uint64(t.RangeEnd), 16))
			}
		default:
			b.WriteString(fixedText[t.Kind])
		}
		prev = t.Kind
		prevDelim = ""
		if t.Kind == TokenDelim {
			prevDelim = t.Value
		}
	}
	return []byte(b.String())
}

var fixedText = [tokenKindCount]string{
	TokenCDO: "<!--", TokenCDC: "-->", TokenColon: ":", TokenSemicolon: ";",
	TokenComma: ",", TokenOpenSquare: "[", TokenCloseSquare: "]",
	TokenOpenParen: "(", TokenCloseParen: ")", TokenOpenCurly: "{",
	TokenCloseCurly: "}", TokenIncludeMatch: "~=", TokenDashMatch: "|=",
	TokenPrefixMatch: "^=", TokenSuffixMatch: "$=", TokenSubstringMatch: "*=",
	TokenColumn: "||",
}

// formatNumber writes a number so it re-tokenizes with the same value and
// type flag.
func formatNumber(t *Token) string {
	s := strconv.FormatFloat(t.Number, 'g', -1, 64)
	if t.Number == 0 {
		s = "0"
	}
	integer := t.Flags&FlagInteger != 0
	hasPoint := strings.ContainsAny(s, ".eE")
	if strings.ContainsAny(s, "IN") { // ±Inf: out of range
		sign := ""
		if t.Number < 0 {
			sign = "-"
		}
		if integer {
			return sign + "1" + strings.Repeat("0", 400)
		}
		return sign + "1e999"
	}
	if integer && hasPoint {
		// A huge integer prints with an exponent; spell it out.
		s = strconv.FormatFloat(t.Number, 'f', -1, 64)
	}
	if !integer && !hasPoint {
		s += ".0"
	}
	return s
}

// needsComment reports whether an empty comment must separate the
// previous token from t (the §9 table, extended conservatively).
func needsComment(prev TokenKind, prevDelim string, t *Token) bool {
	k := t.Kind
	identLike := k == TokenIdent || k == TokenFunction || k == TokenURL || k == TokenBadURL
	numeric := k == TokenNumber || k == TokenPercentage || k == TokenDimension
	delim := ""
	if k == TokenDelim {
		delim = t.Value
	}
	switch prev {
	case TokenWhitespace:
		return k == TokenWhitespace
	case TokenIdent:
		return identLike || numeric || delim == "-" || k == TokenCDC || k == TokenOpenParen ||
			k == TokenUnicodeRange
	case TokenAtKeyword, TokenHash, TokenDimension:
		return identLike || numeric || delim == "-" || k == TokenCDC || k == TokenUnicodeRange
	case TokenNumber:
		return identLike || numeric || delim == "%" || delim == "-" || delim == "." ||
			k == TokenCDC || k == TokenUnicodeRange
	case TokenPercentage, TokenUnicodeRange:
		return numeric || identLike || delim == "?" || delim == "-"
	case TokenDelim:
		switch prevDelim {
		case "#", "-":
			return identLike || numeric || delim == "-" || k == TokenCDC || delim == "\\" || k == TokenUnicodeRange
		case "@":
			return identLike || delim == "-" || k == TokenCDC || delim == "\\" || k == TokenUnicodeRange
		case ".", "+":
			return numeric
		case "/":
			return delim == "*"
		case "<":
			return delim == "!"
		case "~", "|", "^", "$", "*":
			return delim == "=" || (prevDelim == "|" && delim == "|")
		}
	}
	return false
}

func writeHexEscape(b *strings.Builder, r rune) {
	b.WriteByte('\\')
	b.WriteString(strconv.FormatInt(int64(r), 16))
	b.WriteByte(' ')
}

// writeIdent serializes an identifier (CSSOM "serialize an identifier").
func writeIdent(b *strings.Builder, s string) {
	if s == "-" {
		b.WriteString("\\-")
		return
	}
	i := 0
	for j, r := range s {
		switch {
		case r == utf8.RuneError && !strings.HasPrefix(s[j:], "�"):
			b.WriteString("�")
		case r == 0:
			b.WriteString("�")
		case (r >= 1 && r <= 0x1F) || r == 0x7F:
			writeHexEscape(b, r)
		case isDigit(r) && (i == 0 || (i == 1 && s[0] == '-')):
			writeHexEscape(b, r)
		case isIdentChar(r):
			b.WriteRune(r)
		default:
			b.WriteByte('\\')
			b.WriteRune(r)
		}
		i++
	}
}

// writeName serializes an unrestricted hash name: like an identifier but
// digits and "-" may start it.
func writeName(b *strings.Builder, s string) {
	for _, r := range s {
		switch {
		case (r >= 1 && r <= 0x1F) || r == 0x7F:
			writeHexEscape(b, r)
		case isIdentChar(r):
			b.WriteRune(r)
		default:
			b.WriteByte('\\')
			b.WriteRune(r)
		}
	}
}

// writeUnit serializes a dimension unit: an identifier whose leading "e"
// must not read as an exponent.
func writeUnit(b *strings.Builder, s string) {
	if len(s) >= 2 && (s[0] == 'e' || s[0] == 'E') &&
		(isDigit(rune(s[1])) || s[1] == '-' || s[1] == '+') {
		writeHexEscape(b, rune(s[0]))
		b.WriteString(identTail(s[1:]))
		return
	}
	writeIdent(b, s)
}

// identTail serializes s as the continuation of an identifier (no
// leading-digit escaping).
func identTail(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case (r >= 1 && r <= 0x1F) || r == 0x7F:
			writeHexEscape(&b, r)
		case isIdentChar(r):
			b.WriteRune(r)
		default:
			b.WriteByte('\\')
			b.WriteRune(r)
		}
	}
	return b.String()
}

// writeString serializes a string (CSSOM "serialize a string").
func writeString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case (r >= 1 && r <= 0x1F) || r == 0x7F:
			writeHexEscape(b, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

// writeURL serializes an unquoted url-token value.
func writeURL(b *strings.Builder, s string) {
	for _, r := range s {
		switch {
		case r == '"' || r == '\'' || r == '(' || r == ')' || r == '\\' || isWhitespace(r) ||
			isNonPrintable(r) || r == '\r' || r == '\f':
			writeHexEscape(b, r)
		default:
			b.WriteRune(r)
		}
	}
}
