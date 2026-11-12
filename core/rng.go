package core

import (
	crand "crypto/rand"
	"encoding/binary"
)

// Rng is the randomness port (spec 24.4). Implementations need not be safe
// for concurrent use; callers that share one must synchronize.
type Rng interface {
	Uint64() uint64
}

// SystemRng draws from the operating system's cryptographic source.
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

// SplitMix64 is a small, fast, deterministic Rng. It is the reference
// seeded generator used by tests, simulations and anything that must
// reproduce a sequence from a seed.
type SplitMix64 struct{ state uint64 }

// NewSplitMix64 returns a generator seeded with seed.
func NewSplitMix64(seed uint64) *SplitMix64 { return &SplitMix64{state: seed} }

// Uint64 implements Rng.
func (s *SplitMix64) Uint64() uint64 {
	s.state += 0x9e3779b97f4a7c15
	z := s.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// Float64 returns a uniformly distributed value in [0, 1) from r.
func Float64(r Rng) float64 { return float64(r.Uint64()>>11) / (1 << 53) }

// IntN returns a uniformly distributed value in [0, n) from r. It panics
// if n <= 0.
func IntN(r Rng, n int) int {
	if n <= 0 {
		panic("core: IntN with non-positive n")
	}
	// Lemire's nearly-divisionless method would be faster; the modulo bias
	// here is below 2^-32 for any n that fits a slice index and is
	// irrelevant for the uses in this code base (shuffles, jitter).
	return int(r.Uint64() % uint64(n))
}

// Shuffle permutes the n elements addressed by swap using r (Fisher-Yates).
func Shuffle(r Rng, n int, swap func(i, j int)) {
	for i := n - 1; i > 0; i-- {
		swap(i, IntN(r, i+1))
	}
}
