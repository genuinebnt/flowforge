package chunkstore

import "math/bits"

// Rabin fingerprint chunking (FORMAT.md §1.2): the LBFS 2001 scheme as
// restic implements it, with this format's fixed polynomial, window and
// cut rule.

// RabinPolynomial is the degree-53 polynomial over GF(2) the fingerprint
// is computed modulo (bit i is the coefficient of x^i). It is
// irreducible (TestChunkRabinPolynomialIrreducible checks Ben-Or's
// criterion), as Rabin 1981 requires for the fingerprint's collision
// bound. It is part of the format.
const RabinPolynomial uint64 = 0x3DA3358B4DC173

// RabinWindow is the rolling window, in bytes (LBFS's 48).
const RabinWindow = 48

// rabinDegree is deg(RabinPolynomial); fingerprints are below 2^53.
const rabinDegree = 53

// rabinShift selects a fingerprint's top 8 bits, the byte that leaves
// the 53-bit range when the fingerprint is shifted left by 8.
const rabinShift = rabinDegree - 8

// polyDeg is the degree of a GF(2) polynomial (-1 for 0).
func polyDeg(p uint64) int { return 63 - bits.LeadingZeros64(p) }

// polyMod is x mod p over GF(2): subtract (XOR) shifted copies of p
// until the degree is below deg(p).
func polyMod(x, p uint64) uint64 {
	dp := polyDeg(p)
	for d := polyDeg(x); d >= dp; d = polyDeg(x) {
		x ^= p << (d - dp)
	}
	return x
}

// rabinTables are the two 256-entry tables of the rolling update:
// mod[b] = ((b·x^53) mod P) | b·x^53, which both clears the 8 bits that
// overflow a left shift by 8 and adds their reduction; out[b] is the
// fingerprint of b followed by RabinWindow-1 zero bytes, the
// contribution of the byte leaving the window. Immutable after
// initialisation.
var rabinMod, rabinOut = func() ([256]uint64, [256]uint64) {
	var mod, out [256]uint64
	for b := range uint64(256) {
		t := b << rabinDegree
		mod[b] = polyMod(t, RabinPolynomial) | t
		h := polyMod(b, RabinPolynomial)
		for range RabinWindow - 1 {
			h = polyMod(h<<8, RabinPolynomial)
		}
		out[b] = h
	}
	return mod, out
}()

// rabin is a validated Rabin chunker.
type rabin struct {
	min, avg, max int
	mask          uint64
}

// newRabin builds the chunker for a canonical configuration: MinSize >=
// MinChunkFloor (64) > RabinWindow, so hashing starts inside data.
func newRabin(c ChunkerConfig) *rabin {
	return &rabin{min: c.MinSize, avg: c.AvgSize, max: c.MaxSize, mask: uint64(c.AvgSize - 1)}
}

// Config implements Cutter.
func (r *rabin) Config() ChunkerConfig {
	return ChunkerConfig{Algo: ChunkRabin, MinSize: r.min, AvgSize: r.avg, MaxSize: r.max}
}

// Cut returns the length of the first chunk of data:
//
//	n = len(data); if n <= min: return n
//	n = min(n, max); fp = 0; window = 48 zero bytes
//	for i in [min-48, n):
//	    fp ^= out[oldest window byte]; window <- data[i]
//	    fp = ((fp << 8) | data[i]) ^ mod[fp >> 45]
//	    if i+1 >= min and fp & (avg-1) == avg-1: return i+1
//	return n
//
// Hashing starts 48 bytes before min with an all-zero window (whose
// fingerprint is 0), so the first test sees exactly data[min-48 : min]
// and every cut depends only on the 48 bytes before it: an insertion
// moves only the cuts whose window it touches (shift resistance). The
// cut follows the window's last byte (LBFS), unlike FastCDC's. The rule
// tests for all-one low bits so runs of zero bytes, whose fingerprint is
// 0, never cut early. The expected chunk length is about min + avg.
func (r *rabin) Cut(data []byte) int {
	n := len(data)
	if n <= r.min {
		return n
	}
	if n > r.max {
		n = r.max
	}
	var win [RabinWindow]byte
	var fp uint64
	w := 0
	for i := r.min - RabinWindow; i < n; i++ {
		b := data[i]
		fp ^= rabinOut[win[w]]
		win[w] = b
		if w++; w == RabinWindow {
			w = 0
		}
		fp = (fp<<8 | uint64(b)) ^ rabinMod[fp>>rabinShift]
		if fp&r.mask == r.mask && i+1 >= r.min {
			return i + 1
		}
	}
	return n
}
