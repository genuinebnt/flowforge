package vecindex

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"slices"

	"github.com/genuinebnt/flowforge/core"
)

// MinHash and LSH parameters (spec 22.8).
const (
	// DefaultMinHashPermutations is the signature length.
	DefaultMinHashPermutations = 128
	// DefaultLSHBands and DefaultLSHRows split a 128-value signature into
	// 32 bands of 4 rows.
	DefaultLSHBands = 32
	DefaultLSHRows  = 4
	// DefaultMinHashSeed seeds the permutation coefficients of
	// DefaultMinHasher. It is part of the conformance contract: changing it
	// changes every signature.
	DefaultMinHashSeed uint64 = 0x46464d494e484153 // "FFMINHAS"

	mersenne61 = 1<<61 - 1
)

// MinHasher computes MinHash signatures. Permutation i is the universal
// hash h_i(x) = (a_i * x + b_i) mod (2^61 - 1) applied to x = FNV1a64(e)
// mod (2^61 - 1), with a_i in [1, p) and b_i in [0, p) drawn, a then b for
// each i, from SplitMix64(seed) by rejection of values >= p. It is
// immutable and safe for concurrent use.
type MinHasher struct {
	a, b []uint64
}

// NewMinHasher returns a hasher with n permutations derived from seed.
func NewMinHasher(n int, seed uint64) (*MinHasher, error) {
	if n < 1 {
		return nil, fmt.Errorf("%w: %d minhash permutations", ErrInvalidConfig, n)
	}
	rng := core.NewSplitMix64(seed)
	draw := func(lo uint64) uint64 {
		for {
			if v := rng.Uint64() >> 3; v >= lo && v < mersenne61 {
				return v
			}
		}
	}
	m := &MinHasher{a: make([]uint64, n), b: make([]uint64, n)}
	for i := range n {
		m.a[i] = draw(1)
		m.b[i] = draw(0)
	}
	return m, nil
}

// DefaultMinHasher returns the 128-permutation hasher with
// DefaultMinHashSeed.
func DefaultMinHasher() *MinHasher {
	m, err := NewMinHasher(DefaultMinHashPermutations, DefaultMinHashSeed)
	if err != nil {
		panic(err) // constant arguments: impossible
	}
	return m
}

// Permutations returns the signature length.
func (m *MinHasher) Permutations() int { return len(m.a) }

// mulAddMod61 returns (a*x + b) mod 2^61-1 for a, x, b < 2^61-1.
func mulAddMod61(a, x, b uint64) uint64 {
	hi, lo := bits.Mul64(a, x)
	// a*x < 2^122 = hi*2^64 + lo; fold the bits above 61 back in.
	r := (hi<<3 | lo>>61) + (lo & mersenne61)
	r = (r & mersenne61) + (r >> 61)
	if r >= mersenne61 {
		r -= mersenne61
	}
	r += b
	if r >= mersenne61 {
		r -= mersenne61
	}
	return r
}

// Signature returns the MinHash signature of the set of elements
// (duplicates do not matter). The empty set's signature is all
// math.MaxUint64, which no element can produce.
func (m *MinHasher) Signature(elements []string) []uint64 {
	sig := make([]uint64, len(m.a))
	for i := range sig {
		sig[i] = math.MaxUint64
	}
	for _, e := range elements {
		x := FNV1a64(e) % mersenne61
		for i := range sig {
			if h := mulAddMod61(m.a[i], x, m.b[i]); h < sig[i] {
				sig[i] = h
			}
		}
	}
	return sig
}

// EstimateJaccard returns the fraction of positions where two signatures
// agree, an unbiased estimate of the Jaccard similarity of their sets. It
// returns 0 for signatures of different lengths or length zero.
func EstimateJaccard(a, b []uint64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	eq := 0
	for i := range a {
		if a[i] == b[i] {
			eq++
		}
	}
	return float64(eq) / float64(len(a))
}

