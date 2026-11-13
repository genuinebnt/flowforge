package jsparse

import "unicode"

// Code point classes of ECMA-262 12.2 (white space), 12.3 (line
// terminators) and 12.7 (identifier names). ID_Start and ID_Continue are
// derived from the Go unicode tables exactly as UAX #31 defines them:
//
//	ID_Start    = L + Nl + Other_ID_Start - Pattern_Syntax - Pattern_White_Space
//	ID_Continue = ID_Start + Mn + Mc + Nd + Pc + Other_ID_Continue - Pattern_Syntax - Pattern_White_Space
//
// The Rust port uses the unicode-id-start crate, which encodes the same
// derivation; both follow the Unicode version of their standard library.

const (
	cpLS   = 0x2028 // LINE SEPARATOR
	cpPS   = 0x2029 // PARAGRAPH SEPARATOR
	cpZWNJ = 0x200C
	cpZWJ  = 0x200D
	cpBOM  = 0xFEFF // ZWNBSP
	cpNBSP = 0x00A0
)

// asciiIdent classifies ASCII bytes: bit 0 = IdentifierStart, bit 1 =
// IdentifierPart.
var asciiIdent = func() (t [128]uint8) {
	for c := 'a'; c <= 'z'; c++ {
		t[c] = 3
	}
	for c := 'A'; c <= 'Z'; c++ {
		t[c] = 3
	}
	for c := '0'; c <= '9'; c++ {
		t[c] = 2
	}
	t['$'] = 3
	t['_'] = 3
	return t
}()

func isPatternExcluded(r rune) bool {
	return unicode.Is(unicode.Pattern_Syntax, r) || unicode.Is(unicode.Pattern_White_Space, r)
}

// isIDStart reports whether r may start an IdentifierName
// (IdentifierStartChar :: UnicodeIDStart | $ | _).
func isIDStart(r rune) bool {
	if r < 128 {
		return r >= 0 && asciiIdent[r]&1 != 0
	}
	if unicode.IsLetter(r) || unicode.Is(unicode.Nl, r) || unicode.Is(unicode.Other_ID_Start, r) {
		return !isPatternExcluded(r)
	}
	return false
}

// isIDContinue reports whether r may continue an IdentifierName
// (IdentifierPartChar :: UnicodeIDContinue | $ | ZWNJ | ZWJ).
func isIDContinue(r rune) bool {
	if r < 128 {
		return r >= 0 && asciiIdent[r]&2 != 0
	}
	if r == cpZWNJ || r == cpZWJ {
		return true
	}
	if isIDStart(r) {
		return true
	}
	if unicode.In(r, unicode.Mn, unicode.Mc, unicode.Nd, unicode.Pc, unicode.Other_ID_Continue) {
		return !isPatternExcluded(r)
	}
	return false
}

// isWhiteSpace reports whether r is WhiteSpace (12.2): TAB, VT, FF,
// ZWNBSP or any Space_Separator (USP).
func isWhiteSpace(r rune) bool {
	switch r {
	case '\t', '\v', '\f', ' ', cpNBSP, cpBOM:
		return true
	}
	return r > 127 && unicode.Is(unicode.Zs, r)
}

// isLineTerminator reports whether r is a LineTerminator (12.3).
func isLineTerminator(r rune) bool {
	return r == '\n' || r == '\r' || r == cpLS || r == cpPS
}

func isDecimalDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}
