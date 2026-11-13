package vecindex

import (
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/sketch"
)

// Near-duplicate detection without embeddings (spec 22.8). The algorithms
// live in package sketch (shared with content); these are thin wrappers
// that keep vecindex's API and its core.UUID ids. Everything is a pure,
// deterministic function of its input, so it can run inside the VM as a
// builtin and two implementations of the spec agree bit for bit. Strings
// are hashed as their UTF-8 bytes with 64-bit FNV-1a.

const (
	// NearDuplicateDistance is the SimHash Hamming distance at or below
	// which two documents are near duplicates.
	NearDuplicateDistance = sketch.NearDuplicateDistance
	// DefaultShingleWidth is the number of tokens per shingle used by
	// SimHashText.
	DefaultShingleWidth = sketch.DefaultShingleWidth
)

// FNV1a64 is the 64-bit FNV-1a hash of s's bytes, the feature hash of
// SimHash and MinHash (sketch.FNV1a64).
func FNV1a64(s string) uint64 { return sketch.FNV1a64(s) }

// Tokens lower-cases text (Unicode simple folding to lower case) and splits
// it into maximal runs of letters and digits (sketch.Tokens).
func Tokens(text string) []string { return sketch.Tokens(text) }

// Shingles returns the contiguous windows of w tokens, each joined by a
// single space (sketch.Shingles).
func Shingles(tokens []string, w int) []string { return sketch.Shingles(tokens, w) }

// CharNGrams returns the rune n-grams of s (sketch.CharNGrams), the
// character n-gram features of FQDN labels (spec 22.1).
func CharNGrams(s string, n int) []string { return sketch.CharNGrams(s, n) }

// SimHash is the 64-bit SimHash of a multiset of features (sketch.SimHash).
func SimHash(features []string) uint64 { return sketch.SimHash(features) }

// SimHashText is SimHash over the 3-token shingles of Tokens(text).
func SimHashText(text string) uint64 { return sketch.SimHashText(text) }

// Hamming returns the number of differing bits.
func Hamming(a, b uint64) int { return sketch.Hamming(a, b) }

// SimHashMatch is one near-duplicate found by SimHashIndex.
type SimHashMatch = sketch.Match[core.UUID]

// SimHashIndex finds near duplicates (Hamming distance <= 3) with
// multi-index hashing over four 16-bit blocks (sketch.SimHashIndex keyed
// by entity id, ties ordered bytewise). It is not safe for concurrent use.
type SimHashIndex = sketch.SimHashIndex[core.UUID]

// NewSimHashIndex returns an empty index.
func NewSimHashIndex() *SimHashIndex { return sketch.NewSimHashIndex[core.UUID](compareID) }
