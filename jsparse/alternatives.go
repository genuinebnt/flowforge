package jsparse

import (
	"sync"
	"unicode/utf8"
)

// Algorithms selects among implementations that were measured against
// each other (README "Design choices measured"; docs/CONVENTIONS.md
// "Pluggable algorithms"). The zero value is the default, fastest
// configuration. Every candidate produces identical trees and
// diagnostics (TestJSAlgoEquivalent).
type Algorithms struct {
	Keywords   KeywordLookup
	IdentClass IdentClassifier
	Binary     BinaryStrategy
}

// KeywordLookup is how the lexer maps an IdentifierName to a Keyword.
type KeywordLookup uint8

// Keyword lookup candidates.
const (
	// KeywordSwitch is a length-guarded string switch (lookupKeyword),
	// which Go compiles to a binary search over the case strings.
	KeywordSwitch KeywordLookup = iota
	// KeywordMap is a hash-map lookup.
	KeywordMap
)

// IdentClassifier is how non-ASCII code points are tested for ID_Start /
// ID_Continue (ASCII always uses a 128-entry table).
type IdentClassifier uint8

// Identifier classification candidates.
const (
	// IdentUnicodeTables searches Go's unicode range tables.
	IdentUnicodeTables IdentClassifier = iota
	// IdentBMPBitmap indexes two precomputed 8 KiB bitmaps for the BMP
	// (built on first use) and falls back to the tables above U+FFFF.
	IdentBMPBitmap
)

// BinaryStrategy is how binary-operator expressions are parsed.
type BinaryStrategy uint8

// Binary expression parsing candidates.
const (
	// BinaryClimbing is precedence climbing: one loop over a precedence
	// table (parseBinary).
	BinaryClimbing BinaryStrategy = iota
	// BinaryCascade is classic recursive descent with one function call
	// per precedence level (ShortCircuitExpression → ... →
	// ExponentiationExpression), mirroring the grammar's productions.
	BinaryCascade
)

var keywordMap = func() map[string]Keyword {
	m := make(map[string]Keyword, len(keywordNames))
	for _, name := range keywordNames {
		m[name] = lookupKeyword(name)
	}
	return m
}()

// keywordNames lists every word lookupKeyword recognises.
var keywordNames = []string{
	"await", "break", "case", "catch", "class", "const", "continue", "debugger", "default", "delete", "do",
	"else", "enum", "export", "extends", "false", "finally", "for", "function", "if", "import", "in",
	"instanceof", "new", "null", "return", "super", "switch", "this", "throw", "true", "try", "typeof",
	"var", "void", "while", "with", "yield", "implements", "interface", "let", "package", "private",
	"protected", "public", "static", "as", "async", "from", "get", "meta", "of", "set", "target", "eval",
	"arguments", "constructor", "__proto__",
}

func (a Algorithms) keyword(s string) Keyword {
	if a.Keywords == KeywordMap {
		return keywordMap[s]
	}
	return lookupKeyword(s)
}

// idBitmaps holds ID_Start (index 0) and ID_Continue (index 1) for the
// BMP, one bit per code point, derived from isIDStart / isIDContinue.
var idBitmaps = sync.OnceValue(func() *[2][1 << 10]uint64 {
	var t [2][1 << 10]uint64
	for r := rune(0); r < 1<<16; r++ {
		if isIDStart(r) {
			t[0][r>>6] |= 1 << (r & 63)
		}
		if isIDContinue(r) {
			t[1][r>>6] |= 1 << (r & 63)
		}
	}
	return &t
})

func (a Algorithms) idStart(r rune) bool {
	if a.IdentClass == IdentBMPBitmap && r >= utf8.RuneSelf && r < 1<<16 {
		return idBitmaps()[0][r>>6]&(1<<(r&63)) != 0
	}
	return isIDStart(r)
}

func (a Algorithms) idContinue(r rune) bool {
	if a.IdentClass == IdentBMPBitmap && r >= utf8.RuneSelf && r < 1<<16 {
		return idBitmaps()[1][r>>6]&(1<<(r&63)) != 0
	}
	return isIDContinue(r)
}
