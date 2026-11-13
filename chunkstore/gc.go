package chunkstore

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
)

// DefaultLiveThresholdPercent: GC rewrites a pack whose live bytes are
// below this percentage of its record bytes.
const DefaultLiveThresholdPercent = 50

// GCOptions tunes a collection.
type GCOptions struct {
	// LiveThresholdPercent in [1, 100]; zero means 50. A pack is rewritten
	// when live*100 < threshold*total, where live and total are summed
	// record sizes (16 + stored_len). Packs with no live record are always
	// deleted; fully live packs are never rewritten.
	LiveThresholdPercent int
	// RecompressLevel is the zstd level for records copied into new packs
	// (zero: DefaultRecompressLevel = 19). Each copied chunk keeps the
	// smaller of its current zstd payload and the recompressed one, and
	// only if that saves at least 1/8 of raw_len; otherwise it is stored
	// raw (codec 0). Negative: copy records verbatim.
	RecompressLevel int
}

// GCReport summarises a collection.
type GCReport struct {
	LiveManifests   int // distinct manifests marked (live and pinned)
	PinnedManifests int
	GraceManifests  int // manifests kept by GC grace (Put returned since the last GC began)
	MarkedChunks    int // chunks reachable from the live manifests, manifests included
	PacksKept       int
	PacksRewritten  int // packs whose live chunks were copied into new packs
	PacksDropped    int // packs with no live chunk, deleted outright
	PacksWritten    int
	ChunksDropped   int   // unreachable chunks removed
	BytesReclaimed  int64 // object bytes deleted minus object bytes written
	OrphansDeleted  int   // unreferenced pack objects removed
	Recompressed    int   // copied chunks whose payload changed
}

