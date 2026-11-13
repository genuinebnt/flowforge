package chunkstore

import (
	"bytes"
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

// CHUNK-INV-02: boundaries are a pure function of the bytes since the
// previous cut: re-chunking data[c:] from any cut c yields exactly the
// remaining cuts (shifted), and chunking a copy gives the same cuts.
func TestChunkInv02_BoundariesArePure(t *testing.T) {
	r := testkit.Rng(t)
	for _, cfg := range []ChunkerConfig{DefaultChunkerConfig(), smallChunking} {
		f := mustCDC(t, cfg)
		for range 20 {
			var data []byte
			if core.IntN(r, 2) == 0 {
				data = randBytes(r, core.IntN(r, 300_000))
			} else {
				data = textish(r, core.IntN(r, 300_000))
			}
			cuts := f.Boundaries(data)
			if !slices.Equal(cuts, f.Boundaries(slices.Clone(data))) {
				t.Fatal("same bytes, different cuts")
			}
			prev := 0
			for i, c := range cuts {
				tail := f.Boundaries(data[prev:])
				for j := range tail {
					tail[j] += prev
				}
				if !slices.Equal(tail, cuts[i:]) {
					t.Fatalf("re-chunking from cut %d diverges", prev)
				}
				prev = c
				if i > 3 {
					break // a few resyncs per input keep the test fast
				}
			}
		}
	}
}

// CHUNK-INV-03: shift resistance. Inserting k bytes near the start of
// 1 MiB of random data preserves at least 90 % of the chunks.
func TestChunkInv03_ShiftResistance(t *testing.T) {
	r := testkit.Rng(t)
	f := mustCDC(t, DefaultChunkerConfig())
	for range 20 {
		data := randBytes(r, 1<<20)
		k := 1 + core.IntN(r, 64)
		pos := core.IntN(r, 4096)
		edited := slices.Insert(slices.Clone(data), pos, randBytes(r, k)...)
		before := chunkSet(data, f.Boundaries(data))
		after := chunkSet(edited, f.Boundaries(edited))
		kept := 0
		for id := range before {
			if _, ok := after[id]; ok {
				kept++
			}
		}
		if ratio := float64(kept) / float64(len(before)); ratio < 0.90 {
			t.Fatalf("insert %d bytes at %d kept %.1f%% of %d chunks", k, pos, 100*ratio, len(before))
		}
		// O(1): the edited chunk plus a few while cut-point skipping
		// resynchronises, independent of the input size.
		if len(before)-kept > 12 {
			t.Fatalf("insert changed %d chunks, want O(1)", len(before)-kept)
		}
	}
}

// CHUNK-INV-04: every chunk length is in [min, max] except the last,
// which is in [1, max]; cuts are strictly increasing and end at len.
func TestChunkInv04_ChunkLengthBounds(t *testing.T) {
	r := testkit.Rng(t)
	cfgs := []ChunkerConfig{DefaultChunkerConfig(), smallChunking, {MinSize: 64, AvgSize: 256, MaxSize: 300}}
	for i := range testkit.Cases() / 10 {
		cfg := cfgs[i%len(cfgs)]
		f := mustCDC(t, cfg)
		var data []byte
		switch core.IntN(r, 3) {
		case 0:
			data = randBytes(r, core.IntN(r, 200_000))
		case 1:
			data = textish(r, core.IntN(r, 200_000))
		default:
			data = bytes.Repeat([]byte{byte(core.IntN(r, 256))}, core.IntN(r, 200_000)) // no cut ever matches
		}
		checkBounds(t, f, data, f.Boundaries(data))
	}
}

func checkBounds(t testing.TB, f *FastCDC, data []byte, cuts []int) {
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
	prev := 0
	for i, c := range cuts {
		n := c - prev
		last := i == len(cuts)-1
		if n < 1 || n > f.max || (!last && n < f.min) {
			t.Fatalf("chunk %d/%d has length %d outside [%d, %d]", i, len(cuts), n, f.min, f.max)
		}
		prev = c
	}
}

// CHUNK-INV-05: the streaming chunker yields exactly the slice API's
// chunks for every read size, including one-byte, half and error-at-EOF
// readers.
func TestChunkInv05_StreamingEqualsSlice(t *testing.T) {
	r := testkit.Rng(t)
	f := mustCDC(t, smallChunking)
	data := textish(r, 120_000)
	want := f.Boundaries(data)
	readers := map[string]func([]byte) io.Reader{
		"onebyte": func(b []byte) io.Reader { return iotest.OneByteReader(bytes.NewReader(b)) },
		"half":    func(b []byte) io.Reader { return iotest.HalfReader(bytes.NewReader(b)) },
		"dataerr": func(b []byte) io.Reader { return iotest.DataErrReader(bytes.NewReader(b)) },
	}
	for _, size := range []int{1, 2, 3, 7, 255, 256, 1023, 1024, 4096, 8191, 8192, 8193, 65536, 1 << 20} {
		readers["fixed"+strconv.Itoa(size)] = func(b []byte) io.Reader { return &fixedReader{b: b, n: size} }
	}
	for name, mk := range readers {
		got := streamCuts(t, f, mk(data))
		if !slices.Equal(got, want) {
			t.Fatalf("%s: streaming cuts differ from slice cuts", name)
		}
	}
	// Default parameters on random data, random read sizes.
	fd := mustCDC(t, DefaultChunkerConfig())
	for range 10 {
		d := randBytes(r, core.IntN(r, 500_000))
		got := streamCuts(t, fd, &randReader{b: d, r: r})
		if !slices.Equal(got, fd.Boundaries(d)) {
			t.Fatal("random read sizes: streaming cuts differ")
		}
	}
}

func streamCuts(t testing.TB, f *FastCDC, r io.Reader) []int {
	t.Helper()
	ch := f.NewChunker(r)
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
