package chunkstore

import (
	"bytes"
	"fmt"
	"io"
	"slices"
	"strconv"
	"testing"
	"testing/iotest"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

func mustCDC(t testing.TB, cfg ChunkerConfig) *FastCDC {
	t.Helper()
	f, err := NewFastCDC(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func chunkSet(data []byte, cuts []int) map[ChunkID]struct{} {
	set := map[ChunkID]struct{}{}
	prev := 0
	for _, c := range cuts {
		set[SumChunk(data[prev:c])] = struct{}{}
		prev = c
	}
	return set
}

// chunkerCandidates are the configurations every chunking invariant runs
// against: each algorithm with its defaults and with small parameters
// that exercise every code path quickly.
func chunkerCandidates() []ChunkerConfig {
	return []ChunkerConfig{
		DefaultChunkerConfig(), smallChunking,
		{Algo: ChunkRabin}, {Algo: ChunkRabin, MinSize: 256, AvgSize: 1024, MaxSize: 8192},
		{Algo: ChunkFixed}, {Algo: ChunkFixed, AvgSize: 1000},
		{Algo: ChunkWhole}, {Algo: ChunkWhole, MaxSize: 5000},
	}
}

func mustCutter(t testing.TB, cfg ChunkerConfig) Cutter {
	t.Helper()
	c, err := NewCutter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func cfgName(c ChunkerConfig) string {
	return fmt.Sprintf("%s-%d-%d-%d", c.Algo, c.MinSize, c.AvgSize, c.MaxSize)
}

// CHUNK-INV-02: boundaries are a pure function of the bytes since the
// previous cut: re-chunking data[c:] from any cut c yields exactly the
// remaining cuts (shifted), and chunking a copy gives the same cuts. For
// every chunker.
func TestChunkInv02_BoundariesArePure(t *testing.T) {
	r := testkit.Rng(t)
	for _, cfg := range chunkerCandidates() {
		f := mustCutter(t, cfg)
		for range 10 {
			var data []byte
			if core.IntN(r, 2) == 0 {
				data = randBytes(r, core.IntN(r, 300_000))
			} else {
				data = textish(r, core.IntN(r, 300_000))
			}
			cuts := Boundaries(f, data)
			if !slices.Equal(cuts, Boundaries(f, slices.Clone(data))) {
				t.Fatalf("%s: same bytes, different cuts", cfgName(cfg))
			}
			prev := 0
			for i, c := range cuts {
				tail := Boundaries(f, data[prev:])
				for j := range tail {
					tail[j] += prev
				}
				if !slices.Equal(tail, cuts[i:]) {
					t.Fatalf("%s: re-chunking from cut %d diverges", cfgName(cfg), prev)
				}
				prev = c
				if i > 3 {
					break // a few resyncs per input keep the test fast
				}
			}
		}
	}
}

// CHUNK-INV-03: shift resistance of the content-defined chunkers
// (FastCDC and Rabin; fixed-size and whole-blob chunking claim none, which
// BenchmarkChunkAlgo/*/shift measures). Inserting k bytes near the start
// of 1 MiB of random data preserves at least 90 % of the chunks, and
// changes O(1) of them.
func TestChunkInv03_ShiftResistance(t *testing.T) {
	r := testkit.Rng(t)
	for _, cfg := range []ChunkerConfig{DefaultChunkerConfig(), {Algo: ChunkRabin}} {
		f := mustCutter(t, cfg)
		for range 20 {
			data := randBytes(r, 1<<20)
			k := 1 + core.IntN(r, 64)
			pos := core.IntN(r, 4096)
			edited := slices.Insert(slices.Clone(data), pos, randBytes(r, k)...)
			before := chunkSet(data, Boundaries(f, data))
			after := chunkSet(edited, Boundaries(f, edited))
			kept := 0
			for id := range before {
				if _, ok := after[id]; ok {
					kept++
				}
			}
			if ratio := float64(kept) / float64(len(before)); ratio < 0.90 {
				t.Fatalf("%s: insert %d bytes at %d kept %.1f%% of %d chunks", cfgName(cfg), k, pos, 100*ratio, len(before))
			}
			// O(1): the edited chunk plus a few while cut-point skipping
			// resynchronises, independent of the input size.
			if len(before)-kept > 12 {
				t.Fatalf("%s: insert changed %d chunks, want O(1)", cfgName(cfg), len(before)-kept)
			}
		}
	}
}

// CHUNK-INV-04: every chunk length is in [min, max] except the last,
// which is in [1, max]; cuts are strictly increasing and end at len. For
// fixed-size chunking min = max = the size; for whole-blob chunking every
// chunk but the last is exactly max.
func TestChunkInv04_ChunkLengthBounds(t *testing.T) {
	r := testkit.Rng(t)
	cfgs := append(chunkerCandidates(), ChunkerConfig{MinSize: 64, AvgSize: 256, MaxSize: 300},
		ChunkerConfig{Algo: ChunkRabin, MinSize: 64, AvgSize: 256, MaxSize: 300})
	for i := range testkit.Cases() / 10 {
		cfg := cfgs[i%len(cfgs)]
		f := mustCutter(t, cfg)
		var data []byte
		switch core.IntN(r, 3) {
		case 0:
			data = randBytes(r, core.IntN(r, 200_000))
		case 1:
			data = textish(r, core.IntN(r, 200_000))
		default:
			data = bytes.Repeat([]byte{byte(core.IntN(r, 256))}, core.IntN(r, 200_000)) // no cut ever matches
		}
		checkBounds(t, f, data, Boundaries(f, data))
	}
}

// chunkBounds are the length bounds of a non-last chunk.
func chunkBounds(c ChunkerConfig) (lo, hi int) {
	if c.Algo == ChunkWhole {
		return c.MaxSize, c.MaxSize
	}
	return c.MinSize, c.MaxSize
}

func checkBounds(t testing.TB, f Cutter, data []byte, cuts []int) {
	t.Helper()
	if len(data) == 0 {
		if len(cuts) != 0 {
			t.Fatalf("empty input gave %d chunks", len(cuts))
		}
		return
	}
	if cuts[len(cuts)-1] != len(data) {
		t.Fatalf("last cut %d != len %d", cuts[len(cuts)-1], len(data))
	}
	lo, hi := chunkBounds(f.Config())
	prev := 0
	for i, c := range cuts {
		n := c - prev
		last := i == len(cuts)-1
		if n < 1 || n > hi || (!last && n < lo) {
			t.Fatalf("%s: chunk %d/%d has length %d outside [%d, %d]", cfgName(f.Config()), i, len(cuts), n, lo, hi)
		}
		prev = c
	}
}

// CHUNK-INV-05: the streaming chunker yields exactly the slice API's
// chunks for every read size, including one-byte, half and error-at-EOF
// readers, for every chunker (whole-blob chunking with a 16 MiB maximum
// grows its buffer past the initial 256 KiB).
func TestChunkInv05_StreamingEqualsSlice(t *testing.T) {
	r := testkit.Rng(t)
	data := textish(r, 120_000)
	readers := map[string]func([]byte) io.Reader{
		"onebyte": func(b []byte) io.Reader { return iotest.OneByteReader(bytes.NewReader(b)) },
		"half":    func(b []byte) io.Reader { return iotest.HalfReader(bytes.NewReader(b)) },
		"dataerr": func(b []byte) io.Reader { return iotest.DataErrReader(bytes.NewReader(b)) },
	}
	for _, size := range []int{1, 2, 3, 7, 255, 256, 1023, 1024, 4096, 8191, 8192, 8193, 65536, 1 << 20} {
		readers["fixed"+strconv.Itoa(size)] = func(b []byte) io.Reader { return &fixedReader{b: b, n: size} }
	}
	for _, cfg := range chunkerCandidates() {
		f := mustCutter(t, cfg)
		want := Boundaries(f, data)
		for name, mk := range readers {
			got := streamCuts(t, f, mk(data))
			if !slices.Equal(got, want) {
				t.Fatalf("%s %s: streaming cuts differ from slice cuts", cfgName(cfg), name)
			}
		}
		// Random data, random read sizes, beyond the 256 KiB initial
		// buffer.
		for range 4 {
			d := randBytes(r, core.IntN(r, 600_000))
			got := streamCuts(t, f, &randReader{b: d, r: r})
			if !slices.Equal(got, Boundaries(f, d)) {
				t.Fatalf("%s: random read sizes: streaming cuts differ", cfgName(cfg))
			}
		}
	}
	// A whole blob above the initial buffer is one chunk.
	big := randBytes(r, 3<<20)
	if got := streamCuts(t, mustCutter(t, ChunkerConfig{Algo: ChunkWhole}), &randReader{b: big, r: r}); !slices.Equal(got, []int{len(big)}) {
		t.Fatalf("whole blob of 3 MiB cut at %v", got)
	}
}

func streamCuts(t testing.TB, f Cutter, r io.Reader) []int {
	t.Helper()
	ch := NewChunker(f, r)
	var cuts []int
	pos := 0
	for {
		c, err := ch.Next()
		if err == io.EOF {
			return cuts
		}
		if err != nil {
			t.Fatal(err)
		}
		pos += len(c)
		cuts = append(cuts, pos)
	}
}

type fixedReader struct {
	b []byte
	n int
}

func (f *fixedReader) Read(p []byte) (int, error) {
	if len(f.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p[:min(len(p), f.n)], f.b)
	f.b = f.b[n:]
	return n, nil
}

type randReader struct {
	b []byte
	r core.Rng
}

func (rr *randReader) Read(p []byte) (int, error) {
	if len(rr.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p[:min(len(p), 1+core.IntN(rr.r, 20_000))], rr.b)
	rr.b = rr.b[n:]
	return n, nil
}

// The chunker reports reader errors and readers that make no progress.
func TestChunkerReaderErrors(t *testing.T) {
	f := mustCDC(t, smallChunking)
	ch := f.NewChunker(iotest.ErrReader(io.ErrUnexpectedEOF))
	if _, err := ch.Next(); err != io.ErrUnexpectedEOF {
		t.Fatalf("got %v", err)
	}
	ch = f.NewChunker(zeroReader{})
	if _, err := ch.Next(); err != errNoProgress {
		t.Fatalf("got %v", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read([]byte) (int, error) { return 0, nil }

// Invalid configurations are rejected with INVALID_CONFIG.
func TestChunkerConfigValidation(t *testing.T) {
	bad := []ChunkerConfig{
		{MinSize: 2048, AvgSize: 8000, MaxSize: 65536},         // not a power of two
		{MinSize: 32, AvgSize: 256, MaxSize: 1024},             // min too small
		{MinSize: 4096, AvgSize: 4096, MaxSize: 65536},         // min == avg
		{MinSize: 2048, AvgSize: 8192, MaxSize: 8192},          // avg == max
		{MinSize: 64, AvgSize: 128, MaxSize: 1024},             // avg below 2^8
		{MinSize: 1 << 20, AvgSize: 1 << 21, MaxSize: 1 << 22}, // avg above 2^20
		{MinSize: 2048, AvgSize: 8192, MaxSize: 32 << 20},      // max above ceiling
	}
	for _, c := range bad {
		if _, err := NewFastCDC(c); CodeOf(err) != CodeInvalidConfig {
			t.Errorf("%+v: got %v", c, err)
		}
	}
	// Mask bit counts follow avgBits ± NormalizationLevel for every
	// permitted average, and MaskL ⊂ MaskS.
	for b := MinAvgBits; b <= MaxAvgBits; b++ {
		f := mustCDC(t, ChunkerConfig{MinSize: 64, AvgSize: 1 << b, MaxSize: 1<<b + 1})
		if popcount(f.MaskS()) != b+2 || popcount(f.MaskL()) != b-2 || f.MaskL()&^f.MaskS() != 0 {
			t.Errorf("avg 2^%d: masks %#x %#x", b, f.MaskS(), f.MaskL())
		}
	}
}

func popcount(x uint64) int {
	n := 0
	for ; x != 0; x &= x - 1 {
		n++
	}
	return n
}
