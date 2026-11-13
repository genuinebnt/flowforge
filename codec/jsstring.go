package codec

import (
	"slices"
	"unicode/utf8"
)

// SurrogateMode says how a decoded UTF-16 surrogate that is not part of a
// pair is written.
type SurrogateMode uint8

const (
	// SurrogateWTF8 writes it as its 3-byte generalized UTF-8 (WTF-8)
	// form, as jsparse does for cooked string values, so the string
	// round-trips through EncodeJSString. The zero value.
	SurrogateWTF8 SurrogateMode = iota
	// SurrogateReplace writes U+FFFD, so the output is always valid UTF-8
	// (for indexing and display).
	SurrogateReplace
)

// JSOptions configures DecodeJSString.
type JSOptions struct {
	// Mode: Strict accepts exactly the string literal bodies that are
	// valid in ECMAScript strict mode code (ECMA-262 §12.9.4 without
	// Annex B.1.2): no LegacyOctalEscapeSequence, no \8 or \9, no raw CR
	// or LF, no malformed \x or \u, code points at most U+10FFFF, no
	// invalid UTF-8. Lenient decodes sloppy-mode escapes (B.1.2) and keeps
	// malformed escapes, raw line terminators and invalid bytes as they
	// are.
	Mode Mode
	// Quote, when ' or ", is the delimiter the body came from; strict
	// mode then rejects it unescaped. 0 checks nothing.
	Quote byte
	// Surrogates selects the output form of unpaired surrogates.
	Surrogates SurrogateMode
}

// jsState is the string-literal decoder state, named after the ECMA-262
// §12.9.4 productions.
type jsState uint8

const (
	// jsStringCharacter: a SourceCharacter, LineContinuation, or '\'
	// starting an EscapeSequence (DoubleStringCharacter /
	// SingleStringCharacter).
	jsStringCharacter jsState = iota
	// jsEscapeSequence: '\' read; the escape letter follows.
	jsEscapeSequence
	// jsHexEscapeSequence: "\x" read; two HexDigits follow.
	jsHexEscapeSequence
	// jsUnicodeEscapeSequence: "\u" read; four HexDigits or '{' follow.
	jsUnicodeEscapeSequence
	// jsCodePoint: "\u{" read; HexDigits up to '}' (the CodePoint
	// production, value at most 0x10FFFF).
	jsCodePoint
	// jsLegacyOctal: a LegacyOctalEscapeSequence digit read (B.1.2);
	// more octal digits may follow.
	jsLegacyOctal
	// jsLineContinuationCR: '\' CR read; an LF completes the CRLF
	// LineTerminatorSequence.
	jsLineContinuationCR
)

// DecodeJSString decodes the body of a JavaScript string literal (the
// text between the quotes) to UTF-8. Escaped surrogate pairs
// (\uD83D\uDE00, in any escape form) combine into one code point.
func DecodeJSString(src []byte, o JSOptions) ([]byte, error) {
	out, err := AppendDecodeJSString(nil, src, o)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []byte{}
	}
	return out, nil
}

