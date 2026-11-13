package vecindex

import (
	"errors"
	"fmt"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/sketch"
)

// MinHash and LSH parameters (spec 22.8), re-exported from sketch.
const (
	// DefaultMinHashPermutations is the signature length.
	DefaultMinHashPermutations = sketch.DefaultMinHashPermutations
	// DefaultLSHBands is the number of LSH bands of a 128-value signature.
	DefaultLSHBands = sketch.DefaultLSHBands
	// DefaultLSHRows is the number of signature values per band.
	DefaultLSHRows = sketch.DefaultLSHRows
	// DefaultMinHashSeed seeds the permutation coefficients of
	// DefaultMinHasher. It is part of the conformance contract.
	DefaultMinHashSeed = sketch.DefaultMinHashSeed
)

// MinHasher computes MinHash signatures (sketch.MinHasher): permutation i
// is (a_i * x + b_i) mod (2^61 - 1) over x = FNV1a64(e) mod (2^61 - 1).
type MinHasher = sketch.MinHasher

// NewMinHasher returns a hasher with n permutations derived from seed.
// Errors wrap ErrInvalidConfig.
func NewMinHasher(n int, seed uint64) (*MinHasher, error) {
	m, err := sketch.NewMinHasher(n, seed)
	if err != nil {
		return nil, mapSketchErr(err)
	}
	return m, nil
}

// DefaultMinHasher returns the 128-permutation hasher with
// DefaultMinHashSeed.
func DefaultMinHasher() *MinHasher { return sketch.DefaultMinHasher() }

// EstimateJaccard returns the fraction of positions where two signatures
// agree (sketch.EstimateJaccard).
func EstimateJaccard(a, b []uint64) float64 { return sketch.EstimateJaccard(a, b) }

// Jaccard returns |A ∩ B| / |A ∪ B| of two sets given as element lists;
// two empty sets have similarity 1.
func Jaccard(a, b []string) float64 { return sketch.Jaccard(a, b) }

// CollisionProbability is 1 - (1 - s^rows)^bands, the probability that two
// sets with Jaccard similarity s share an LSH band.
func CollisionProbability(s float64, bands, rows int) float64 {
	return sketch.CollisionProbability(s, bands, rows)
}

// LSH is a banded locality-sensitive hash index over MinHash signatures
// keyed by entity id (sketch.LSH). Errors wrap vecindex's sentinels. It is
// not safe for concurrent use.
type LSH struct {
	l *sketch.LSH[core.UUID]
}

// NewLSH returns an empty index for signatures of bands*rows values.
func NewLSH(bands, rows int) (*LSH, error) {
	l, err := sketch.NewLSH[core.UUID](bands, rows, compareID)
	if err != nil {
		return nil, mapSketchErr(err)
	}
	return &LSH{l: l}, nil
}

// Add records id's signature, replacing an earlier one. A signature of the
// wrong length fails with ErrDimension.
func (l *LSH) Add(id core.UUID, sig []uint64) error { return mapSketchErr(l.l.Add(id, sig)) }

// Remove forgets id.
func (l *LSH) Remove(id core.UUID) { l.l.Remove(id) }

// Candidates returns the ids sharing at least one band with sig, sorted.
func (l *LSH) Candidates(sig []uint64) ([]core.UUID, error) {
	c, err := l.l.Candidates(sig)
	return c, mapSketchErr(err)
}

// mapSketchErr rewraps sketch's sentinels as vecindex's.
func mapSketchErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sketch.ErrSignatureLength):
		return fmt.Errorf("%w: %w", ErrDimension, err)
	case errors.Is(err, sketch.ErrInvalidConfig):
		return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	default:
		return err
	}
}
