package sketch

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DefaultShingleWidth is the number of tokens per shingle used by
// SimHashText.
const DefaultShingleWidth = 3

// Tokens lower-cases text (Unicode simple folding to lower case) and splits
// it into maximal runs of letters and digits.
//
// PORT: strings.ToLower maps rune by rune with the simple (1:1) Unicode
// lowercase mapping and keeps invalid UTF-8 bytes as U+FFFD-free raw
// bytes only when no rune changes; Rust's str::to_lowercase uses the full
// mapping (e.g. 'İ' becomes two runes) and final-sigma rules, so port
// with char::to_lowercase taking the simple mapping. Letters and digits
// are unicode.IsLetter / unicode.IsDigit (categories L* and Nd).
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

// TokenShingles returns the k-shingles of a token-id sequence as hashes:
// element i is TokensHash(ids[i:i+k]). As with Shingles, a non-empty
// sequence shorter than k gives one shingle of all of it, no tokens give
// none, and k < 1 is treated as 1. The result keeps sequence order and
// duplicates (a multiset, the SimHash input).
func TokenShingles(ids []uint32, k int) []uint64 {
	k = max(k, 1)
	if len(ids) == 0 {
		return nil
	}
	if len(ids) <= k {
		return []uint64{TokensHash(ids)}
	}
	out := make([]uint64, 0, len(ids)-k+1)
	for i := 0; i+k <= len(ids); i++ {
		out = append(out, TokensHash(ids[i:i+k]))
	}
	return out
}

// WShingling is Broder's w-shingling of a token-id sequence: the set of its
// w-shingle hashes (TokenShingles), sorted ascending without duplicates. It
// is the MinHash and Jaccard input.
func WShingling(ids []uint32, w int) []uint64 {
	s := TokenShingles(ids, w)
	slices.Sort(s)
	return slices.Compact(s)
}

// JaccardSorted returns |A ∩ B| / |A ∪ B| of two sets given as ascending,
// duplicate-free slices (WShingling output). Two empty sets have
// similarity 1.
func JaccardSorted(a, b []uint64) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	inter, i, j := 0, 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			inter++
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}
