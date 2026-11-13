package chunkstore

import (
	"io"
	"math/bits"

	"github.com/genuinebnt/flowforge/core"
)

// GearSeed is the SplitMix64 seed of the FastCDC gear table: "FFCD" read
// as a big-endian u32 (V2_DESIGN §6.1).
const GearSeed uint64 = 0x46464344

// Default chunking parameters. They are part of the format: two stores
// dedupe against each other only if they chunk with the same parameters.
const (
	DefaultMinSize = 2 << 10  // 2 KiB
	DefaultAvgSize = 8 << 10  // 8 KiB
	DefaultMaxSize = 64 << 10 // 64 KiB
)

// Configuration bounds accepted by NewFastCDC.
const (
	MinAvgBits      = 8        // smallest average chunk size: 256 B
	MaxAvgBits      = 20       // largest average chunk size: 1 MiB
	MinChunkFloor   = 64       // smallest permitted MinSize
	MaxChunkCeiling = 16 << 20 // largest permitted MaxSize
	// NormalizationLevel is the FastCDC normalized-chunking level (NC):
	// MaskS has avgBits+NC one bits, MaskL avgBits-NC.
	NormalizationLevel = 2
)

// gearTable is the 256-entry gear table: entry i is the (i+1)-th output of
// core.NewSplitMix64(GearSeed). It is immutable after initialisation.
var gearTable = func() [256]uint64 {
	var t [256]uint64
	r := core.NewSplitMix64(GearSeed)
	for i := range t {
		t[i] = r.Uint64()
	}
	return t
}()

// GearTable returns a copy of the gear table.
func GearTable() [256]uint64 { return gearTable }

// maskBitOrder is the ordered list of gear-hash bit positions from which
// masks are built: a mask with k one bits sets the first k positions. The
// first 11 are the paper's 8 KiB MaskL (0x0000d90003530000), the next two
// complete its MaskA (0x0000d93003530000), the next two its MaskS
// (0x0000d9f003530000); the remaining positions extend the list for
// average sizes above 8 KiB. All positions are at bit 16 or above so every
// masked bit depends on at least the last 17 bytes (the paper's "padding
// zeros" spread the effective window to 48 bytes).
var maskBitOrder = [...]uint8{
	16, 17, 20, 22, 24, 25, 40, 43, 44, 46, 47, // MaskL, 11 bits
	36, 37, // → MaskA, 13 bits
	38, 39, // → MaskS, 15 bits
	41, 42, 45, 26, 27, 28, 29, // extension for avgBits up to 20
}

// maskWithBits returns the mask of the first k positions of maskBitOrder.
func maskWithBits(k int) uint64 {
	var m uint64
	for _, p := range maskBitOrder[:k] {
		m |= 1 << p
	}
	return m
}

// FastCDC is a validated, immutable chunker configuration. It is safe for
// concurrent use.
type FastCDC struct {
	min, avg, max int
	maskS, maskL  uint64
}

// NewFastCDC validates cfg (whose Algo must be unset or ChunkFastCDC) and
// derives the masks.
func NewFastCDC(cfg ChunkerConfig) (*FastCDC, error) {
	if cfg.Algo != 0 && cfg.Algo != ChunkFastCDC {
		return nil, errf(CodeInvalidConfig, "", "NewFastCDC given the %s algorithm", cfg.Algo)
	}
	cfg, err := cfg.canonical()
	if err != nil {
		return nil, err
	}
	b := bits.TrailingZeros(uint(cfg.AvgSize))
	return &FastCDC{
		min: cfg.MinSize, avg: cfg.AvgSize, max: cfg.MaxSize,
		maskS: maskWithBits(b + NormalizationLevel),
		maskL: maskWithBits(b - NormalizationLevel),
	}, nil
}

// Config returns the parameters.
func (f *FastCDC) Config() ChunkerConfig {
	return ChunkerConfig{Algo: ChunkFastCDC, MinSize: f.min, AvgSize: f.avg, MaxSize: f.max}
}

// MaskS returns the strict mask used before the average size.
func (f *FastCDC) MaskS() uint64 { return f.maskS }

// MaskL returns the loose mask used after the average size.
func (f *FastCDC) MaskL() uint64 { return f.maskL }

// Cut returns the length of the first chunk of data (FastCDC 2016,
// Algorithm 2, with normalized chunking). It is 0 only for empty data.
//
//	n = len(data); if n <= min: return n
//	n = min(n, max); normal = min(avg, n); fp = 0
//	for i in [min, normal): fp = (fp << 1) + gear[data[i]]; if fp & maskS == 0: return i
//	for i in [normal, n):   fp = (fp << 1) + gear[data[i]]; if fp & maskL == 0: return i
//	return n
//
// Arithmetic is modulo 2^64. As in the paper, the byte at the returned
// position took part in the decision but begins the next chunk, and the
// first min bytes are skipped (cut-point skipping), so a cut depends only
// on data[min : cut+1].
func (f *FastCDC) Cut(data []byte) int {
	n := len(data)
	if n <= f.min {
		return n
	}
	if n > f.max {
		n = f.max
	}
	normal := f.avg
	if n < normal {
		normal = n
	}
	var fp uint64
	i := f.min
	for ; i < normal; i++ {
		fp = (fp << 1) + gearTable[data[i]]
		if fp&f.maskS == 0 {
			return i
		}
	}
	for ; i < n; i++ {
		fp = (fp << 1) + gearTable[data[i]]
		if fp&f.maskL == 0 {
			return i
		}
	}
	return n
}

// Boundaries returns the end offset of every chunk of data, in order. The
// last element is len(data); empty data has no chunks.
func (f *FastCDC) Boundaries(data []byte) []int { return Boundaries(f, data) }

// NewChunker returns a streaming chunker over r (NewChunker(f, r)).
func (f *FastCDC) NewChunker(r io.Reader) *Chunker { return NewChunker(f, r) }
