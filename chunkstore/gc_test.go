package chunkstore

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// reachable computes the closure of manifests (and their chunks) in s.
func reachable(t testing.TB, s *Store, mids []ManifestID) map[ChunkID]struct{} {
	t.Helper()
	out := map[ChunkID]struct{}{}
	for _, mid := range mids {
		mb, err := s.readChunk(context.Background(), ChunkID(mid), CodeManifestNotFound)
		if err != nil {
			t.Fatalf("manifest %s: %v", mid, err)
		}
		m, err := DecodeManifest(mb)
		if err != nil {
			t.Fatal(err)
		}
		out[ChunkID(mid)] = struct{}{}
		for _, c := range m.Chunks {
			out[c] = struct{}{}
		}
	}
	return out
}

func keysOf(m map[ManifestID][]byte) []ManifestID {
	return slices.SortedFunc(maps.Keys(m), func(a, b ManifestID) int { return bytes.Compare(a[:], b[:]) })
}

// CHUNK-INV-09: over random put / drop / pin / gc / reopen sequences, GC
// never removes a chunk reachable from a live, pinned or grace manifest
// (every such blob still reads back exactly), fsck is clean after every
// GC, every surviving pack is fully live or at least threshold % live,
// and with threshold 100 nothing unreachable remains.
func TestChunkInv09_GCSafetyAndReclaim(t *testing.T) {
	r := testkit.Rng(t)
	ctx := context.Background()
	iters := max(5, testkit.Cases()/20)
	rewritten, dropped := 0, 0
	defer func() { t.Logf("%d sequences: %d packs rewritten, %d dropped", iters, rewritten, dropped) }()
	for range iters {
		b := NewMemBackend()
		cfg := Config{Chunking: smallChunking, TargetPackSize: 2048 + core.IntN(r, 64<<10), DeferFlush: core.IntN(r, 3) > 0}
		s := openStore(t, b, cfg)
		live := map[ManifestID][]byte{}
		pinned := map[ManifestID][]byte{}
		base := textish(r, 20_000)
		for range 15 + core.IntN(r, 25) {
			switch op := core.IntN(r, 10); {
			case op < 4: // put a variant of the base or of a live blob
				src := base
				if len(live) > 0 && core.IntN(r, 2) == 0 {
					ks := keysOf(live)
					src = live[ks[core.IntN(r, len(ks))]]
				}
				x := mutate(r, src, 1+core.IntN(r, 5))
				if core.IntN(r, 6) == 0 {
					x = randBytes(r, core.IntN(r, 30_000))
				}
				id, _ := mustPut(t, s, x)
				if _, p := pinned[id]; !p {
					live[id] = x
				}
			case op < 6: // retention drops a blob
				if ks := keysOf(live); len(ks) > 0 {
					delete(live, ks[core.IntN(r, len(ks))])
				}
			case op < 7: // a reviewed change pins a blob
				if ks := keysOf(live); len(ks) > 0 {
					k := ks[core.IntN(r, len(ks))]
					pinned[k] = live[k]
					delete(live, k)
				}
			case op < 9:
				pct := 1 + core.IntN(r, 100)
				level := []int{-1, 1, 3, 19}[core.IntN(r, 4)]
				s.mu.RLock()
				grace := slices.Collect(maps.Keys(s.recent))
				s.mu.RUnlock()
				rep, err := s.GC(ctx, keysOf(live), keysOf(pinned), GCOptions{LiveThresholdPercent: pct, RecompressLevel: level})
				if err != nil {
					t.Fatalf("GC: %v", err)
				}
				rewritten += rep.PacksRewritten
				dropped += rep.PacksDropped
				checkAfterGC(t, s, live, pinned, grace, pct)
			default:
				if err := s.Close(ctx); err != nil {
					t.Fatal(err)
				}
				s = openStore(t, b, cfg)
			}
		}
		// Two full collections (the first may keep grace manifests):
		// exactly the reachable chunks remain.
		for range 2 {
			if _, err := s.GC(ctx, keysOf(live), keysOf(pinned), GCOptions{LiveThresholdPercent: 100}); err != nil {
				t.Fatal(err)
			}
		}
		want := reachable(t, s, slices.Concat(keysOf(live), keysOf(pinned)))
		if m := s.Metrics(); m.Chunks != uint64(len(want)) {
			t.Fatalf("after full GC %d chunks remain, %d reachable", m.Chunks, len(want))
		}
		checkAfterGC(t, s, live, pinned, nil, 100)
	}
}