// AppendDecodeJSString appends the decoding of src to dst; on error dst
// is returned unchanged.
func AppendDecodeJSString(dst, src []byte, o JSOptions) ([]byte, error) {
	strict := o.Mode == Strict
	d := jsDec{out: slices.Grow(dst, len(src)), high: -1, replace: o.Surrogates == SurrogateReplace}
	state := jsStringCharacter
	esc, n := 0, 0 // start of the current escape; digits read
	var v rune
	// malformed handles an escape that is not well formed: always a
	// SyntaxError in ECMAScript; lenient output keeps its bytes.
	malformed := func(end int) error {
		if strict {
			return errAt(CodeInvalidEscape, esc)
		}
		d.flushHigh()
		d.out = append(d.out, src[esc:end]...)
		state = jsStringCharacter
		return nil
	}
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch state {
		case jsStringCharacter:
			switch {
			case c == '\\':
				state, esc = jsEscapeSequence, i
			case strict && (c == '\n' || c == '\r'):
				// §12.9.4: LineTerminator other than LS/PS is not a
				// string character.
				return dst, errAt(CodeInvalidByte, i)
			case strict && o.Quote != 0 && c == o.Quote:
				return dst, errAt(CodeInvalidByte, i)
			case c < utf8.RuneSelf:
				d.flushHigh()
				d.out = append(d.out, c)
			default:
				r, size := utf8.DecodeRune(src[i:])
				if r == utf8.RuneError && size == 1 && strict {
					return dst, errAt(CodeInvalidByte, i)
				}
				d.flushHigh()
				d.out = append(d.out, src[i:i+size]...)
				i += size - 1
			}
		case jsEscapeSequence:
			state = jsStringCharacter
			switch c {
			case '\n':
				// LineContinuation contributes no code units.
			case '\r':
				state = jsLineContinuationCR
			case 'b':
				d.unit('\b')
			case 't':
				d.unit('\t')
			case 'n':
				d.unit('\n')
			case 'v':
				d.unit('\v')
			case 'f':
				d.unit('\f')
			case 'r':
				d.unit('\r')
			case 'x':
				state, n, v = jsHexEscapeSequence, 0, 0
			case 'u':
				state, n, v = jsUnicodeEscapeSequence, 0, 0
			case '0', '1', '2', '3', '4', '5', '6', '7':
				next := byte(0)
				if i+1 < len(src) {
					next = src[i+1]
				}
				if c == '0' && !(next >= '0' && next <= '9') {
					d.unit(0) // \0 [lookahead ∉ DecimalDigit]
					break
				}
				if strict {
					return dst, errAt(CodeInvalidEscape, esc)
				}
				state, n, v = jsLegacyOctal, 1, rune(c-'0')
			case '8', '9':
				// NonOctalDecimalEscapeSequence (B.1.2): the digit itself.
				if strict {
					return dst, errAt(CodeInvalidEscape, esc)
				}
				d.unit(rune(c))
			default:
				r, size := utf8.DecodeRune(src[i:])
				switch {
				case r == 0x2028 || r == 0x2029:
					// LS and PS are LineTerminators: a LineContinuation.
				case r == utf8.RuneError && size == 1:
					if strict {
						return dst, errAt(CodeInvalidByte, i)
					}
					d.flushHigh()
					d.out = append(d.out, c)
				default:
					// NonEscapeCharacter: the character itself.
					d.flushHigh()
					d.out = append(d.out, src[i:i+size]...)
				}
				i += size - 1
			}
		case jsLineContinuationCR:
			state = jsStringCharacter
			if c != '\n' {
				i--
			}
		case jsHexEscapeSequence:
			if hexVal[c] == 0xFF {
				if err := malformed(i); err != nil {
					return dst, err
				}
				i--
				continue
			}
			v, n = v<<4|rune(hexVal[c]), n+1
			if n == 2 {
				d.unit(v)
				state = jsStringCharacter
			}
		case jsUnicodeEscapeSequence:
			switch {
			case n == 0 && c == '{':
				state = jsCodePoint
			case hexVal[c] != 0xFF:
				v, n = v<<4|rune(hexVal[c]), n+1
				if n == 4 {
					d.unit(v)
					state = jsStringCharacter
				}
			default:
				if err := malformed(i); err != nil {
					return dst, err
				}
				i--
			}
		case jsCodePoint:
			switch {
			case c == '}' && n > 0:
				d.unit(v)
				state = jsStringCharacter
			case hexVal[c] != 0xFF && v<<4|rune(hexVal[c]) <= utf8.MaxRune:
				// Leading zeros are allowed (CodePoint is a MV, not a
				// digit count), so only the value is bounded.
				v, n = v<<4|rune(hexVal[c]), n+1
			default:
				end := i
				if c == '}' || hexVal[c] != 0xFF {
					end = i + 1
				}
				if err := malformed(end); err != nil {
					return dst, err
				}
				if end == i {
					i--
				}
			}
		case jsLegacyOctal:
			// B.1.2: ZeroToThree OctalDigit OctalDigit, or FourToSeven
			// OctalDigit; the value is at most 0o377.
			first := src[esc+1]
			if c >= '0' && c <= '7' && (n == 1 || n == 2 && first <= '3') {
				v, n = v<<3|rune(c-'0'), n+1
				if n == 3 || first >= '4' {
					d.unit(v)
					state = jsStringCharacter
				}
				continue
			}
			d.unit(v)
			state = jsStringCharacter
			i--
		}
	}
	switch state {
	case jsEscapeSequence:
		// A lone trailing backslash.
		if strict {
			return dst, errAt(CodeTruncated, len(src))
		}
		d.flushHigh()
		d.out = append(d.out, '\\')
	case jsHexEscapeSequence, jsUnicodeEscapeSequence, jsCodePoint:
		if strict {
			return dst, errAt(CodeTruncated, len(src))
		}
		d.flushHigh()
		d.out = append(d.out, src[esc:]...)
	case jsLegacyOctal:
		d.unit(v)
	}
	d.flushHigh()
	return d.out, nil
}

