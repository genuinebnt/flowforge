package core

import (
	crand "crypto/rand"
	"encoding/binary"
)

// Rng is the randomness port (spec 24.4). Implementations need not be safe
// for concurrent use; callers that share one must synchronize.
type Rng interface {
	// Uint64 returns the next 64 uniformly distributed bits.
	Uint64() uint64
}

// SystemRng draws from the operating system's cryptographic source. It is
// stateless and safe for concurrent use.
type SystemRng struct{}

// Uint64 implements Rng.
func (SystemRng) Uint64() uint64 {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		// crypto/rand.Read is documented never to fail on supported platforms.
		panic("core: crypto/rand failed: " + err.Error())
	}
	return binary.LittleEndian.Uint64(b[:])
}

// SplitMix64 is a small, fast, deterministic Rng (Steele, Lea and Flood,
// "Fast splittable pseudorandom number generators", 2014). It is the
// reference seeded generator used by tests, simulations and anything that
// must reproduce a sequence from a seed (docs/PORTING_RUST.md 4.3). Not
// safe for concurrent use; not cryptographically secure.
type SplitMix64 struct{ state uint64 }

// NewSplitMix64 returns a generator seeded with seed.
func NewSplitMix64(seed uint64) *SplitMix64 { return &SplitMix64{state: seed} }

// Uint64 implements Rng.
//
// PORT: the additions and multiplications wrap modulo 2^64 (Go unsigned
// arithmetic); use wrapping_add/wrapping_mul in Rust or debug builds panic.
func (s *SplitMix64) Uint64() uint64 {
	s.state += 0x9e3779b97f4a7c15
	z := s.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// Float64 returns a uniformly distributed value in [0, 1) from r, built
// from the top 53 bits of one Uint64 so every result is exact in float64.
// Seeded sequences depend on this exact formula (docs/PORTING_RUST.md 4.3).
func Float64(r Rng) float64 { return float64(r.Uint64()>>11) / (1 << 53) }

// IntN returns a value in [0, n) from one Uint64 of r, as Uint64() % n.
// It panics if n <= 0 (a programmer error). Seeded sequences depend on the
// plain modulo (docs/PORTING_RUST.md 4.3), so do not replace it with a
// rejection or multiply-shift method.
func IntN(r Rng, n int) int {
	if n <= 0 {
		panic("core: IntN with non-positive n")
	}
	// The modulo bias is about n/2^64, below 2^-32 for any n < 2^32, which
	// is irrelevant for the uses in this code base (shuffles, jitter).
	return int(r.Uint64() % uint64(n))
}

// Shuffle permutes the n elements addressed by swap using r: Fisher-Yates
// from the top, swapping i with IntN(r, i+1) for i = n-1 down to 1, which
// draws exactly n-1 values. The order of draws is part of the seeded
// contract (docs/PORTING_RUST.md 4.3).
func Shuffle(r Rng, n int, swap func(i, j int)) {
	for i := n - 1; i > 0; i-- {
		swap(i, IntN(r, i+1))
	}
}
