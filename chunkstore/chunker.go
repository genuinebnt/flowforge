package chunkstore

import (
	"errors"
	"io"
	"math/bits"
)

// ChunkAlgo identifies a chunking algorithm (a pluggable decision,
// docs/ALGORITHMS.md "Content-defined chunking"). Its byte value is part
// of the format: version-2 manifests record it (FORMAT.md §1, §5), so a
// store whose blobs were written with different chunkers stays readable
// and each blob's chunk list can be re-derived from its bytes.
type ChunkAlgo uint8

// Chunking algorithms. The zero value means "unset": the default
// (FastCDC) in a Config, and "not recorded" in a version-1 manifest.
const (
	// ChunkFastCDC is FastCDC 2016 with normalized chunking (FastCDC),
	// the default.
	ChunkFastCDC ChunkAlgo = 1
	// ChunkRabin is content-defined chunking with a Rabin fingerprint
	// over a 48-byte rolling window (LBFS 2001, as in restic): cut where
	// the fingerprint's low bits are all one (Rabin).
	ChunkRabin ChunkAlgo = 2
	// ChunkFixed cuts every AvgSize bytes (Fixed): no shift resistance,
	// the baseline content-defined chunking is measured against.
	ChunkFixed ChunkAlgo = 3
	// ChunkWhole stores each blob as one chunk, split only every MaxSize
	// bytes (Whole): dedupe of identical blobs only.
	ChunkWhole ChunkAlgo = 4
)

// ChunkAlgos lists every algorithm, in format-byte order.
func ChunkAlgos() []ChunkAlgo { return []ChunkAlgo{ChunkFastCDC, ChunkRabin, ChunkFixed, ChunkWhole} }

// String returns "fastcdc", "rabin", "fixed", "whole", or "unset".
func (a ChunkAlgo) String() string {
	switch a {
	case ChunkFastCDC:
		return "fastcdc"
	case ChunkRabin:
		return "rabin"
	case ChunkFixed:
		return "fixed"
	case ChunkWhole:
		return "whole"
	case 0:
		return "unset"
	}
	return "unknown"
}

// ParseChunkAlgo is the inverse of String ("" is the default, FastCDC).
func ParseChunkAlgo(s string) (ChunkAlgo, error) {
	switch s {
	case "", "fastcdc":
		return ChunkFastCDC, nil
	case "rabin":
		return ChunkRabin, nil
	case "fixed":
		return ChunkFixed, nil
	case "whole":
		return ChunkWhole, nil
	}
	return 0, errf(CodeInvalidConfig, "", "unknown chunker %q (fastcdc, rabin, fixed, whole)", s)
}

// Default parameters of the other algorithms. Rabin uses FastCDC's
// 2/8/64 KiB so the two content-defined candidates are compared at the
// same bounds; fixed-size chunks are FastCDC's average size; whole-blob
// pieces are the largest chunk any algorithm may produce.
const (
	DefaultFixedSize    = DefaultAvgSize
	DefaultWholeMaxSize = MaxChunkCeiling
)

// ChunkerConfig selects a chunking algorithm and its parameters. The zero
// value is FastCDC with the defaults (2 KiB / 8 KiB / 64 KiB). With all
// three sizes zero, an algorithm takes its own defaults. Canonical forms,
// which manifests record: FastCDC and Rabin (min, avg, max); fixed
// (size, size, size); whole (0, 0, max).
type ChunkerConfig struct {
	// Algo is the algorithm (zero: FastCDC).
	Algo    ChunkAlgo
	MinSize int // every chunk but the last is at least this long
	AvgSize int // FastCDC normalization point / Rabin mask; a power of two. Fixed: the chunk size
	MaxSize int // no chunk is longer
}

// DefaultChunkerConfig returns the format's default parameters (FastCDC).
func DefaultChunkerConfig() ChunkerConfig {
	return ChunkerConfig{Algo: ChunkFastCDC, MinSize: DefaultMinSize, AvgSize: DefaultAvgSize, MaxSize: DefaultMaxSize}
}

