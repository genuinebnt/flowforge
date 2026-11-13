package chunkstore

import (
	"bytes"
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
	"github.com/klauspost/compress/zstd"
)

func roundTripSizes() []int {
	sizes := []int{0, 1, 2, 63, 64, 65}
	for _, b := range []int{256, 1024, 8192, DefaultMinSize, DefaultAvgSize, DefaultMaxSize, 2 * DefaultMaxSize} {
		sizes = append(sizes, b-1, b, b+1)
	}
	return sizes
}

// CHUNK-INV-01: Get(Put(x)) == x for sizes 0, 1, the min/avg/max
// boundaries ±1 of both test configurations, and random sizes; Stat
// reports the size.
func TestChunkInv01_RoundTrip(t *testing.T) {
	r := testkit.Rng(t)
	ctx := context.Background()
	for _, cfg := range []Config{{}, {Chunking: smallChunking, TargetPackSize: 4096}, {Chunking: smallChunking, DeferFlush: true}} {
		s := openStore(t, NewMemBackend(), cfg)
		sizes := roundTripSizes()
		for range 20 {
			sizes = append(sizes, core.IntN(r, 300_000))
		}
		for i, n := range sizes {
			var x []byte
			if i%2 == 0 {
				x = randBytes(r, n)
			} else {
				x = textish(r, n)
			}
			id, st := mustPut(t, s, x)
			if st.Raw != uint64(n) {
				t.Fatalf("Stats.Raw %d, want %d", st.Raw, n)
			}
			if got := mustGet(t, s, id); !bytes.Equal(got, x) {
				t.Fatalf("size %d: round trip differs", n)
			}
			info, err := s.Stat(ctx, id)
			if err != nil || info.Size != uint64(n) || info.ID != id {
				t.Fatalf("Stat: %+v %v", info, err)
			}
		}
		if err := s.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// CHUNK-INV-06: storing the same content twice adds zero chunks, zero
// stored bytes and writes nothing to the backend; repeated chunks within
// one input are stored once.
func TestChunkInv06_Dedupe(t *testing.T) {
	r := testkit.Rng(t)
	b := NewMemBackend()
	s := openStore(t, b, Config{Chunking: smallChunking})
	for range 20 {
		x := textish(r, core.IntN(r, 100_000))
		id1, _ := mustPut(t, s, x)
		before := snapshot(b)
		id2, st := mustPut(t, s, x)
		if id1 != id2 || st.NewChunks != 0 || st.Stored != 0 || st.NewManifest || st.NewRaw != 0 {
			t.Fatalf("second put: id equal %v, stats %+v", id1 == id2, st)
		}
		if !sameObjects(before, snapshot(b)) {
			t.Fatal("second put wrote to the backend")
		}
	}
	// A block repeated 8 times: its chunks are stored about once.
	blk := randBytes(r, 64<<10)
	x := bytes.Repeat(blk, 8)
	_, st := mustPut(t, s, x)
	if st.DedupChunks < 5*st.NewChunks {
		t.Fatalf("repeated block: %d new, %d dedup", st.NewChunks, st.DedupChunks)
	}
}

// CHUNK-INV-11: the same inputs in the same order (same clock) produce
// byte-identical backend objects: packs, indexes, manifests, ref. Manifest
// ids depend only on content and chunking, not on packing.
func TestChunkInv11_Determinism(t *testing.T) {
	seed := testkit.Seed(t)
	run := func(cfg Config) (map[string][]byte, []ManifestID) {
		r := core.NewSplitMix64(seed)
		b := NewMemBackend()
		s := openStore(t, b, cfg)
		var ids []ManifestID
		base := textish(r, 80_000)
		for range 10 {
			base = mutate(r, base, 3)
			id, _ := mustPut(t, s, base)
			ids = append(ids, id)
		}
		if err := s.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		return snapshot(b), ids
	}
	clock := testkit.NewFakeClock(time.Unix(1_700_000_000, 0))
	a, idsA := run(Config{Chunking: smallChunking, Clock: clock})
	b, idsB := run(Config{Chunking: smallChunking, Clock: clock})
	if !sameObjects(a, b) || !slices.Equal(idsA, idsB) {
		t.Fatal("same inputs produced different bytes")
	}
	_, idsC := run(Config{Chunking: smallChunking, DeferFlush: true, TargetPackSize: 10_000})
	if !slices.Equal(idsA, idsC) {
		t.Fatal("manifest ids depend on packing")
	}
	// Manifest and index encodings are pure functions of their fields.
	m := Manifest{TotalLen: 3, Chunks: []ChunkID{SumChunk([]byte("a")), SumChunk([]byte("b")), SumChunk([]byte("c"))}}
	if !bytes.Equal(m.Encode(), (&Manifest{TotalLen: 3, Chunks: slices.Clone(m.Chunks)}).Encode()) {
		t.Fatal("manifest encoding not deterministic")
	}
	es := []IndexEntry{{ID: SumChunk([]byte("x")), Offset: 16, CompLen: 3, RawLen: 3}, {ID: SumChunk([]byte("y")), Offset: 35, CompLen: 3, RawLen: 3}}
	i1, _ := NewIndex(PackID{1}, 100, 7, es)
	i2, _ := NewIndex(PackID{1}, 100, 7, []IndexEntry{es[1], es[0]})
	if !bytes.Equal(i1.Encode(), i2.Encode()) {
		t.Fatal("index encoding depends on entry order")
	}
}

// CHUNK-INV-12: the pinned klauspost encoder is byte-deterministic (two
// encoders, repeated calls, concurrency 1 vs 8 give the same frames), and
// the format does not depend on the compressed bytes: records written by
// a different encoder (another level) are read back and keep the same
// chunk ids.
func TestChunkInv12_ZstdDeterminismAndFormatIndependence(t *testing.T) {
	r := testkit.Rng(t)
	c1, err := newCodec()
	if err != nil {
		t.Fatal(err)
	}
	defer c1.close()
	c2, _ := newCodec()
	defer c2.close()
	opts := append(encoderOptions(zstd.SpeedDefault), zstd.WithEncoderConcurrency(8))
	c8, err := zstd.NewWriter(nil, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer c8.Close()
	for range 50 {
		x := textish(r, 1+core.IntN(r, 70_000))
		f1 := c1.enc.EncodeAll(x, nil)
		if !bytes.Equal(f1, c2.enc.EncodeAll(x, nil)) || !bytes.Equal(f1, c1.enc.EncodeAll(x, nil)) || !bytes.Equal(f1, c8.EncodeAll(x, nil)) {
			t.Fatal("zstd output not deterministic for fixed settings")
		}
	}

	// A pack whose frames come from SpeedBestCompression and SpeedFastest
	// encoders is readable by a normal store, with the same ids.
	ctx := context.Background()
	b := NewMemBackend()
	s := openStore(t, b, Config{Chunking: smallChunking})
	x := textish(r, 50_000)
	id, _ := mustPut(t, s, x)
	for _, lvl := range []int{1, 19} {
		other, err := newCodecLevel(lvl)
		if err != nil {
			t.Fatal(err)
		}
		// Rewrite every pack with the other encoder.
		for _, p := range s.packs {
			data, _ := b.Get(ctx, s.ns.pack(p.id))
			pk, err := DecodePack(data)
			if err != nil {
				t.Fatal(err)
			}
			w := NewPackWriter(pk.CreatedUnixNs)
			var es []IndexEntry
			for _, e := range p.idx.Entries {
				raw, err := s.codec.openRecord(e, data[e.Offset:e.Offset+e.RecordSize()])
				if err != nil {
					t.Fatal(err)
				}
				codec, payload := other.encode(raw)
				es = append(es, IndexEntry{ID: e.ID, Offset: w.Append(e.RawLen, codec, payload), CompLen: uint32(len(payload)), RawLen: e.RawLen})
			}
			pdata, pid, crc := w.Finish()
			ix, _ := NewIndex(pid, uint64(len(pdata)), crc, es)
			nb := NewMemBackend()
			_ = nb.Put(ctx, s.ns.pack(pid), pdata)
			_ = nb.Put(ctx, s.ns.index(pid), ix.Encode())
			rb, _ := (&Ref{Generation: 1, Packs: []PackID{pid}}).Encode()
			_ = nb.Put(ctx, s.ns.ref(), rb)
			s2 := openStore(t, nb, Config{Chunking: smallChunking})
			if got := mustGet(t, s2, id); !bytes.Equal(got, x) {
				t.Fatalf("level %d frames: content differs", lvl)
			}
			if rep, _ := s2.Fsck(ctx, []ManifestID{id}); len(rep.Problems) != 0 {
				t.Fatalf("level %d frames: fsck %+v", lvl, rep.Problems)
			}
		}
		other.close()
	}
}

// CHUNK-INV-16: concurrent Put, Get, Stat and GC are race-free and lose
// nothing: every blob whose Put completed is readable at the end, and
// fsck is clean.
func TestChunkInv16_Concurrency(t *testing.T) {
	seed := testkit.Seed(t)
	ctx := context.Background()
	s := openStore(t, NewMemBackend(), Config{Chunking: smallChunking, TargetPackSize: 16 << 10})
	base := textish(core.NewSplitMix64(seed), 40_000)
	// The bookkeeping follows the GC-grace contract: a Put and the
	// recording of its id happen under mu.RLock; the live set is taken
	// under mu.Lock, so it names every returned Put.
	var (
		mu     sync.RWMutex
		doneMu sync.Mutex
		done   = map[ManifestID][]byte{}
	)
	var wg sync.WaitGroup
	for w := range 6 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r := core.NewSplitMix64(seed + uint64(w) + 1)
			for range 15 {
				x := mutate(r, base, 1+core.IntN(r, 4))
				mu.RLock()
				id, _, err := s.Put(ctx, bytes.NewReader(x))
				if err != nil {
					mu.RUnlock()
					t.Error(err)
					return
				}
				doneMu.Lock()
				done[id] = x
				doneMu.Unlock()
				mu.RUnlock()
				got, err := getAll(s, id)
				if err != nil || !bytes.Equal(got, x) {
					t.Errorf("concurrent get: %v", err)
					return
				}
			}
		}(w)
	}
	stop := make(chan struct{})
	var gcwg sync.WaitGroup
	gcwg.Add(1)
	go func() {
		defer gcwg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			mu.Lock()
			live := make([]ManifestID, 0, len(done))
			for id := range done {
				live = append(live, id)
			}
			mu.Unlock()
			if _, err := s.GC(ctx, live, nil, GCOptions{LiveThresholdPercent: 100, RecompressLevel: -1}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	wg.Wait()
	close(stop)
	gcwg.Wait()
	live := make([]ManifestID, 0, len(done))
	for id, x := range done {
		live = append(live, id)
		if got := mustGet(t, s, id); !bytes.Equal(got, x) {
			t.Fatal("content lost")
		}
	}
	if _, err := s.GC(ctx, live, nil, GCOptions{LiveThresholdPercent: 100}); err != nil {
		t.Fatal(err)
	}
	rep, err := s.Fsck(ctx, live)
	if err != nil || len(rep.Problems) != 0 {
		t.Fatalf("fsck: %v %+v", err, rep.Problems)
	}
}

// CHUNK-INV-17: tenant isolation. Two namespaces on one backend storing
// identical content each pay for their own chunks; presence, Stats,
// Metrics, Get, GC and Fsck of one never reflect the other.
func TestChunkInv17_TenantIsolation(t *testing.T) {
	r := testkit.Rng(t)
	ctx := context.Background()
	b := NewMemBackend()
	a, err := Open(ctx, b, "tenant-a", Config{Chunking: smallChunking})
	if err != nil {
		t.Fatal(err)
	}
	x := textish(r, 60_000)
	idA, stA := mustPut(t, a, x)
	bb, err := Open(ctx, b, "tenant-b", Config{Chunking: smallChunking})
	if err != nil {
		t.Fatal(err)
	}
	if bb.Has(ChunkID(idA)) {
		t.Fatal("tenant b sees tenant a's manifest")
	}
	if _, err := bb.Get(ctx, idA); CodeOf(err) != CodeManifestNotFound {
		t.Fatalf("tenant b reads tenant a's blob: %v", err)
	}
	if _, err := bb.Stat(ctx, idA); CodeOf(err) != CodeManifestNotFound {
		t.Fatalf("tenant b stats tenant a's blob: %v", err)
	}
	idB, stB := mustPut(t, bb, x)
	if idA != idB {
		t.Fatal("content ids differ across tenants")
	}
	if stB.DedupChunks != stA.DedupChunks || stB.NewChunks != stA.NewChunks || !stB.NewManifest || stB.Stored != stA.Stored {
		t.Fatalf("tenant b's stats reflect tenant a: a %+v b %+v", stA, stB)
	}
	ma, mb := a.Metrics(), bb.Metrics()
	if ma.Chunks != mb.Chunks || ma.ObjectBytes != mb.ObjectBytes {
		t.Fatalf("metrics differ: %+v vs %+v", ma, mb)
	}
	// GC of a with nothing live (twice: past the grace) leaves b intact.
	for range 2 {
		if _, err := a.GC(ctx, nil, nil, GCOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if a.Has(ChunkID(idA)) {
		t.Fatal("tenant a kept an unreachable manifest")
	}
	if got := mustGet(t, bb, idB); !bytes.Equal(got, x) {
		t.Fatal("tenant a's GC damaged tenant b")
	}
	rep, err := bb.Fsck(ctx, []ManifestID{idB})
	if err != nil || len(rep.Problems) != 0 {
		t.Fatalf("fsck b: %v %+v", err, rep.Problems)
	}
	// Namespaces are mandatory and validated.
	for _, ns := range []string{"", "A", "a/b", "..", "a.b", string(make([]byte, 65))} {
		if _, err := Open(ctx, b, ns, Config{}); CodeOf(err) != CodeInvalidConfig {
			t.Errorf("namespace %q: %v", ns, err)
		}
	}
	names, _ := b.List(ctx, "")
	for _, n := range names {
		if !bytes.HasPrefix([]byte(n), []byte("t/tenant-a/")) && !bytes.HasPrefix([]byte(n), []byte("t/tenant-b/")) {
			t.Fatalf("object %q outside the namespaces", n)
		}
	}
}

// CHUNK-INV-19: index lookup (fan-out + binary search) agrees with a
// linear scan for present and absent ids (differential test).
func TestChunkInv19_IndexLookupMatchesScan(t *testing.T) {
	r := testkit.Rng(t)
	for range 50 {
		n := core.IntN(r, 600)
		var es []IndexEntry
		off := uint64(PackHeaderSize)
		seen := map[ChunkID]bool{}
		for range n {
			var id ChunkID
			copy(id[:], randBytes(r, 32))
			if core.IntN(r, 4) == 0 && len(es) > 0 {
				id[0] = es[core.IntN(r, len(es))].ID[0] // crowd buckets
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			cl := uint32(1 + core.IntN(r, 100))
			es = append(es, IndexEntry{ID: id, Offset: off, CompLen: cl, RawLen: cl})
			off += RecordHeaderSize + uint64(cl)
		}
		ix, err := NewIndex(PackID{}, off+PackFooterSize, 0, es)
		if err != nil {
			t.Fatal(err)
		}
		dec, err := DecodeIndex(ix.Encode())
		if err != nil {
			t.Fatal(err)
		}
		for range 200 {
			var q ChunkID
			if len(es) > 0 && core.IntN(r, 2) == 0 {
				q = es[core.IntN(r, len(es))].ID
			} else {
				copy(q[:], randBytes(r, 32))
			}
			var want IndexEntry
			found := false
			for _, e := range es {
				if e.ID == q {
					want, found = e, true
				}
			}
			got, ok := dec.Lookup(q)
			if ok != found || got != want {
				t.Fatalf("lookup %s: got %v %v, want %v %v", q, got, ok, want, found)
			}
		}
	}
}

// CHUNK-INV-20: a store reopened from its backend serves the same blobs
// and dedupes against what it already holds.
func TestChunkInv20_ReopenPersistence(t *testing.T) {
	r := testkit.Rng(t)
	b := NewMemBackend()
	s := openStore(t, b, Config{Chunking: smallChunking, DeferFlush: true, TargetPackSize: 20_000})
	blobs := map[ManifestID][]byte{}
	for range 10 {
		x := textish(r, core.IntN(r, 50_000))
		id, _ := mustPut(t, s, x)
		blobs[id] = x
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Put(context.Background(), bytes.NewReader(nil)); CodeOf(err) != CodeClosed {
		t.Fatalf("put after close: %v", err)
	}
	s2 := openStore(t, b, Config{Chunking: smallChunking})
	for id, x := range blobs {
		if got := mustGet(t, s2, id); !bytes.Equal(got, x) {
			t.Fatal("reopened store differs")
		}
		if _, st := mustPut(t, s2, x); st.NewChunks != 0 || st.NewManifest {
			t.Fatalf("reopened store does not dedupe: %+v", st)
		}
	}
}

// CHUNK-INV-23: metrics are consistent with Stats and with the backend:
// ingest counters are the sums of Put stats, ObjectBytes is the sum of
// the namespace's object sizes, and the ratios follow from the counters.
func TestChunkInv23_MetricsConsistency(t *testing.T) {
	r := testkit.Rng(t)
	ctx := context.Background()
	b := NewMemBackend()
	s := openStore(t, b, Config{Chunking: smallChunking})
	var sum Stats
	base := textish(r, 100_000)
	for range 10 {
		base = mutate(r, base, 2)
		_, st := mustPut(t, s, base)
		sum.Raw += st.Raw
		sum.Stored += st.Stored
		sum.NewChunks += st.NewChunks
		sum.DedupChunks += st.DedupChunks
		sum.NewRaw += st.NewRaw
	}
	m := s.Metrics()
	if m.Puts != 10 || m.LogicalBytes != sum.Raw || m.NewStoredBytes != sum.Stored || m.NewChunks != uint64(sum.NewChunks) ||
		m.DedupChunks != uint64(sum.DedupChunks) || m.NewRawBytes != sum.NewRaw {
		t.Fatalf("metrics %+v vs stats %+v", m, sum)
	}
	var objBytes uint64
	names, _ := b.List(ctx, "t/"+testNS+"/")
	for _, n := range names {
		d, _ := b.Get(ctx, n)
		objBytes += uint64(len(d))
	}
	if m.ObjectBytes != objBytes || m.CompressedBytes != m.NewStoredBytes {
		t.Fatalf("ObjectBytes %d, backend holds %d; compressed %d stored %d", m.ObjectBytes, objBytes, m.CompressedBytes, m.NewStoredBytes)
	}
	if d := m.DedupeRatio(); d < 5 {
		t.Fatalf("dedupe ratio %.2f for 10 near-identical versions", d)
	}
	if c := m.CompressionRatio(); c <= 1 {
		t.Fatalf("compression ratio %.2f on text", c)
	}
}
