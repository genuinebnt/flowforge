package vecindex

import (
	"math/bits"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/genuinebnt/flowforge/core"
)

// Near-duplicate detection without embeddings (spec 22.8). Everything here
// is a pure, deterministic function of its input, so it can run inside the
// VM as a builtin and two implementations of the spec agree bit for bit.
// Strings are hashed as their UTF-8 bytes with 64-bit FNV-1a.

const (
	// NearDuplicateDistance is the SimHash Hamming distance at or below
	// which two documents are near duplicates.
	NearDuplicateDistance = 3
	// DefaultShingleWidth is the number of tokens per shingle used by
	// SimHashText.
	DefaultShingleWidth = 3

	fnvOffset64 = 14695981039346656037
	fnvPrime64  = 1099511628211
	simBlocks   = 4
)

// FNV1a64 is the 64-bit FNV-1a hash of s's bytes, the feature hash of
// SimHash and MinHash.
func FNV1a64(s string) uint64 {
	h := uint64(fnvOffset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnvPrime64
	}
	return h
}

// Tokens lower-cases text (strings.ToLower: per-rune Unicode simple case
// mapping) and splits it into maximal runs of letters and digits.
//
// PORT: "letter" is unicode.IsLetter (general category L) and "digit" is
// unicode.IsDigit (category Nd) from Go's Unicode tables. Rust's
// char::is_alphabetic (the broader Alphabetic property) and
// char::is_numeric (all of N) differ, as does str::to_lowercase (full
// mappings); use the general categories and simple mappings, and pin the
// Unicode version, or SimHash values will not agree bit for bit.
func Tokens(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// Shingles returns the contiguous windows of w tokens, each joined by a
// single space. Fewer than w (but at least one) tokens give one shingle of
// all of them; w < 1 is treated as 1.
func Shingles(tokens []string, w int) []string {
	w = max(w, 1)
	if len(tokens) == 0 {
		return nil
	}
	if len(tokens) <= w {
		return []string{strings.Join(tokens, " ")}
	}
	out := make([]string, 0, len(tokens)-w+1)
	for i := 0; i+w <= len(tokens); i++ {
		out = append(out, strings.Join(tokens[i:i+w], " "))
	}
	return out
}

// CharNGrams returns the rune n-grams of s (the character n-gram features
// of FQDN labels, spec 22.1). A non-empty s shorter than n gives one gram
// of all of s; n < 1 is treated as 1. Invalid UTF-8 bytes count as one
// rune each.
func CharNGrams(s string, n int) []string {
	n = max(n, 1)
	if s == "" {
		return nil
	}
	// Byte offsets of every rune start, plus the end.
	starts := make([]int, 0, len(s)+1)
	for i := 0; i < len(s); {
		starts = append(starts, i)
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	runes := len(starts)
	starts = append(starts, len(s))
	if runes <= n {
		return []string{s}
	}
	out := make([]string, 0, runes-n+1)
	for i := 0; i+n <= runes; i++ {
		out = append(out, s[starts[i]:starts[i+n]])
	}
	return out
}

// SimHash is the 64-bit SimHash of a multiset of features: every
// occurrence of a feature adds +1 to each bit position where its FNV-1a
// hash has a one and -1 where it has a zero; bit i of the result is set
// when position i ends strictly positive. No features give 0.
func SimHash(features []string) uint64 {
	var acc [64]int
	for _, f := range features {
		h := FNV1a64(f)
		for i := range acc {
			if h&(1<<i) != 0 {
				acc[i]++
			} else {
				acc[i]--
			}
		}
	}
	var out uint64
	for i, v := range acc {
		if v > 0 {
			out |= 1 << i
		}
	}
	return out
}

// SimHashText is SimHash over the 3-token shingles of Tokens(text).
func SimHashText(text string) uint64 {
	return SimHash(Shingles(Tokens(text), DefaultShingleWidth))
}

// Hamming returns the number of differing bits.
func Hamming(a, b uint64) int { return bits.OnesCount64(a ^ b) }

// SimHashMatch is one near-duplicate found by SimHashIndex.
type SimHashMatch struct {
	ID       core.UUID
	Distance int
}

// SimHashIndex finds near duplicates (Hamming distance <= 3) with
// multi-index hashing: the 64 bits are split into four 16-bit blocks, and
// by the pigeonhole principle any pair within distance 3 agrees exactly on
// at least one block, so a lookup probes four hash tables and verifies the
// candidates with a popcount. It is not safe for concurrent use.
type SimHashIndex struct {
	tables [simBlocks]map[uint16][]core.UUID
	hashes map[core.UUID]uint64
}

// NewSimHashIndex returns an empty index.
func NewSimHashIndex() *SimHashIndex {
	x := &SimHashIndex{hashes: make(map[core.UUID]uint64)}
	for i := range x.tables {
		x.tables[i] = make(map[uint16][]core.UUID)
	}
	return x
}

func simBlock(h uint64, i int) uint16 { return uint16(h >> (16 * i)) }

// Add records id's hash, replacing an earlier one.
func (x *SimHashIndex) Add(id core.UUID, h uint64) {
	x.Remove(id)
	x.hashes[id] = h
	for i := range x.tables {
		b := simBlock(h, i)
		x.tables[i][b] = append(x.tables[i][b], id)
	}
}

// Remove forgets id.
func (x *SimHashIndex) Remove(id core.UUID) {
	h, ok := x.hashes[id]
	if !ok {
		return
	}
	delete(x.hashes, id)
	for i := range x.tables {
		b := simBlock(h, i)
		l := slices.DeleteFunc(x.tables[i][b], func(u core.UUID) bool { return u == id })
		if len(l) == 0 {
			delete(x.tables[i], b)
		} else {
			x.tables[i][b] = l
		}
	}
}

// Len returns the number of stored hashes.
func (x *SimHashIndex) Len() int { return len(x.hashes) }

// Near returns every stored id within NearDuplicateDistance of h, sorted
// by distance then id.
func (x *SimHashIndex) Near(h uint64) []SimHashMatch {
	seen := make(map[core.UUID]struct{})
	var out []SimHashMatch
	for i := range x.tables {
		for _, id := range x.tables[i][simBlock(h, i)] {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			if d := Hamming(h, x.hashes[id]); d <= NearDuplicateDistance {
				out = append(out, SimHashMatch{ID: id, Distance: d})
			}
		}
	}
	slices.SortFunc(out, func(a, b SimHashMatch) int {
		if a.Distance != b.Distance {
			return a.Distance - b.Distance
		}
		return compareID(a.ID, b.ID)
	})
	return out
}
