package content

import (
	"fmt"

	"github.com/genuinebnt/flowforge/diff"
)

// DiffPolicy selects how an artifact's normalized text is diffed
// (pluggable decision "diff", benchmark BenchmarkContentAlgo/diff/
// <candidate>). Every policy goes through diff.BoundedDiff, so the §5
// bounds (edit cap, size caps, line then hash fallback) always hold.
type DiffPolicy uint8

// Diff policies.
const (
	// DiffBySize (default): MyersLinearSpace over word tokens for inputs
	// up to LargeInputBytes (minimal scripts on small artifacts), and
	// Histogram over lines above it (docs/ALGORITHMS.md: about 14× faster
	// than Myers on large snapshots, git-compatible, readable hunks).
	DiffBySize DiffPolicy = iota
	// DiffMyers: MyersLinearSpace tokens up to the §5 4 MiB cap.
	DiffMyers
	// DiffHistogram: Histogram over lines always.
	DiffHistogram
	// DiffPatience: Patience over lines always.
	DiffPatience
)

// LargeInputBytes is the DiffBySize threshold: 256 KiB of normalized
// text.
const LargeInputBytes = 256 << 10

// String returns the policy's configuration name.
func (p DiffPolicy) String() string {
	return [...]string{"size", "myers", "histogram", "patience"}[min(int(p), 3)]
}

// ParseDiffPolicy is the inverse of String.
func ParseDiffPolicy(s string) (DiffPolicy, error) {
	for p := DiffBySize; p <= DiffPatience; p++ {
		if p.String() == s {
			return p, nil
		}
	}
	return 0, fmt.Errorf("%w: diff policy %q", ErrUnknownAlgorithm, s)
}

// Bounds returns the diff.Bounds of the policy for inputs of the given
// size (the larger side). base carries the caller's edit and size caps.
func (p DiffPolicy) Bounds(size int, base diff.Bounds) diff.Bounds {
	b := base
	switch p {
	case DiffBySize:
		b.TokenAlgorithm, b.LineAlgorithm = diff.MyersLinearSpace, diff.Histogram
		if size > LargeInputBytes {
			// Skip the token level: the input is over this policy's token
			// bound, so BoundedDiff goes straight to histogram lines.
			b.MaxInputBytes = LargeInputBytes
		}
	case DiffMyers:
		b.TokenAlgorithm, b.LineAlgorithm = diff.MyersLinearSpace, diff.Histogram
	case DiffHistogram:
		b.TokenAlgorithm, b.LineAlgorithm, b.MaxInputBytes = diff.Histogram, diff.Histogram, 1
	case DiffPatience:
		b.TokenAlgorithm, b.LineAlgorithm, b.MaxInputBytes = diff.Patience, diff.Patience, 1
	}
	return b
}