func checkAfterGC(t *testing.T, s *Store, live, pinned map[ManifestID][]byte, grace []ManifestID, pct int) {
	t.Helper()
	ctx := context.Background()
	all := slices.Concat(keysOf(live), keysOf(pinned))
	for _, set := range []map[ManifestID][]byte{live, pinned} {
		for id, x := range set {
			if got := mustGet(t, s, id); !bytes.Equal(got, x) {
				t.Fatalf("GC damaged blob %s", id)
			}
		}
	}
	fr, err := s.Fsck(ctx, all)
	if err != nil || len(fr.Problems) != 0 {
		t.Fatalf("fsck after GC: %v %+v", err, fr.Problems)
	}
	var keepIDs []ManifestID
	keepIDs = append(keepIDs, all...)
	for _, g := range grace {
		if s.Has(ChunkID(g)) {
			keepIDs = append(keepIDs, g)
		}
	}
	marked := reachable(t, s, keepIDs)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i, p := range s.packs {
		var liveB, total uint64
		for _, e := range p.idx.Entries {
			total += e.RecordSize()
			if _, ok := marked[e.ID]; ok && s.locs[e.ID].pack == int32(i) {
				liveB += e.RecordSize()
			}
		}
		if liveB != total && liveB*100 < uint64(pct)*total {
			// GC seals the open pack before it plans, so no pack may be
			// below the threshold right after it.
			t.Fatalf("pack %s is %d/%d live, below %d%%", p.id, liveB, total, pct)
		}
	}
	if pct == 100 {
		for id := range s.locs {
			if _, ok := marked[id]; !ok {
				t.Fatalf("threshold 100 left unreachable chunk %s", id)
			}
		}
	}
}

// buildGCFixture returns a backend holding several packs, some fully
// dead, some partly dead, with the live set and its contents.
func buildGCFixture(t *testing.T, r core.Rng) (*MemBackend, map[ManifestID][]byte) {
	t.Helper()
	b := NewMemBackend()
	s := openStore(t, b, Config{Chunking: smallChunking, TargetPackSize: 6000})
	live := map[ManifestID][]byte{}
	base := textish(r, 15_000)
	for i := range 8 {
		x := mutate(r, base, 3)
		if i%3 == 0 {
			x = randBytes(r, 9000)
		}
		id, _ := mustPut(t, s, x)
		if i%2 == 0 {
			live[id] = x
		}
	}
	// Leave an orphan from an "earlier crash" too.
	orphan := NewPackWriter(0)
	orphan.Append(3, CodecRaw, []byte("abc"))
	od, oid, _ := orphan.Finish()
	_ = b.Put(context.Background(), s.ns.pack(oid), od)
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	return b, live
}