// GC deletes every chunk not reachable from live or pinned (caller
// supplied manifest ids: live is what retention keeps, pinned are
// versions cited by reviewed or alerted changes, which retention never
// releases) from the packs it rewrites, in a crash-safe order:
//
//  1. seal the open pack;
//  2. mark: read and verify each live and pinned manifest; a missing
//     manifest or a missing chunk aborts GC with nothing changed;
//  3. plan: per pack (ascending id) compute live/total record bytes;
//  4. write the new packs and their indexes holding the live records of
//     the rewritten packs (each record fully verified, then recompressed
//     per GCOptions.RecompressLevel);
//  5. replace the ref (the commit point);
//  6. delete the rewritten and dropped packs, then any orphan object
//     under the namespace's packs/ not named by the ref (leftovers of
//     earlier crashes).
//
// Besides live and pinned, GC keeps the GC-grace set described at
// Store.Put. A crash before step 5 leaves the old ref, so every old pack is still
// there; a crash after it leaves only orphans. GC holds the store's
// write lock throughout.
func (s *Store) GC(ctx context.Context, live, pinned []ManifestID, opts GCOptions) (GCReport, error) {
	var rep GCReport
	pct := opts.LiveThresholdPercent
	if pct == 0 {
		pct = DefaultLiveThresholdPercent
	}
	if pct < 1 || pct > 100 {
		return rep, errf(CodeInvalidConfig, "", "LiveThresholdPercent %d outside [1, 100]", pct)
	}
	level := opts.RecompressLevel
	if level == 0 {
		level = DefaultRecompressLevel
	}
	if level > 22 {
		return rep, errf(CodeInvalidConfig, "", "RecompressLevel %d above 22", level)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return rep, ErrClosed
	}
	if err := s.sealLocked(ctx); err != nil {
		return rep, err
	}

	// Mark.
	marked := map[ChunkID]struct{}{}
	pinnedSet := map[ManifestID]struct{}{}
	for _, mid := range pinned {
		pinnedSet[mid] = struct{}{}
	}
	rep.PinnedManifests = len(pinnedSet)
	for _, mid := range slices.Concat(pinned, live) {
		if _, done := marked[ChunkID(mid)]; done {
			continue
		}
		rep.LiveManifests++
		mb, err := s.readChunkLocked(ctx, ChunkID(mid), CodeManifestNotFound)
		if err != nil {
			return rep, err
		}
		m, err := decodeManifestChunk(mid, mb)
		if err != nil {
			return rep, err
		}
		marked[ChunkID(mid)] = struct{}{}
		for _, c := range m.Chunks {
			if _, ok := s.locs[c]; !ok {
				return rep, errf(CodeChunkMissing, c.String(), "referenced by live manifest %s", mid)
			}
			marked[c] = struct{}{}
		}
	}
	// Grace: manifests put since the last GC began, and the chunks of
	// running Puts (see Store.Put).
	grace := make([]ManifestID, 0, len(s.recent))
	for mid := range s.recent {
		if _, done := marked[ChunkID(mid)]; !done {
			grace = append(grace, mid)
		}
	}
	slices.SortFunc(grace, func(a, b ManifestID) int { return bytes.Compare(a[:], b[:]) })
	for _, mid := range grace {
		if _, ok := s.locs[ChunkID(mid)]; !ok {
			continue // lost to an earlier failure; nothing to keep
		}
		rep.GraceManifests++
		mb, err := s.readChunkLocked(ctx, ChunkID(mid), CodeManifestNotFound)
		if err != nil {
			return rep, err
		}
		m, err := decodeManifestChunk(mid, mb)
		if err != nil {
			return rep, err
		}
		marked[ChunkID(mid)] = struct{}{}
		for _, c := range m.Chunks {
			marked[c] = struct{}{}
		}
	}
	for ps := range s.inflight {
		for id := range ps.ids {
			marked[id] = struct{}{}
		}
	}
	rep.MarkedChunks = len(marked)

	// Plan, in ascending pack id order so the result is deterministic.
	order := make([]int, len(s.packs))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int { return bytes.Compare(s.packs[a].id[:], s.packs[b].id[:]) })
	var keep, rewrite, drop []int
	for _, i := range order {
		var liveB, total uint64
		for _, e := range s.packs[i].idx.Entries {
			total += e.RecordSize()
			if s.isLiveCopy(marked, i, e.ID) {
				liveB += e.RecordSize()
			}
		}
		switch {
		case liveB == total:
			keep = append(keep, i)
		case liveB == 0:
			drop = append(drop, i)
		case liveB*100 < uint64(pct)*total:
			rewrite = append(rewrite, i)
		default:
			keep = append(keep, i)
		}
	}

	var recodec *codec
	if level > 0 && len(rewrite) > 0 {
		var err error
		if recodec, err = newCodecLevel(level); err != nil {
			return rep, err
		}
		defer recodec.close()
	}

	// Copy live records of rewritten packs into new packs.
	var written []packState
	var writtenBytes int64
	var w *PackWriter
	var entries []IndexEntry
	flush := func() error {
		if w == nil || w.Count() == 0 {
			return nil
		}
		ps, size, err := s.writePack(ctx, w, entries)
		if err != nil {
			return err
		}
		written = append(written, ps)
		writtenBytes += int64(size) + int64(ps.idxSize)
		w, entries = nil, nil
		return nil
	}
	for _, i := range rewrite {
		p := s.packs[i]
		name := s.ns.pack(p.id)
		data, err := s.b.Get(ctx, name)
		if errors.Is(err, ErrObjectNotFound) {
			return rep, errf(CodePackMissing, name, "live pack is missing")
		}
		if err != nil {
			return rep, backendErr(name, err)
		}
		if uint64(len(data)) != p.idx.PackSize {
			return rep, errf(CodeIndexPackMismatch, name, "pack is %d bytes, index says %d", len(data), p.idx.PackSize)
		}
		es := slices.Clone(p.idx.Entries)
		slices.SortFunc(es, func(a, b IndexEntry) int { return cmpU64(a.Offset, b.Offset) })
		for _, e := range es {
			if !s.isLiveCopy(marked, i, e.ID) {
				continue
			}
			rec := data[e.Offset : e.Offset+e.RecordSize()]
			raw, err := s.codec.openRecord(e, rec)
			if err != nil {
				return rep, withObject(err, name)
			}
			h, _ := parseRecordHeader(rec)
			codec, payload := h.Codec, rec[RecordHeaderSize:]
			if recodec != nil {
				codec, payload = recompress(recodec, raw, h.Codec, payload)
				if codec != h.Codec || !bytes.Equal(payload, rec[RecordHeaderSize:]) {
					rep.Recompressed++
				}
			}
			if w == nil {
				w = NewPackWriter(s.now())
			}
			off := w.Append(e.RawLen, codec, payload)
			entries = append(entries, IndexEntry{ID: e.ID, Offset: off, CompLen: uint32(len(payload)), RawLen: e.RawLen})
			if w.Size() >= s.cfg.TargetPackSize {
				if err := flush(); err != nil {
					return rep, err
				}
			}
		}
	}
	if err := flush(); err != nil {
		return rep, err
	}

	// Commit.
	var next []packState
	for _, i := range keep {
		next = append(next, s.packs[i])
	}
	next = append(next, written...)
	replaced := map[string]struct{}{}
	if len(rewrite)+len(drop) > 0 {
		ids := make([]PackID, len(next))
		for i, p := range next {
			ids[i] = p.id
		}
		ref, size, err := s.writeRef(ctx, ids)
		if err != nil {
			return rep, err
		}
		s.ref, s.refSize = ref, size
		var reclaimed int64
		for _, i := range slices.Concat(rewrite, drop) {
			p := s.packs[i]
			replaced[s.ns.pack(p.id)] = struct{}{}
			reclaimed += int64(p.idx.PackSize) + int64(p.idxSize)
			for _, e := range p.idx.Entries {
				if !s.isLiveCopy(marked, i, e.ID) {
					rep.ChunksDropped++
				}
			}
		}
		rep.BytesReclaimed = reclaimed - writtenBytes
		s.packs = next
		slices.SortFunc(s.packs, func(a, b packState) int { return bytes.Compare(a.id[:], b.id[:]) })
		s.rebuildLocs()
	}
	rep.PacksKept, rep.PacksRewritten, rep.PacksDropped, rep.PacksWritten = len(keep), len(rewrite), len(drop), len(written)

	// Delete everything under packs/ that the ref does not name: the
	// packs just replaced and orphans of earlier crashes.
	names, err := s.b.List(ctx, s.ns.packs())
	if err != nil {
		return rep, backendErr(s.ns.packs(), err)
	}
	wanted := make(map[string]struct{}, 2*len(s.packs))
	for _, p := range s.packs {
		wanted[s.ns.pack(p.id)] = struct{}{}
		wanted[s.ns.index(p.id)] = struct{}{}
	}
	for _, n := range names {
		if _, ok := wanted[n]; ok {
			continue
		}
		if err := s.b.Delete(ctx, n); err != nil {
			return rep, backendErr(n, err)
		}
		if _, ok := replaced[n]; !ok && strings.HasSuffix(n, ".pack") {
			rep.OrphansDeleted++
		}
	}
	s.recent = map[ManifestID]struct{}{}
	return rep, nil
}

// recompress chooses the payload for a chunk copied by GC: the smaller of
// the current zstd payload (if any) and a fresh frame at the GC level,
// kept only if it saves at least 1/8 of the raw size; raw otherwise. Ties
// keep the current payload.
func recompress(c *codec, raw []byte, codec uint8, payload []byte) (uint8, []byte) {
	frame := c.enc.EncodeAll(raw, make([]byte, 0, len(raw)/2+64))
	best := frame
	if codec == CodecZstd && len(payload) <= len(frame) {
		best = payload
	}
	if keepCompressed(len(best), len(raw)) {
		return CodecZstd, best
	}
	return CodecRaw, raw
}

// isLiveCopy reports whether entry id of pack i is live: marked, and the
// copy the presence cache points at (a duplicate copy in another pack is
// dead).
func (s *Store) isLiveCopy(marked map[ChunkID]struct{}, i int, id ChunkID) bool {
	if _, ok := marked[id]; !ok {
		return false
	}
	return s.locs[id].pack == int32(i)
}

func cmpU64(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
