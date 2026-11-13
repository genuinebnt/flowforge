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
	ASCIIClass ASCIIClassifier
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
	// KeywordPerfectHash is a minimal-probe perfect hash: the first two
	// and last two bytes and the length, packed into a uint64, times a
	// constant, top 7 bits; every keyword has its own slot of a
	// 128-entry table, so a lookup is one multiply and at most one string
	// comparison.
	KeywordPerfectHash
)

// ASCIIClassifier is how ASCII bytes are tested for IdentifierStart /
// IdentifierPart in the lexer's identifier loop and dispatch.
type ASCIIClassifier uint8

// ASCII classification candidates.
const (
	// ASCIITable indexes a 128-entry class table (asciiIdent).
	ASCIITable ASCIIClassifier = iota
	// ASCIIBranches compares ranges: a letter after case folding with
	// c|0x20, a digit, '$' or '_'.
	ASCIIBranches
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
	switch a.Keywords {
	case KeywordMap:
		return keywordMap[s]
	case KeywordPerfectHash:
		return perfectHashKeyword(s)
	}
	return lookupKeyword(s)
}

// keywordHashMul is the multiplier of the keyword perfect hash, the first
// SplitMix64 output (seed "keyword" read as a big-endian integer,
// 0x6b6579776f7264, ORed with 1) after 990,456 others that maps all 58
// words to distinct slots; TestJSKeywordPerfectHash checks it.
const keywordHashMul = 0xde0bb5a880f98a31

// keywordSlot is a word's slot: the packed bytes s[0], s[1], s[n-2],
// s[n-1] and n, multiplied by keywordHashMul (mod 2^64), top 7 bits.
// len(s) >= 2.
func keywordSlot(s string) uint64 {
	n := len(s)
	k := uint64(s[0]) | uint64(s[1])<<8 | uint64(s[n-2])<<16 | uint64(s[n-1])<<24 | uint64(n)<<32
	return (k * keywordHashMul) >> 57
}

type keywordEntry struct {
	name string
	kw   Keyword
}

// keywordTable is the perfect hash table: slot → word (empty: none).
var keywordTable = func() (t [128]keywordEntry) {
	for _, name := range keywordNames {
		h := keywordSlot(name)
		if t[h].name != "" {
			panic("jsparse: keyword perfect hash collision: " + name + " and " + t[h].name)
		}
		t[h] = keywordEntry{name, lookupKeyword(name)}
	}
	return t
}()

// perfectHashKeyword is lookupKeyword through keywordTable.
func perfectHashKeyword(s string) Keyword {
	if len(s) < 2 || len(s) > 11 {
		return KwNone
	}
	if e := &keywordTable[keywordSlot(s)]; e.name == s {
		return e.kw
	}
	return KwNone
}

// asciiIDStart and asciiIDPart classify an ASCII byte (c < 0x80) by the
// configured candidate; both give asciiIdent's answer.
func (a Algorithms) asciiIDStart(c byte) bool {
	if a.ASCIIClass == ASCIIBranches {
		l := c | 0x20
		return (l >= 'a' && l <= 'z') || c == '$' || c == '_'
	}
	return asciiIdent[c]&1 != 0
}

func (a Algorithms) asciiIDPart(c byte) bool {
	if a.ASCIIClass == ASCIIBranches {
		l := c | 0x20
		return (l >= 'a' && l <= 'z') || (c >= '0' && c <= '9') || c == '$' || c == '_'
	}
	return asciiIdent[c]&2 != 0
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
