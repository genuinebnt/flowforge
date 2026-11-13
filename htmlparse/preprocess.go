package htmlparse

import "unicode/utf8"

// This file implements "preprocessing the input stream" (HTML §13.2.3.5)
// for a UTF-8 input buffer: newline normalization and the
// surrogate / noncharacter / control-character parse errors. The
// tokenizer does not copy the input; it reads code points through
// Tokenizer.next, which applies these rules on the fly so token spans
// stay byte offsets into the original buffer.
//
// Decoding rules for the UTF-8 buffer:
//   - CR LF is one LF; a lone CR is LF.
//   - A well-formed UTF-8 sequence is its code point.
//   - A three-byte "generalized UTF-8" sequence for a surrogate (ED A0..BF
//     80..BF, as produced by WTF-8/CESU-8 encoders) is reported as
//     surrogate-in-input-stream and read as U+FFFD, so the tokenizer
//     always produces valid UTF-8.
//   - Any other ill-formed sequence is U+FFFD, consuming the maximal
//     subpart as the WHATWG UTF-8 decoder does (Encoding §4.1).

// decodeRune decodes the code point at the start of b (len(b) > 0) under
// the rules above, except that surrogates are returned as their code point
// (the caller reports and replaces them).
//
// PORT: Go strings may hold any bytes; this parser only ever stores valid
// UTF-8 (ill-formed input and surrogates become U+FFFD), so every string
// maps to a Rust String. The input itself stays &[u8].
//
// PORT: utf8.DecodeRune returns (RuneError, 1) for every ill-formed
// sequence, whatever its length, and (RuneError, 3) for a real U+FFFD
// (EF BF BD); hence the w == 3 test, the explicit surrogate check (Go and
// Rust both reject ED A0..BF as UTF-8) and maximalSubpart for the width.
// A Rust port decoding with str::from_utf8's error_len gets the same
// maximal-subpart widths but must keep the surrogate case.
func decodeRune(b []byte) (rune, int) {
	c := b[0]
	if c < utf8.RuneSelf {
		return rune(c), 1
	}
	r, w := utf8.DecodeRune(b)
	if r != utf8.RuneError || w == 3 {
		return r, w
	}
	if len(b) >= 3 && c == 0xED && b[1] >= 0xA0 && b[1] <= 0xBF && b[2] >= 0x80 && b[2] <= 0xBF {
		return 0xD000 | rune(b[1]&0x3F)<<6 | rune(b[2]&0x3F), 3
	}
	return utf8.RuneError, maximalSubpart(b)
}

// maximalSubpart returns the length of the maximal subpart of an
// ill-formed UTF-8 sequence starting at b[0] (at least 1): the longest
// prefix that could still start a well-formed sequence (Unicode §3.9
// "U+FFFD substitution of maximal subparts", the WHATWG decoder's
// behaviour). lo/hi narrow the first continuation byte exactly where
// Table 3-7 does (E0, ED, F0, F4).
func maximalSubpart(b []byte) int {
	c := b[0]
	var need int
	lo, hi := byte(0x80), byte(0xBF)
	switch {
	case c >= 0xC2 && c <= 0xDF:
		need = 1
	case c == 0xE0:
		need, lo = 2, 0xA0
	case c == 0xED:
		need, hi = 2, 0x9F
	case c >= 0xE1 && c <= 0xEF:
		need = 2
	case c == 0xF0:
		need, lo = 3, 0x90
	case c == 0xF4:
		need, hi = 3, 0x8F
	case c >= 0xF1 && c <= 0xF3:
		need = 3
	default:
		return 1
	}
	n := 1
	for i := 0; i < need; i++ {
		if n >= len(b) || b[n] < lo || b[n] > hi {
			return n
		}
		lo, hi = 0x80, 0xBF
		n++
	}
	return n // well-formed; not reached for ill-formed input
}

// isNoncharacter reports Infra "noncharacter": U+FDD0..U+FDEF and the
// last two code points of every plane (U+xxFFFE, U+xxFFFF).
func isNoncharacter(r rune) bool {
	return (r >= 0xFDD0 && r <= 0xFDEF) || (r&0xFFFE == 0xFFFE && r <= 0x10FFFF)
}

// isSurrogate reports Infra "surrogate".
func isSurrogate(r rune) bool { return r >= 0xD800 && r <= 0xDFFF }

// isControlError reports a control (C0 or U+007F..U+009F) other than
// ASCII whitespace and NULL: the control-character-in-input-stream and
// control-character-reference condition. CR is ASCII whitespace and
// returns false; the input stream normalizes it to LF first, and the
// numeric character reference end state tests 0x0D on its own (the spec
// lists it explicitly there).
func isControlError(r rune) bool {
	switch {
	case r == 0 || r == '\t' || r == '\n' || r == '\f' || r == '\r':
		return false
	case r < 0x20:
		return true
	case r >= 0x7F && r <= 0x9F:
		return true
	}
	return false
}

// asciiPlain marks bytes that can be copied straight into character data
// in the text states: printable ASCII plus TAB, LF, FF. It excludes CR
// (newline normalization), NUL, other controls (input-stream errors),
// '<' and '&' (state changes). The text fast paths test this table first.
//
// PORT: a constant 256-entry table (a const array in Rust).
var asciiPlain = func() (t [256]bool) {
	for c := 0x20; c < 0x7F; c++ {
		t[c] = true
	}
	t['\t'], t['\n'], t['\f'] = true, true, true
	t['<'], t['&'] = false, false
	return t
}()

func isASCIIAlpha(c rune) bool { return (c|0x20) >= 'a' && (c|0x20) <= 'z' }
func isASCIIUpper(c rune) bool { return c >= 'A' && c <= 'Z' }
func isASCIIDigit(c rune) bool { return c >= '0' && c <= '9' }
func isASCIIAlnum(c rune) bool { return isASCIIAlpha(c) || isASCIIDigit(c) }
func isASCIIHex(c rune) bool   { return isASCIIDigit(c) || ((c|0x20) >= 'a' && (c|0x20) <= 'f') }

// isHTMLSpace is ASCII whitespace as the tokenizer sees it: CR never
// reaches the tokenizer states (next normalizes it), so it is omitted.
func isHTMLSpace(c rune) bool { return c == '\t' || c == '\n' || c == '\f' || c == ' ' }

// isHTMLSpaceB is Infra "ASCII whitespace" for raw bytes and attribute
// values, which are not newline-normalized, so CR is included (the
// encoding prescan, labels, the non-negative integer parser).
func isHTMLSpaceB(c byte) bool  { return c == '\t' || c == '\n' || c == '\f' || c == ' ' || c == '\r' }
func isASCIIAlnumB(c byte) bool { return isASCIIAlnum(rune(c)) }

// lowerASCII returns s with ASCII upper-case letters lowered.
func lowerASCII(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'A' && b[j] <= 'Z' {
					b[j] += 0x20
				}
			}
			return string(b)
		}
	}
	return s
}

// equalFoldASCII is ASCII case-insensitive equality.
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if x >= 'A' && x <= 'Z' {
			x += 0x20
		}
		if y >= 'A' && y <= 'Z' {
			y += 0x20
		}
		if x != y {
			return false
		}
	}
	return true
}

// hasPrefixFoldASCII reports whether s starts with prefix, ASCII
// case-insensitively.
func hasPrefixFoldASCII(s, prefix string) bool {
	return len(s) >= len(prefix) && equalFoldASCII(s[:len(prefix)], prefix)
}