// CHUNK-INV-10: GC is crash safe. Injecting a crash at every mutating
// backend call of a GC (the call fails; or it succeeds and the
// acknowledgement is lost), then reopening, leaves a store whose fsck has
// no errors and which serves every live blob exactly; a later GC on a
// healthy backend completes the collection (fsck without any problem,
// nothing unreachable left).
func TestChunkInv10_GCCrashSafety(t *testing.T) {
	r := testkit.Rng(t)
	ctx := context.Background()
	base, live := buildGCFixture(t, r)
	liveIDs := keysOf(live)
	opts := GCOptions{LiveThresholdPercent: 100}

	count := &faultBackend{Backend: memFrom(snapshot(base)), k: -1}
	s := openStore(t, count, Config{Chunking: smallChunking, TargetPackSize: 6000})
	if _, err := s.GC(ctx, liveIDs, nil, opts); err != nil {
		t.Fatal(err)
	}
	steps := count.ops
	t.Logf("GC makes %d mutating calls", steps)
	if steps < 5 {
		t.Fatalf("GC made only %d mutating calls; fixture too small", steps)
	}
	for k := range steps {
		for _, ackLost := range []bool{false, true} {
			inner := memFrom(snapshot(base))
			fb := &faultBackend{Backend: inner, k: k, ackLost: ackLost}
			s := openStore(t, fb, Config{Chunking: smallChunking, TargetPackSize: 6000})
			if _, err := s.GC(ctx, liveIDs, nil, opts); err == nil {
				t.Fatalf("step %d: GC ignored the crash", k)
			}
			// Restart on the surviving bytes.
			s2 := openStore(t, inner, Config{Chunking: smallChunking, TargetPackSize: 6000})
			rep, err := s2.Fsck(ctx, liveIDs)
			if err != nil || !rep.OK() {
				t.Fatalf("step %d ackLost=%v: fsck after crash: %v %+v", k, ackLost, err, rep.Problems)
			}
			for id, x := range live {
				if got := mustGet(t, s2, id); !bytes.Equal(got, x) {
					t.Fatalf("step %d: live blob lost", k)
				}
			}
			if _, err := s2.GC(ctx, liveIDs, nil, opts); err != nil {
				t.Fatalf("step %d: recovery GC: %v", k, err)
			}
			rep, _ = s2.Fsck(ctx, liveIDs)
			if len(rep.Problems) != 0 {
				t.Fatalf("step %d: after recovery GC: %+v", k, rep.Problems)
			}
			if m, want := s2.Metrics().Chunks, len(reachable(t, s2, liveIDs)); m != uint64(want) {
				t.Fatalf("step %d: %d chunks remain, %d reachable", k, m, want)
			}
		}
	}
}

// CHUNK-INV-15: Put is crash safe. A crash at any mutating call of a Put
// leaves, after reopening, an fsck-clean store that serves every blob put
// before; the interrupted blob is either complete or absent.
func TestChunkInv15_PutCrashSafety(t *testing.T) {
	r := testkit.Rng(t)
	ctx := context.Background()
	cfg := Config{Chunking: smallChunking, TargetPackSize: 4000}
	base := NewMemBackend()
	s := openStore(t, base, cfg)
	prior := map[ManifestID][]byte{}
	for range 3 {
		x := textish(r, 10_000)
		id, _ := mustPut(t, s, x)
		prior[id] = x
	}
	x := mutate(r, textish(r, 30_000), 2)
	count := &faultBackend{Backend: memFrom(snapshot(base)), k: -1}
	sc := openStore(t, count, cfg)
	newID, _ := mustPut(t, sc, x)
	for k := range count.ops {
		for _, ackLost := range []bool{false, true} {
			inner := memFrom(snapshot(base))
			s := openStore(t, &faultBackend{Backend: inner, k: k, ackLost: ackLost}, cfg)
			if _, _, err := s.Put(ctx, bytes.NewReader(x)); err == nil {
				t.Fatalf("step %d: Put ignored the crash", k)
			}
			s2 := openStore(t, inner, cfg)
			ids := keysOf(prior)
			if s2.Has(ChunkID(newID)) {
				ids = append(ids, newID)
				if got := mustGet(t, s2, newID); !bytes.Equal(got, x) {
					t.Fatalf("step %d: interrupted blob visible but wrong", k)
				}
			}
			rep, err := s2.Fsck(ctx, ids)
			if err != nil || !rep.OK() {
				t.Fatalf("step %d: fsck: %v %+v", k, err, rep.Problems)
			}
			for id, want := range prior {
				if got := mustGet(t, s2, id); !bytes.Equal(got, want) {
					t.Fatalf("step %d: prior blob lost", k)
				}
			}
		}
	}
}