// withDefaults fills the algorithm and, when no size is given, the
// algorithm's default sizes.
func (c ChunkerConfig) withDefaults() ChunkerConfig {
	if c.Algo == 0 {
		c.Algo = ChunkFastCDC
	}
	if c.MinSize != 0 || c.AvgSize != 0 || c.MaxSize != 0 {
		return c
	}
	switch c.Algo {
	case ChunkFastCDC, ChunkRabin:
		c.MinSize, c.AvgSize, c.MaxSize = DefaultMinSize, DefaultAvgSize, DefaultMaxSize
	case ChunkFixed:
		c.AvgSize = DefaultFixedSize
	case ChunkWhole:
		c.MaxSize = DefaultWholeMaxSize
	}
	return c
}

// canonical validates c (after defaults) and returns its canonical form.
func (c ChunkerConfig) canonical() (ChunkerConfig, error) {
	c = c.withDefaults()
	switch c.Algo {
	case ChunkFastCDC, ChunkRabin:
		if c.AvgSize <= 0 || c.AvgSize&(c.AvgSize-1) != 0 {
			return c, errf(CodeInvalidConfig, "", "%s: AvgSize %d is not a power of two", c.Algo, c.AvgSize)
		}
		b := bits.TrailingZeros(uint(c.AvgSize))
		if b < MinAvgBits || b > MaxAvgBits {
			return c, errf(CodeInvalidConfig, "", "%s: AvgSize %d outside [2^%d, 2^%d]", c.Algo, c.AvgSize, MinAvgBits, MaxAvgBits)
		}
		if c.MinSize < MinChunkFloor || c.MinSize >= c.AvgSize || c.AvgSize >= c.MaxSize || c.MaxSize > MaxChunkCeiling {
			return c, errf(CodeInvalidConfig, "", "%s: need %d <= MinSize < AvgSize < MaxSize <= %d, got %d/%d/%d",
				c.Algo, MinChunkFloor, MaxChunkCeiling, c.MinSize, c.AvgSize, c.MaxSize)
		}
	case ChunkFixed:
		if c.AvgSize < MinChunkFloor || c.AvgSize > MaxChunkCeiling {
			return c, errf(CodeInvalidConfig, "", "fixed: size %d outside [%d, %d]", c.AvgSize, MinChunkFloor, MaxChunkCeiling)
		}
		if (c.MinSize != 0 && c.MinSize != c.AvgSize) || (c.MaxSize != 0 && c.MaxSize != c.AvgSize) {
			return c, errf(CodeInvalidConfig, "", "fixed: MinSize and MaxSize must be 0 or the size, got %d/%d/%d", c.MinSize, c.AvgSize, c.MaxSize)
		}
		c.MinSize, c.MaxSize = c.AvgSize, c.AvgSize
	case ChunkWhole:
		if c.MinSize != 0 || c.AvgSize != 0 {
			return c, errf(CodeInvalidConfig, "", "whole: only MaxSize applies, got %d/%d/%d", c.MinSize, c.AvgSize, c.MaxSize)
		}
		if c.MaxSize < MinChunkFloor || c.MaxSize > MaxChunkCeiling {
			return c, errf(CodeInvalidConfig, "", "whole: MaxSize %d outside [%d, %d]", c.MaxSize, MinChunkFloor, MaxChunkCeiling)
		}
	default:
		return c, errf(CodeInvalidConfig, "", "unknown chunker algorithm %d", c.Algo)
	}
	return c, nil
}

// Cutter is one chunking algorithm with validated parameters. Every
// implementation is immutable and safe for concurrent use.
type Cutter interface {
	// Cut returns the length of the first chunk of data: 0 only for empty
	// data, else in [1, MaxSize]. It must be given at least MaxSize bytes
	// unless data is the rest of the stream. The cut depends only on the
	// bytes of data (no state across calls), so a stream is chunked by
	// repeated cuts from offset 0 (CHUNK-INV-02).
	Cut(data []byte) int
	// Config returns the canonical parameters, as manifests record them.
	Config() ChunkerConfig
}

