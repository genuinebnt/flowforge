package content

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/genuinebnt/flowforge/jsparse"
	"github.com/genuinebnt/flowforge/sketch"
)

// LineageAlgo selects the file-level lineage matcher (pluggable decision
// "lineage", benchmark BenchmarkContentAlgo/lineage/<candidate>).
type LineageAlgo uint8

// Lineage matchers. All three estimate how much two bundles share; they
// differ in cost, stored size and error.
const (
	// LineageSimHash (default, V2 §5): 64-bit SimHash over 3-token
	// shingles, matched at Hamming distance ≤ MaxHamming (6).
	LineageSimHash LineageAlgo = iota
	// LineageMinHash: 128-permutation MinHash Jaccard estimate ≥
	// MinJaccard.
	LineageMinHash
	// LineageJaccard: exact Jaccard of the shingle sets ≥ MinJaccard (the
	// reference; needs the shingle sets of the old versions).
	LineageJaccard
)

// String returns the matcher's configuration name.
func (a LineageAlgo) String() string {
	return [...]string{"simhash", "minhash", "jaccard"}[min(int(a), 2)]
}

// ParseLineageAlgo is the inverse of String.
func ParseLineageAlgo(s string) (LineageAlgo, error) {
	for a := LineageSimHash; a <= LineageJaccard; a++ {
		if a.String() == s {
			return a, nil
		}
	}
	return 0, fmt.Errorf("%w: lineage %q", ErrUnknownAlgorithm, s)
}

// Lineage defaults.
const (
	DefaultMaxHamming = 6
	DefaultMinJaccard = 0.75
	lineageShingle    = 3
)

// Fingerprint is what lineage stores per script version.
type Fingerprint struct {
	SimHash uint64   `json:"simhash"`
	MinHash []uint64 `json:"minhash,omitempty"`
	// Shingles is the sorted set of shingle hashes (only kept for
	// LineageJaccard).
	Shingles []uint64 `json:"shingles,omitempty"`
	Size     int      `json:"size"`
}

// LineageTokens returns the token stream lineage fingerprints: for
// JavaScript, the canonical tokens with every bound identifier replaced
// by "$" (so a minifier's renaming does not move the fingerprint); for
// other text, sketch.Tokens.
func LineageTokens(kind Kind, body []byte) []string {
	if kind == KindJS {
		prog := parseJS(body, true)
		if prog.Root != jsparse.NoNode && len(prog.Tokens) > 0 {
			sp := splitter{prog: prog, bindings: bindingNames(prog)}
			return sp.canonicalTokens(0, len(prog.Source), nil, true)
		}
	}
	return sketch.Tokens(string(body))
}

// FingerprintOf computes the fingerprint the algorithm needs from a token
// stream.
func FingerprintOf(tokens []string, size int, algo LineageAlgo) Fingerprint {
	sh := sketch.Shingles(tokens, lineageShingle)
	fp := Fingerprint{Size: size}
	switch algo {
	case LineageSimHash:
		fp.SimHash = sketch.SimHash(sh)
	case LineageMinHash:
		fp.MinHash = sketch.DefaultMinHasher().Signature(sh)
	case LineageJaccard:
		hs := make([]uint64, len(sh))
		for i, s := range sh {
			hs[i] = sketch.FNV1a64(s)
		}
		slices.Sort(hs)
		fp.Shingles = slices.Compact(hs)
	}
	return fp
}

// LineageCandidate is one script the page loaded before.
type LineageCandidate struct {
	URL         string
	Fingerprint Fingerprint
}

// LineageMatch is a predecessor found for a new script URL.
type LineageMatch struct {
	URL string `json:"url"`
	// By is "url" (the lineage keys agree) or the algorithm name.
	By string `json:"by"`
	// Distance is the Hamming distance (simhash) or floor((1 − J) × 1000)
	// for the Jaccard estimate J (minhash, jaccard); lower is closer.
	Distance int `json:"distance"`
}

// LineageOptions configures FindPredecessor.
type LineageOptions struct {
	Algo       LineageAlgo
	MaxHamming int     // 0: DefaultMaxHamming
	MinJaccard float64 // 0: DefaultMinJaccard
}

// FindPredecessor finds the best predecessor of a new script among the
// page's previous scripts (V2 §5 chunk lineage): a previous script with
// the same URL wins outright (By "url", distance 0); then by lineage key
// (URL with hash-like name parts stripped); when exactly one previous
// script shares the key it is the predecessor. Otherwise (none, or
// several sharing it) the similarity matcher decides among the candidates
// (only the same-key ones when several share it), within the threshold,
// ties broken by size ratio, then URL.
//
// The scan is linear: a page loads tens of scripts, and the sketch
// package's multi-index SimHash lookup is exact only up to distance 3,
// while lineage needs 6 (sketch/OPEN_QUESTIONS.md). PORT: keep the
// linear scan; it is exact at any threshold.
func FindPredecessor(newURL string, fp Fingerprint, prev []LineageCandidate, opts LineageOptions) (LineageMatch, bool) {
	if opts.MaxHamming <= 0 {
		opts.MaxHamming = DefaultMaxHamming
	}
	if opts.MinJaccard <= 0 {
		opts.MinJaccard = DefaultMinJaccard
	}
	key := LineageKey(newURL)
	var same []LineageCandidate
	for _, c := range prev {
		if c.URL == newURL {
			return LineageMatch{URL: c.URL, By: "url", Distance: 0}, true
		}
		if LineageKey(c.URL) == key {
			same = append(same, c)
		}
	}
	if len(same) == 1 {
		return LineageMatch{URL: same[0].URL, By: "url", Distance: distance(fp, same[0].Fingerprint, opts.Algo)}, true
	}
	pool := prev
	if len(same) > 1 {
		pool = same
	}
	type scored struct {
		c     LineageCandidate
		dist  int
		ratio float64
	}
	var best []scored
	for _, c := range pool {
		d := distance(fp, c.Fingerprint, opts.Algo)
		if !within(d, opts) {
			continue
		}
		best = append(best, scored{c, d, sizeRatio(fp.Size, c.Fingerprint.Size)})
	}
	if len(best) == 0 {
		return LineageMatch{}, false
	}
	slices.SortFunc(best, func(a, b scored) int {
		return cmp.Or(cmp.Compare(a.dist, b.dist), cmp.Compare(b.ratio, a.ratio), strings.Compare(a.c.URL, b.c.URL))
	})
	return LineageMatch{URL: best[0].c.URL, By: opts.Algo.String(), Distance: best[0].dist}, true
}

// distance is the matcher's dissimilarity: Hamming bits, or
// floor((1 − J) × 1000) for the Jaccard matchers.
func distance(a, b Fingerprint, algo LineageAlgo) int {
	switch algo {
	case LineageMinHash:
		if len(a.MinHash) == 0 || len(a.MinHash) != len(b.MinHash) {
			return 1000
		}
		return int((1 - sketch.EstimateJaccard(a.MinHash, b.MinHash)) * 1000)
	case LineageJaccard:
		return int((1 - sketch.JaccardSorted(a.Shingles, b.Shingles)) * 1000)
	}
	return sketch.Hamming(a.SimHash, b.SimHash)
}

func within(d int, o LineageOptions) bool {
	if o.Algo == LineageSimHash {
		return d <= o.MaxHamming
	}
	return d <= int((1-o.MinJaccard)*1000)
}

func sizeRatio(a, b int) float64 {
	if a == 0 || b == 0 {
		return 0
	}
	return float64(min(a, b)) / float64(max(a, b))
}