// CHUNK-INV-18: pinned manifests are never collected, whatever the live
// set (retention) says.
func TestChunkInv18_Pinning(t *testing.T) {
	r := testkit.Rng(t)
	ctx := context.Background()
	s := openStore(t, NewMemBackend(), Config{Chunking: smallChunking, TargetPackSize: 4096})
	var ids []ManifestID
	var blobs [][]byte
	for range 6 {
		x := textish(r, 12_000)
		id, _ := mustPut(t, s, x)
		ids, blobs = append(ids, id), append(blobs, x)
	}
	pinned := []ManifestID{ids[1], ids[4]}
	for range 3 { // past the grace period
		rep, err := s.GC(ctx, nil, pinned, GCOptions{LiveThresholdPercent: 100})
		if err != nil {
			t.Fatal(err)
		}
		if rep.PinnedManifests != 2 {
			t.Fatalf("report %+v", rep)
		}
	}
	for i, id := range ids {
		isPinned := slices.Contains(pinned, id)
		got, err := getAll(s, id)
		if isPinned && (err != nil || !bytes.Equal(got, blobs[i])) {
			t.Fatalf("pinned blob %d collected: %v", i, err)
		}
		if !isPinned && CodeOf(err) != CodeManifestNotFound {
			t.Fatalf("unpinned, unnamed blob %d survived: %v", i, err)
		}
	}
	// A pinned id that does not exist aborts GC without changing anything.
	before := s.Metrics()
	if _, err := s.GC(ctx, nil, []ManifestID{{0xde, 0xad}}, GCOptions{}); CodeOf(err) != CodeManifestNotFound {
		t.Fatalf("missing pinned manifest: %v", err)
	}
	if after := s.Metrics(); after.Chunks != before.Chunks {
		t.Fatal("failed GC changed the store")
	}
}

// CHUNK-INV-21: GC grace. A blob whose Put returned before a GC began
// survives that GC even when not named, and is collected by the next GC
// that does not name it.
func TestChunkInv21_GCGrace(t *testing.T) {
	r := testkit.Rng(t)
	ctx := context.Background()
	s := openStore(t, NewMemBackend(), Config{Chunking: smallChunking})
	x := textish(r, 20_000)
	id, _ := mustPut(t, s, x)
	rep, err := s.GC(ctx, nil, nil, GCOptions{LiveThresholdPercent: 100})
	if err != nil || rep.GraceManifests != 1 {
		t.Fatalf("first GC: %+v %v", rep, err)
	}
	if got := mustGet(t, s, id); !bytes.Equal(got, x) {
		t.Fatal("grace blob damaged")
	}
	if _, err := s.GC(ctx, nil, nil, GCOptions{LiveThresholdPercent: 100}); err != nil {
		t.Fatal(err)
	}
	if s.Has(ChunkID(id)) || s.Metrics().Chunks != 0 {
		t.Fatal("blob survived a second GC that did not name it")
	}
}

// CHUNK-INV-22: GC recompression keeps chunk identity and content, picks
// the payload by the 1/8 rule (zstd only when 8*stored_len <= 7*raw_len,
// raw otherwise) and never grows a compressible record.
func TestChunkInv22_RecompressionRule(t *testing.T) {
	r := testkit.Rng(t)
	ctx := context.Background()
	b := NewMemBackend()
	s := openStore(t, b, Config{Chunking: smallChunking, TargetPackSize: 1 << 20, DeferFlush: true})
	text := textish(r, 40_000)
	noise := randBytes(r, 40_000)
	keepT, _ := mustPut(t, s, text)
	keepN, _ := mustPut(t, s, noise)
	mustPut(t, s, textish(r, 40_000)) // dead, so the pack is rewritten
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	before := map[ChunkID]uint32{}
	for _, p := range s.packs {
		for _, e := range p.idx.Entries {
			before[e.ID] = e.CompLen
		}
	}
	for range 2 {
		if _, err := s.GC(ctx, []ManifestID{keepT, keepN}, nil, GCOptions{LiveThresholdPercent: 100, RecompressLevel: 19}); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range s.packs {
		data, _ := b.Get(ctx, s.ns.pack(p.id))
		pk, err := DecodePack(data)
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range pk.Records {
			if rec.Codec == CodecZstd && !keepCompressed(int(rec.CompLen), int(rec.RawLen)) {
				t.Fatalf("zstd record %d of %d kept without saving 1/8", rec.CompLen, rec.RawLen)
			}
		}
		for _, e := range p.idx.Entries {
			if old, ok := before[e.ID]; ok && e.CompLen > old {
				t.Fatalf("recompression grew chunk %s from %d to %d", e.ID, old, e.CompLen)
			}
		}
	}
	for id, want := range map[ManifestID][]byte{keepT: text, keepN: noise} {
		if got := mustGet(t, s, id); !bytes.Equal(got, want) {
			t.Fatal("recompressed blob differs")
		}
	}
	// Random data is stored raw at ingest already.
	info, _ := s.Stat(ctx, keepN)
	if info.StoredBytes < info.Size {
		t.Fatalf("random data compressed to %d of %d", info.StoredBytes, info.Size)
	}
}