// NewCutter validates cfg and returns its algorithm.
func NewCutter(cfg ChunkerConfig) (Cutter, error) {
	c, err := cfg.canonical()
	if err != nil {
		return nil, err
	}
	switch c.Algo {
	case ChunkFastCDC:
		return NewFastCDC(c)
	case ChunkRabin:
		return newRabin(c), nil
	case ChunkFixed:
		return fixedCutter{size: c.AvgSize}, nil
	default:
		return wholeCutter{max: c.MaxSize}, nil
	}
}

// Boundaries returns the end offset of every chunk of data under c, in
// order. The last element is len(data); empty data has no chunks.
func Boundaries(c Cutter, data []byte) []int {
	var out []int
	for pos := 0; pos < len(data); {
		pos += c.Cut(data[pos:])
		out = append(out, pos)
	}
	return out
}

// fixedCutter cuts every size bytes.
type fixedCutter struct{ size int }

func (f fixedCutter) Cut(data []byte) int { return min(len(data), f.size) }

func (f fixedCutter) Config() ChunkerConfig {
	return ChunkerConfig{Algo: ChunkFixed, MinSize: f.size, AvgSize: f.size, MaxSize: f.size}
}

// wholeCutter keeps a blob whole, up to max bytes per chunk (a record
// holds at most MaxRecordRaw, and the streaming chunker buffers a chunk).
type wholeCutter struct{ max int }

func (w wholeCutter) Cut(data []byte) int { return min(len(data), w.max) }

func (w wholeCutter) Config() ChunkerConfig { return ChunkerConfig{Algo: ChunkWhole, MaxSize: w.max} }

// NewChunker returns a streaming chunker over r. Its chunks are exactly
// those of Boundaries(c, ·) over the concatenated stream, whatever the
// read sizes of r (CHUNK-INV-05).
func NewChunker(c Cutter, r io.Reader) *Chunker {
	mx := c.Config().MaxSize
	// The buffer starts at 256 KiB and grows to 2*max on demand, so a
	// whole-blob chunker (max 16 MiB) costs memory only for large blobs.
	return &Chunker{c: c, max: mx, limit: max(2*mx, 256<<10), r: r, buf: make([]byte, 256<<10)}
}

// Chunker splits a stream into chunks. It is not safe for concurrent use.
type Chunker struct {
	c          Cutter
	max        int
	limit      int // largest buffer
	r          io.Reader
	buf        []byte
	start, end int
	eof        bool
	err        error
}

// errNoProgress mirrors bufio's guard against readers that return (0, nil)
// forever.
var errNoProgress = errors.New("chunkstore: reader returned no data 100 times")

// Next returns the next chunk, or io.EOF after the last one. The slice is
// valid until the next call. A read error is returned as is.
func (c *Chunker) Next() ([]byte, error) {
	if c.err != nil {
		return nil, c.err
	}
	if c.end-c.start < c.max && !c.eof {
		if err := c.fill(); err != nil {
			c.err = err
			return nil, err
		}
	}
	if c.start == c.end {
		c.err = io.EOF
		return nil, io.EOF
	}
	n := c.c.Cut(c.buf[c.start:c.end])
	chunk := c.buf[c.start : c.start+n]
	c.start += n
	return chunk, nil
}

// fill reads until at least max bytes are buffered or the reader ends:
// a cut needs max bytes of lookahead (or the end of the stream) to be the
// cut of the slice API.
func (c *Chunker) fill() error {
	if c.start > 0 {
		copy(c.buf, c.buf[c.start:c.end])
		c.end -= c.start
		c.start = 0
	}
	empty := 0
	for c.end < c.max && !c.eof {
		if c.end == len(c.buf) {
			nb := make([]byte, min(2*len(c.buf), c.limit))
			copy(nb, c.buf[:c.end])
			c.buf = nb
		}
		n, err := c.r.Read(c.buf[c.end:])
		if n < 0 || n > len(c.buf)-c.end {
			return errors.New("chunkstore: reader returned an invalid count")
		}
		c.end += n
		switch {
		case err == io.EOF:
			c.eof = true
		case err != nil:
			return err
		case n == 0:
			empty++
			if empty >= 100 {
				return errNoProgress
			}
		}
	}
	return nil
}
