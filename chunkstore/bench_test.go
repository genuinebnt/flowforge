package chunkstore

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/genuinebnt/flowforge/core"
)

// BenchmarkChunk measures FastCDC throughput with the default parameters
// on 64 MiB of random data (acceptance: >= 1 GB/s on a current core).
func BenchmarkChunk(b *testing.B) {
	data := splitmixBytes(1, 64<<20)
	f, err := NewFastCDC(DefaultChunkerConfig())
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		for pos := 0; pos < len(data); {
			pos += f.Cut(data[pos:])
		}
	}
	b.ReportMetric(float64(len(data))*float64(b.N)/b.Elapsed().Seconds()/1e9, "GB/s")
}

func benchSizes() []struct {
	name string
	n    int
} {
	return []struct {
		name string
		n    int
	}{{"1MB", 1 << 20}, {"100MB", 100 << 20}}
}

// BenchmarkPut stores fresh compressible content (no dedupe) into an
// in-memory store.
func BenchmarkPut(b *testing.B) {
	for _, sz := range benchSizes() {
		b.Run(sz.name, func(b *testing.B) {
			data := textish(core.NewSplitMix64(7), sz.n)
			ctx := context.Background()
			b.SetBytes(int64(sz.n))
			b.ReportAllocs()
			for b.Loop() {
				s, err := Open(ctx, NewMemBackend(), "bench", Config{})
				if err != nil {
					b.Fatal(err)
				}
				if _, _, err := s.Put(ctx, bytes.NewReader(data)); err != nil {
					b.Fatal(err)
				}
				_ = s.Close(ctx)
			}
		})
	}
}

// BenchmarkGet reads and fully verifies a stored blob.
func BenchmarkGet(b *testing.B) {
	for _, sz := range benchSizes() {
		b.Run(sz.name, func(b *testing.B) {
			data := textish(core.NewSplitMix64(7), sz.n)
			ctx := context.Background()
			s, err := Open(ctx, NewMemBackend(), "bench", Config{})
			if err != nil {
				b.Fatal(err)
			}
			id, _, err := s.Put(ctx, bytes.NewReader(data))
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(sz.n))
			b.ReportAllocs()
			for b.Loop() {
				rc, err := s.Get(ctx, id)
				if err != nil {
					b.Fatal(err)
				}
				if n, err := io.Copy(io.Discard, rc); err != nil || n != int64(sz.n) {
					b.Fatal(n, err)
				}
			}
		})
	}
}

type acceptanceResult struct {
	raw, stored          uint64
	dedupe, compression  float64
	storedRatio          float64
	bundleSize, versions int
}

// runAcceptance stores 50 versions of a ~2 MB generated JS bundle, each a
// deploy's worth of small edits with an identifier rename every tenth
// version (V2_DESIGN §6.1 acceptance).
func runAcceptance(tb testing.TB) acceptanceResult {
	ctx := context.Background()
	s, err := Open(ctx, NewMemBackend(), "acceptance", Config{})
	if err != nil {
		tb.Fatal(err)
	}
	bundle := newJSBundle(0x4a53, 2<<20)
	res := acceptanceResult{versions: 50}
	for v := 1; v <= res.versions; v++ {
		if v > 1 {
			bundle.NextVersion(v)
		}
		data := bundle.Bytes()
		res.bundleSize = len(data)
		id, _, err := s.Put(ctx, bytes.NewReader(data))
		if err != nil {
			tb.Fatal(err)
		}
		if v == res.versions {
			rc, _ := s.Get(ctx, id)
			got, err := io.ReadAll(rc)
			if err != nil || !bytes.Equal(got, data) {
				tb.Fatal("last version does not round-trip")
			}
		}
	}
	m := s.Metrics()
	res.raw, res.stored = m.LogicalBytes, m.ObjectBytes
	res.dedupe, res.compression, res.storedRatio = m.DedupeRatio(), m.CompressionRatio(), m.StoredRatio()
	return res
}

// TestChunkAcceptance_FiftyVersions: 50 versions must be stored in less
// than 15 % of their raw total (packs, indexes and ref counted).
func TestChunkAcceptance_FiftyVersions(t *testing.T) {
	if testing.Short() {
		t.Skip("stores 100 MB")
	}
	r := runAcceptance(t)
	t.Logf("bundle %d B x %d versions: raw %d, stored %d (%.2f%%), dedupe %.1fx, compression %.2fx",
		r.bundleSize, r.versions, r.raw, r.stored, 100*r.storedRatio, r.dedupe, r.compression)
	if r.bundleSize < 1800<<10 {
		t.Fatalf("bundle is %d bytes, want about 2 MB", r.bundleSize)
	}
	if r.storedRatio >= 0.15 {
		t.Fatalf("stored/raw = %.2f%%, want < 15%%", 100*r.storedRatio)
	}
}

// BenchmarkAcceptance50Versions reports the acceptance ratios.
func BenchmarkAcceptance50Versions(b *testing.B) {
	var r acceptanceResult
	for b.Loop() {
		r = runAcceptance(b)
	}
	b.ReportMetric(100*r.storedRatio, "stored_%")
	b.ReportMetric(r.dedupe, "dedupe_x")
	b.ReportMetric(r.compression, "compression_x")
	b.SetBytes(int64(r.raw))
}
