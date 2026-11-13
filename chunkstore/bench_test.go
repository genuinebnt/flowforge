package chunkstore

import (
	"bytes"
	"context"
	"io"
	"slices"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
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

// chunkAlgoCandidates are the chunkers BenchmarkChunkAlgo compares, each
// with its default parameters.
var chunkAlgoCandidates = []ChunkerConfig{{Algo: ChunkFastCDC}, {Algo: ChunkRabin}, {Algo: ChunkFixed}, {Algo: ChunkWhole}}

// spaVersions are the content conformance SPA fixture's files in deploy
// order (conformance/content/sites/spa): three page versions with their
// headers, three bundle deploys and the lineage scripts.
var spaVersions = []string{
	"v1.headers.txt", "v1.index.html", "main.3f9a1c2b.js", "index-a1b2c3d4.js",
	"v2.headers.txt", "v2.index.html", "main.77e0d41a.js", "app-e5f6a7b8.js",
	"v3.headers.txt", "v3.index.html", "main.5b6c7d8e.js", "other-0f0f0f0f.js",
}

// versionWorkloads builds the synthetic version histories: 20 versions of
// 1 MiB, each derived from the previous one by
//   - edit: 5 in-place overwrites of 1-64 bytes (lengths unchanged);
//   - insert: 5 insertions of 1-64 bytes at random offsets;
//   - shift: 1-64 bytes prepended (everything after moves);
//   - jsbundle: a deploy of the generated webpack-like bundle (statement
//     edits, new modules, a rename every tenth version).
func versionWorkloads() map[string][][]byte {
	r := core.NewSplitMix64(0x5eed)
	base := textish(r, 1<<20)
	out := map[string][][]byte{}
	for _, w := range []string{"edit", "insert", "shift"} {
		v := base
		vs := [][]byte{v}
		for range 19 {
			v = slices.Clone(v)
			switch w {
			case "edit":
				for range 5 {
					n := 1 + core.IntN(r, 64)
					copy(v[core.IntN(r, len(v)-n):], randBytes(r, n))
				}
			case "insert":
				for range 5 {
					v = slices.Insert(v, core.IntN(r, len(v)), randBytes(r, 1+core.IntN(r, 64))...)
				}
			case "shift":
				v = append(randBytes(r, 1+core.IntN(r, 64)), v...)
			}
			vs = append(vs, v)
		}
		out[w] = vs
	}
	js := newJSBundle(0x4a53, 1<<20)
	var vs [][]byte
	for v := 1; v <= 20; v++ {
		if v > 1 {
			js.NextVersion(v)
		}
		vs = append(vs, js.Bytes())
	}
	out["jsbundle"] = vs
	return out
}

// storeVersions puts every version into a fresh store and returns its
// metrics and the number of chunk references.
func storeVersions(b *testing.B, cfg ChunkerConfig, versions [][]byte) (Metrics, int) {
	ctx := context.Background()
	s, err := Open(ctx, NewMemBackend(), "bench", Config{Chunking: cfg, DeferFlush: true})
	if err != nil {
		b.Fatal(err)
	}
	refs := 0
	for _, v := range versions {
		_, st, err := s.Put(ctx, bytes.NewReader(v))
		if err != nil {
			b.Fatal(err)
		}
		refs += st.NewChunks + st.DedupChunks
	}
	if err := s.Close(ctx); err != nil {
		b.Fatal(err)
	}
	return s.Metrics(), refs
}

// BenchmarkChunkAlgo compares the chunkers (docs/ALGORITHMS.md
// "Content-defined chunking"): BenchmarkChunkAlgo/<candidate>/<workload>.
//
//   - throughput: cutting 64 MiB of random data (MB/s; GB/s reported).
//   - spa, edit, insert, shift, jsbundle: every version stored in a fresh
//     store; dedupe_x is logical bytes per new unique raw byte, stored_% the
//     backend bytes (packs, indexes, ref) per logical byte, B/chunk the mean
//     chunk length. ns/op is the time to store the history.
//   - shift-kept: kept_% is the share of a 1 MiB random input's chunks
//     that survive inserting 1-64 bytes in its first 4 KiB (mean of 20).
func BenchmarkChunkAlgo(b *testing.B) {
	random := splitmixBytes(1, 64<<20)
	workloads := versionWorkloads()
	var spa [][]byte
	for _, n := range spaVersions {
		spa = append(spa, testkit.ReadFile(b, "content/sites/spa", n))
	}
	workloads["spa"] = spa
	for _, cfg := range chunkAlgoCandidates {
		cut, err := NewCutter(cfg)
		if err != nil {
			b.Fatal(err)
		}
		name := cfg.Algo.String()
		b.Run(name+"/throughput", func(b *testing.B) {
			b.SetBytes(int64(len(random)))
			b.ReportAllocs()
			for b.Loop() {
				for pos := 0; pos < len(random); {
					pos += cut.Cut(random[pos:])
				}
			}
			b.ReportMetric(float64(len(random))*float64(b.N)/b.Elapsed().Seconds()/1e9, "GB/s")
		})
		for _, w := range []string{"spa", "edit", "insert", "shift", "jsbundle"} {
			b.Run(name+"/"+w, func(b *testing.B) {
				var m Metrics
				var refs int
				for b.Loop() {
					m, refs = storeVersions(b, cfg, workloads[w])
				}
				b.ReportMetric(m.DedupeRatio(), "dedupe_x")
				b.ReportMetric(100*m.StoredRatio(), "stored_%")
				b.ReportMetric(float64(m.LogicalBytes)/float64(max(refs, 1)), "B/chunk")
			})
		}
		b.Run(name+"/shift-kept", func(b *testing.B) {
			r := core.NewSplitMix64(3)
			type trial struct{ before, after []byte }
			var trials []trial
			for range 20 {
				d := randBytes(r, 1<<20)
				e := slices.Insert(slices.Clone(d), core.IntN(r, 4096), randBytes(r, 1+core.IntN(r, 64))...)
				trials = append(trials, trial{d, e})
			}
			var kept float64
			for b.Loop() {
				kept = 0
				for _, tr := range trials {
					before := chunkSet(tr.before, Boundaries(cut, tr.before))
					after := chunkSet(tr.after, Boundaries(cut, tr.after))
					n := 0
					for id := range before {
						if _, ok := after[id]; ok {
							n++
						}
					}
					kept += float64(n) / float64(len(before))
				}
			}
			b.ReportMetric(100*kept/float64(len(trials)), "kept_%")
		})
	}
}
