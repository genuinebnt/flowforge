package sketch

import (
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
	// DefaultLSHBands is the number of LSH bands of a 128-value signature.
	DefaultLSHBands = 32
	// DefaultLSHRows is the number of signature values per band.
	DefaultLSHRows = 4
	// DefaultMinHashSeed seeds the permutation coefficients of
	// DefaultMinHasher. It is part of the conformance contract: changing it
	// changes every signature.
	DefaultMinHashSeed uint64 = 0x46464d494e484153 // "FFMINHAS"

	// Mersenne61 is the prime 2^61 - 1, the modulus of the permutations.
	Mersenne61 uint64 = 1<<61 - 1
)

// MinHasher computes MinHash signatures. Permutation i is the universal
// hash h_i(x) = (a_i * x + b_i) mod (2^61 - 1) applied to x = FNV1a64(e)
// mod (2^61 - 1), with a_i in [1, p) and b_i in [0, p) drawn, a then b for
// each i, from SplitMix64(seed) >> 3 by rejection of values outside the
// range. It is immutable and safe for concurrent use.
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
			if v := rng.Uint64() >> 3; v >= lo && v < Mersenne61 {
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

// Coefficients returns copies of the permutation coefficients a and b.
func (m *MinHasher) Coefficients() (a, b []uint64) { return slices.Clone(m.a), slices.Clone(m.b) }

// MulAddMod61 returns (a*x + b) mod 2^61-1 for a, x, b < 2^61-1, folding
// the 122-bit product with the Mersenne identity 2^61 ≡ 1. PORT: bits.Mul64
// is a full 64x64→128 multiply (u128 in Rust).
func MulAddMod61(a, x, b uint64) uint64 {
	hi, lo := bits.Mul64(a, x)
	// a*x < 2^122 = hi*2^64 + lo; fold the bits above 61 back in.
	r := (hi<<3 | lo>>61) + (lo & Mersenne61)
	r = (r & Mersenne61) + (r >> 61)
	if r >= Mersenne61 {
		r -= Mersenne61
	}
	r += b
	if r >= Mersenne61 {
		r -= Mersenne61
	}
	return r
}

// empty returns the signature of the empty set: every value MaxUint64,
// above any MulAddMod61 result.
func (m *MinHasher) empty() []uint64 {
	sig := make([]uint64, len(m.a))
	for i := range sig {
		sig[i] = math.MaxUint64
	}
	return sig
}

// update lowers each signature value to permutation i of h where smaller.
func (m *MinHasher) update(sig []uint64, h uint64) {
	x := h % Mersenne61
	for i := range sig {
		if v := MulAddMod61(m.a[i], x, m.b[i]); v < sig[i] {
			sig[i] = v
		}
	}
}

// Signature returns the MinHash signature of the set of elements
// (duplicates and order do not matter). The empty set's signature is all
// math.MaxUint64, which no element can produce.
func (m *MinHasher) Signature(elements []string) []uint64 {
	sig := m.empty()
	for _, e := range elements {
		m.update(sig, FNV1a64(e))
	}
	return sig
}

// SignatureHashes is Signature over elements given as their 64-bit hashes
// (for example WShingling output): Signature(e) ==
// SignatureHashes(FNV1a64(e) for each e).
func (m *MinHasher) SignatureHashes(hashes []uint64) []uint64 {
	sig := m.empty()
	for _, h := range hashes {
		m.update(sig, h)
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

// BandKey is the LSH key of band b of sig: FNV-1a over the little-endian
// bytes of rows [b*rows, (b+1)*rows). The caller guarantees the range is in
// bounds.
func BandKey(sig []uint64, b, rows int) uint64 {
	h := fnvOffset64
	for _, v := range sig[b*rows : (b+1)*rows] {
		h = fnvUint64(h, v)
	}
	return h
}

// LSH is a banded locality-sensitive hash index over MinHash signatures:
// two signatures are candidates when they agree on every row of at least
// one band (BandKey equality). Ids are ordered by the comparator given to
// NewLSH. It is not safe for concurrent use. Map iteration order never
// reaches an output: Candidates sorts.
type LSH[ID comparable] struct {
	bands, rows int
	cmp         func(a, b ID) int
	tables      []map[uint64][]ID
	sigs        map[ID][]uint64
}

// NewLSH returns an empty index for signatures of bands*rows values; cmp
// orders Candidates' result and must be a total order.
func NewLSH[ID comparable](bands, rows int, cmp func(a, b ID) int) (*LSH[ID], error) {
	if bands < 1 || rows < 1 || cmp == nil {
		return nil, fmt.Errorf("%w: lsh bands=%d rows=%d", ErrInvalidConfig, bands, rows)
	}
	l := &LSH[ID]{bands: bands, rows: rows, cmp: cmp, tables: make([]map[uint64][]ID, bands), sigs: make(map[ID][]uint64)}
	for i := range l.tables {
		l.tables[i] = make(map[uint64][]ID)
	}
	return l, nil
}

// Bands returns the number of bands.
func (l *LSH[ID]) Bands() int { return l.bands }

// Rows returns the number of signature values per band.
func (l *LSH[ID]) Rows() int { return l.rows }

func (l *LSH[ID]) checkSig(sig []uint64) error {
	if len(sig) != l.bands*l.rows {
		return fmt.Errorf("%w: signature length %d, want %d", ErrSignatureLength, len(sig), l.bands*l.rows)
	}
	return nil
}

// BandKeys returns the key of every band of sig.
func (l *LSH[ID]) BandKeys(sig []uint64) ([]uint64, error) {
	if err := l.checkSig(sig); err != nil {
		return nil, err
	}
	out := make([]uint64, l.bands)
	for b := range out {
		out[b] = BandKey(sig, b, l.rows)
	}
	return out, nil
}

// Add records id's signature, replacing an earlier one.
func (l *LSH[ID]) Add(id ID, sig []uint64) error {
	if err := l.checkSig(sig); err != nil {
		return err
	}
	l.Remove(id)
	l.sigs[id] = slices.Clone(sig)
	for b := range l.bands {
		k := BandKey(sig, b, l.rows)
		l.tables[b][k] = append(l.tables[b][k], id)
	}
	return nil
}

// Remove forgets id.
func (l *LSH[ID]) Remove(id ID) {
	sig, ok := l.sigs[id]
	if !ok {
		return
	}
	delete(l.sigs, id)
	for b := range l.bands {
		k := BandKey(sig, b, l.rows)
		rest := slices.DeleteFunc(l.tables[b][k], func(u ID) bool { return u == id })
		if len(rest) == 0 {
			delete(l.tables[b], k)
		} else {
			l.tables[b][k] = rest
		}
	}
}

// Len returns the number of stored signatures.
func (l *LSH[ID]) Len() int { return len(l.sigs) }

// Candidates returns the ids sharing at least one band with sig, sorted.
func (l *LSH[ID]) Candidates(sig []uint64) ([]ID, error) {
	if err := l.checkSig(sig); err != nil {
		return nil, err
	}
	seen := make(map[ID]struct{})
	var out []ID
	for b := range l.bands {
		for _, id := range l.tables[b][BandKey(sig, b, l.rows)] {
			if _, dup := seen[id]; !dup {
				seen[id] = struct{}{}
				out = append(out, id)
			}
		}
	}
	slices.SortFunc(out, l.cmp)
	return out, nil
}