// jsDec collects decoded UTF-16 code units as UTF-8, pairing surrogates.
type jsDec struct {
	out     []byte
	high    rune // pending high surrogate, -1 for none
	replace bool
}

// unit appends one code unit or code point from an escape. A high
// surrogate waits for a low one; anything else ends the wait.
func (d *jsDec) unit(r rune) {
	if r >= 0xDC00 && r <= 0xDFFF && d.high >= 0 {
		d.out = utf8.AppendRune(d.out, 0x10000+(d.high-0xD800)<<10+(r-0xDC00))
		d.high = -1
		return
	}
	d.flushHigh()
	switch {
	case r >= 0xD800 && r <= 0xDBFF:
		d.high = r
	case r >= 0xDC00 && r <= 0xDFFF:
		d.lone(r)
	default:
		d.out = utf8.AppendRune(d.out, r)
	}
}

func (d *jsDec) flushHigh() {
	if d.high >= 0 {
		d.lone(d.high)
		d.high = -1
	}
}

// lone writes an unpaired surrogate as WTF-8 (ED A0-BF 80-BF) or U+FFFD.
//
// PORT: Go's utf8.AppendRune writes U+FFFD for surrogates, so WTF-8 is
// built by hand; in Rust the same three bytes come from an OsString/
// wtf8 helper, not from char (which cannot hold a surrogate).
func (d *jsDec) lone(r rune) {
	if d.replace {
		d.out = utf8.AppendRune(d.out, utf8.RuneError)
		return
	}
	d.out = append(d.out, 0xED, byte(0x80|(r>>6)&0x3F), byte(0x80|r&0x3F))
}

// EncodeJSString returns a string literal body for src that DecodeJSString
// (strict, same quote) decodes back to src when src is UTF-8 or WTF-8:
// backslash and the quote are escaped (quote 0 escapes both ' and "),
// \b \t \n \v \f \r for those controls, \xHH for other C0 controls and
// DEL, \u2028 and \u2029 for LS and PS, \uDXXX for WTF-8 surrogates, and
// \xHH for bytes that are not UTF-8 (which therefore do not round-trip).
func EncodeJSString(src []byte, quote byte) []byte { return AppendEncodeJSString(nil, src, quote) }

// AppendEncodeJSString appends EncodeJSString(src, quote) to dst.
func AppendEncodeJSString(dst, src []byte, quote byte) []byte {
	dst = slices.Grow(dst, len(src))
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '\\' || c == '"' && quote != '\'' || c == '\'' && quote != '"':
			dst = append(dst, '\\', c)
		case c == '\b':
			dst = append(dst, '\\', 'b')
		case c == '\t':
			dst = append(dst, '\\', 't')
		case c == '\n':
			dst = append(dst, '\\', 'n')
		case c == '\v':
			dst = append(dst, '\\', 'v')
		case c == '\f':
			dst = append(dst, '\\', 'f')
		case c == '\r':
			dst = append(dst, '\\', 'r')
		case c < 0x20 || c == 0x7F:
			dst = append(dst, '\\', 'x', hexUpper[c>>4], hexUpper[c&15])
		case c < utf8.RuneSelf:
			dst = append(dst, c)
		case c == 0xED && i+2 < len(src) && src[i+1] >= 0xA0 && src[i+1] <= 0xBF && src[i+2]&0xC0 == 0x80:
			r := 0xD000 | rune(src[i+1]&0x3F)<<6 | rune(src[i+2]&0x3F)
			dst = append(dst, '\\', 'u', hexUpper[r>>12], hexUpper[r>>8&15], hexUpper[r>>4&15], hexUpper[r&15])
			i += 2
		default:
			r, size := utf8.DecodeRune(src[i:])
			switch {
			case r == utf8.RuneError && size == 1:
				dst = append(dst, '\\', 'x', hexUpper[c>>4], hexUpper[c&15])
			case r == 0x2028 || r == 0x2029:
				dst = append(dst, '\\', 'u', '2', '0', '2', byte('0'+r-0x2020))
			default:
				dst = append(dst, src[i:i+size]...)
			}
			i += size - 1
		}
	}
	return dst
}