// Jaccard returns |A ∩ B| / |A ∪ B| of two sets given as element lists;
// two empty sets have similarity 1.
func Jaccard(a, b []string) float64 {
	sa := make(map[string]struct{}, len(a))
	for _, x := range a {
		sa[x] = struct{}{}
	}
	sb := make(map[string]struct{}, len(b))
	for _, x := range b {
		sb[x] = struct{}{}
	}
	if len(sa) == 0 && len(sb) == 0 {
		return 1
	}
	inter := 0
	for x := range sa {
		if _, ok := sb[x]; ok {
			inter++
		}
	}
	return float64(inter) / float64(len(sa)+len(sb)-inter)
}

// CollisionProbability is the probability that two sets with Jaccard
// similarity s share at least one LSH band: 1 - (1 - s^rows)^bands (spec
// 22.8 with 32 bands of 4 rows).
func CollisionProbability(s float64, bands, rows int) float64 {
	return 1 - math.Pow(1-math.Pow(s, float64(rows)), float64(bands))
}

// LSH is a banded locality-sensitive hash index over MinHash signatures:
// two signatures are candidates when they agree on every row of at least
// one band. A band's key is FNV-1a over the little-endian bytes of its
// rows. It is not safe for concurrent use.
type LSH struct {
	bands, rows int
	tables      []map[uint64][]core.UUID
	sigs        map[core.UUID][]uint64
}

// NewLSH returns an empty index for signatures of bands*rows values.
func NewLSH(bands, rows int) (*LSH, error) {
	if bands < 1 || rows < 1 {
		return nil, fmt.Errorf("%w: lsh bands=%d rows=%d", ErrInvalidConfig, bands, rows)
	}
	l := &LSH{bands: bands, rows: rows, tables: make([]map[uint64][]core.UUID, bands), sigs: make(map[core.UUID][]uint64)}
	for i := range l.tables {
		l.tables[i] = make(map[uint64][]core.UUID)
	}
	return l, nil
}

// bandKey hashes rows [b*rows, (b+1)*rows) of sig.
func (l *LSH) bandKey(sig []uint64, b int) uint64 {
	h := uint64(fnvOffset64)
	var buf [8]byte
	for _, v := range sig[b*l.rows : (b+1)*l.rows] {
		binary.LittleEndian.PutUint64(buf[:], v)
		for _, c := range buf {
			h ^= uint64(c)
			h *= fnvPrime64
		}
	}
	return h
}

func (l *LSH) checkSig(sig []uint64) error {
	if len(sig) != l.bands*l.rows {
		return fmt.Errorf("%w: signature length %d, want %d", ErrDimension, len(sig), l.bands*l.rows)
	}
	return nil
}

// Add records id's signature, replacing an earlier one.
func (l *LSH) Add(id core.UUID, sig []uint64) error {
	if err := l.checkSig(sig); err != nil {
		return err
	}
	l.Remove(id)
	l.sigs[id] = slices.Clone(sig)
	for b := range l.bands {
		k := l.bandKey(sig, b)
		l.tables[b][k] = append(l.tables[b][k], id)
	}
	return nil
}

// Remove forgets id.
func (l *LSH) Remove(id core.UUID) {
	sig, ok := l.sigs[id]
	if !ok {
		return
	}
	delete(l.sigs, id)
	for b := range l.bands {
		k := l.bandKey(sig, b)
		rest := slices.DeleteFunc(l.tables[b][k], func(u core.UUID) bool { return u == id })
		if len(rest) == 0 {
			delete(l.tables[b], k)
		} else {
			l.tables[b][k] = rest
		}
	}
}

// Candidates returns the ids sharing at least one band with sig, sorted.
func (l *LSH) Candidates(sig []uint64) ([]core.UUID, error) {
	if err := l.checkSig(sig); err != nil {
		return nil, err
	}
	seen := make(map[core.UUID]struct{})
	var out []core.UUID
	for b := range l.bands {
		for _, id := range l.tables[b][l.bandKey(sig, b)] {
			if _, dup := seen[id]; !dup {
				seen[id] = struct{}{}
				out = append(out, id)
			}
		}
	}
	slices.SortFunc(out, compareID)
	return out, nil
}
