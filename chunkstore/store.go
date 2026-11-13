package chunkstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"sync"

	"github.com/genuinebnt/flowforge/core"
)

// DefaultTargetPackSize is the open-pack size at which a pack is sealed.
const DefaultTargetPackSize = 16 << 20

// Config configures a Store. The zero value is valid.
type Config struct {
	// Chunking selects the chunking algorithm and its parameters (zero:
	// FastCDC with the defaults). Each manifest records the chunker that
	// cut it, so a store holds and serves blobs cut by any mix of
	// chunkers; dedupe works only between blobs cut alike.
	Chunking ChunkerConfig
	// TargetPackSize seals the open pack once it holds this many bytes
	// (zero: DefaultTargetPackSize).
	TargetPackSize int
	// DeferFlush keeps the open pack across Puts: Put then returns before
	// its chunks are durable, and Flush or Close makes them durable. By
	// default every Put seals the open pack before returning.
	DeferFlush bool
	// Clock stamps created_unix_ns in pack headers. Nil writes 0, which
	// keeps the stored bytes a pure function of the inputs.
	Clock core.Clock
}

// Stats describes one Put.
type Stats struct {
	Raw         uint64 // bytes read from the input
	Stored      uint64 // record bytes (header + payload) newly added, manifest included
	NewChunks   int    // data chunks not present before
	DedupChunks int    // data chunks already present (or repeated within the input)
	NewRaw      uint64 // uncompressed bytes of the new data chunks
	NewManifest bool   // the manifest itself was new
}

// ManifestInfo describes a stored blob.
type ManifestInfo struct {
	ID   ManifestID
	Size uint64 // total uncompressed length
	// Chunking is the chunker the manifest records (Algo zero: a version-1
	// manifest, which does not record it).
	Chunking     ChunkerConfig
	Chunks       int    // chunk references, in order
	UniqueChunks int    // distinct chunks
	StoredBytes  uint64 // record bytes of the distinct chunks plus the manifest
}

// loc locates a chunk: pack is an index into Store.packs, or -1 for the
// open (unsealed) pack.
type loc struct {
	pack int32
	e    IndexEntry
}

// packState is one sealed, live pack: its id, decoded index and the
// encoded index size (for Metrics.ObjectBytes).
type packState struct {
	id      PackID
	idx     *Index
	idxSize int
}

// Store is a content-addressed, deduplicating, compressed blob store over
// one namespace of a Backend. It is constructed only by Open.
//
// Locking: one sync.RWMutex guards the ref, the pack list, the presence
// cache (chunk id → location) and the open pack. Chunking, hashing,
// compression and decompression run outside the lock. Put takes the write
// lock briefly per new chunk (re-checking presence, appending to the open
// pack) and for sealing, which writes the pack, its index and the ref
// while holding it. Get holds the read lock only while it looks up and
// fetches one record. GC and Fsck hold the write lock for their whole
// run, so they never observe a half-written pack and never delete a pack
// a reader is fetching from. Every operation is safe for concurrent use;
// the bytes written depend on the order in which concurrent Puts reach
// the lock, and are otherwise a pure function of the inputs.
type Store struct {
	b     Backend
	cfg   Config
	ns    namespace
	cut   Cutter
	codec *codec

	mu          sync.RWMutex
	closed      bool
	ref         Ref
	refSize     int
	packs       []packState
	locs        map[ChunkID]loc
	open        *PackWriter
	openEntries []IndexEntry
	ingest      Metrics
	// inflight holds the chunk ids each running Put has referenced; GC
	// marks them. recent holds manifests whose Put completed since the
	// last successful GC began; the next GC marks them too.
	inflight map[*putState]struct{}
	recent   map[ManifestID]struct{}
}

// putState is a running Put's set of referenced chunk ids. Only the Put's
// goroutine writes it, while holding s.mu (read or write); GC reads it
// under the write lock.
type putState struct {
	ids map[ChunkID]struct{}
}

// Open loads the store kept in namespace ns of b: the ref and every
// index it names (packs are read lazily). An empty namespace is an empty
// store.
//
// The namespace is mandatory and scopes the store to one tenant: every
// object lives under "t/<ns>/". Dedupe, the presence cache, Stats,
// Metrics, GC and Fsck never look outside it, so a tenant cannot learn
// (through timing, Stats or presence) whether another holds the same
// content; each namespace pays for its own chunks. ns is 1 to 64 bytes
// of [a-z0-9_-].
func Open(ctx context.Context, b Backend, ns string, cfg Config) (*Store, error) {
	nsv, err := newNamespace(ns)
	if err != nil {
		return nil, err
	}
	cut, err := NewCutter(cfg.Chunking)
	if err != nil {
		return nil, err
	}
	if cfg.TargetPackSize == 0 {
		cfg.TargetPackSize = DefaultTargetPackSize
	}
	if cfg.TargetPackSize < 0 {
		return nil, errf(CodeInvalidConfig, "", "TargetPackSize %d", cfg.TargetPackSize)
	}
	cfg.Chunking = cut.Config()
	c, err := newCodec()
	if err != nil {
		return nil, err
	}
	s := &Store{b: b, cfg: cfg, ns: nsv, cut: cut, codec: c, locs: map[ChunkID]loc{},
		inflight: map[*putState]struct{}{}, recent: map[ManifestID]struct{}{}}
	if err := s.load(ctx); err != nil {
		c.close()
		return nil, err
	}
	return s, nil
}

// backendErr keeps a chunkstore error as it is and wraps anything else
// (a backend's own error) as CodeBackend for name.
func backendErr(name string, err error) error {
	if CodeOf(err) != "" {
		return err
	}
	return wrapErr(CodeBackend, name, err)
}

// withObject returns a copy of a chunkstore error naming the object it
// concerns.
func withObject(err error, name string) error {
	var e *Error
	if errors.As(err, &e) {
		c := *e
		c.Object = name
		return &c
	}
	return err
}

// load reads the ref and every live pack's index (Open).
func (s *Store) load(ctx context.Context) error {
	data, err := s.b.Get(ctx, s.ns.ref())
	if errors.Is(err, ErrObjectNotFound) {
		return nil
	}
	if err != nil {
		return backendErr(s.ns.ref(), err)
	}
	ref, err := DecodeRef(data)
	if err != nil {
		return withObject(err, s.ns.ref())
	}
	s.ref, s.refSize = *ref, len(data)
	for _, id := range ref.Packs {
		name := s.ns.index(id)
		idata, err := s.b.Get(ctx, name)
		if errors.Is(err, ErrObjectNotFound) {
			return errf(CodeIndexMissing, name, "index of a live pack is missing")
		}
		if err != nil {
			return backendErr(name, err)
		}
		ix, err := DecodeIndex(idata)
		if err != nil {
			return withObject(err, name)
		}
		if ix.PackID != id {
			return errf(CodeIndexPackMismatch, name, "index describes pack %s", ix.PackID)
		}
		s.packs = append(s.packs, packState{id: id, idx: ix, idxSize: len(idata)})
	}
	s.rebuildLocs()
	return nil
}

// rebuildLocs recomputes the presence cache from the sealed packs (in
// ref order; the first pack holding a chunk wins) and the open pack.
func (s *Store) rebuildLocs() {
	n := len(s.openEntries)
	for _, p := range s.packs {
		n += len(p.idx.Entries)
	}
	s.locs = make(map[ChunkID]loc, n)
	for i, p := range s.packs {
		for _, e := range p.idx.Entries {
			if _, dup := s.locs[e.ID]; !dup {
				s.locs[e.ID] = loc{pack: int32(i), e: e}
			}
		}
	}
	for _, e := range s.openEntries {
		s.locs[e.ID] = loc{pack: -1, e: e}
	}
}

// now is created_unix_ns for a new pack: 0 without a Clock.
func (s *Store) now() int64 {
	if s.cfg.Clock == nil {
		return 0
	}
	return s.cfg.Clock.Now().UnixNano()
}

// Put chunks r, stores the chunks not yet present, stores the manifest
// and returns its id. Unless Config.DeferFlush is set, everything is
// durable when Put returns. On error nothing references the chunks
// already added; GC reclaims them.
//
// GC grace: the chunks of a running Put are never collected, and a
// manifest survives the first GC that starts after its Put returned even
// if that GC does not name it. Every later GC must name it (live or
// pinned) to keep it.
func (s *Store) Put(ctx context.Context, r io.Reader) (ManifestID, Stats, error) {
	var st Stats
	ps := &putState{ids: map[ChunkID]struct{}{}}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ManifestID{}, st, ErrClosed
	}
	s.inflight[ps] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.inflight, ps)
		s.mu.Unlock()
	}()
	ch := NewChunker(s.cut, r)
	m := Manifest{Chunking: s.cfg.Chunking}
	for {
		if err := ctx.Err(); err != nil {
			return ManifestID{}, st, err
		}
		c, err := ch.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ManifestID{}, st, err
		}
		if len(m.Chunks) >= MaxManifestChunks {
			return ManifestID{}, st, errf(CodeTooLarge, "", "input exceeds %d chunks", MaxManifestChunks)
		}
		id := SumChunk(c)
		m.Chunks = append(m.Chunks, id)
		m.TotalLen += uint64(len(c))
		st.Raw += uint64(len(c))
		added, stored, err := s.add(ctx, ps, id, c)
		if err != nil {
			return ManifestID{}, st, err
		}
		if added {
			st.NewChunks++
			st.NewRaw += uint64(len(c))
			st.Stored += stored
		} else {
			st.DedupChunks++
		}
	}
	mb := m.Encode()
	mid := ManifestID(SumChunk(mb))
	added, stored, err := s.add(ctx, ps, ChunkID(mid), mb)
	if err != nil {
		return ManifestID{}, st, err
	}
	st.NewManifest, st.Stored = added, st.Stored+stored
	if !s.cfg.DeferFlush {
		if err := s.Flush(ctx); err != nil {
			return ManifestID{}, st, err
		}
	}
	s.mu.Lock()
	s.recent[mid] = struct{}{}
	s.ingest.Puts++
	s.ingest.LogicalBytes += st.Raw
	s.ingest.NewChunks += uint64(st.NewChunks)
	s.ingest.DedupChunks += uint64(st.DedupChunks)
	s.ingest.NewRawBytes += st.NewRaw
	s.ingest.NewStoredBytes += st.Stored
	s.mu.Unlock()
	return mid, st, nil
}

// add stores one chunk unless present; it reports whether it was added
// and the record bytes it took.
func (s *Store) add(ctx context.Context, ps *putState, id ChunkID, data []byte) (bool, uint64, error) {
	s.mu.RLock()
	_, present := s.locs[id]
	closed := s.closed
	if present && !closed {
		ps.ids[id] = struct{}{}
	}
	s.mu.RUnlock()
	if closed {
		return false, 0, ErrClosed
	}
	if present {
		return false, 0, nil
	}
	codec, payload := s.codec.encode(data)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, 0, ErrClosed
	}
	ps.ids[id] = struct{}{}
	if _, present := s.locs[id]; present {
		return false, 0, nil
	}
	if s.open == nil {
		s.open = NewPackWriter(s.now())
	}
	off := s.open.Append(uint32(len(data)), codec, payload)
	e := IndexEntry{ID: id, Offset: off, CompLen: uint32(len(payload)), RawLen: uint32(len(data))}
	s.openEntries = append(s.openEntries, e)
	s.locs[id] = loc{pack: -1, e: e}
	if s.open.Size() >= s.cfg.TargetPackSize {
		if err := s.sealLocked(ctx); err != nil {
			return true, e.RecordSize(), err
		}
	}
	return true, e.RecordSize(), nil
}

// Flush seals the open pack: it writes the pack, then its index, then a
// ref naming it. A crash before the ref is replaced leaves an orphan pack
// that GC deletes; the store stays consistent either way.
func (s *Store) Flush(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	return s.sealLocked(ctx)
}

// writePack writes a finished pack and its index (not the ref).
func (s *Store) writePack(ctx context.Context, w *PackWriter, entries []IndexEntry) (packState, uint64, error) {
	data, id, crc := w.Finish()
	ix, err := NewIndex(id, uint64(len(data)), crc, entries)
	if err != nil {
		return packState{}, 0, err
	}
	idata := ix.Encode()
	if err := s.b.Put(ctx, s.ns.pack(id), data); err != nil {
		return packState{}, 0, backendErr(s.ns.pack(id), err)
	}
	if err := s.b.Put(ctx, s.ns.index(id), idata); err != nil {
		return packState{}, 0, backendErr(s.ns.index(id), err)
	}
	return packState{id: id, idx: ix, idxSize: len(idata)}, uint64(len(data)), nil
}

// writeRef replaces the ref: the commit point.
func (s *Store) writeRef(ctx context.Context, packs []PackID) (Ref, int, error) {
	slices.SortFunc(packs, func(a, b PackID) int { return bytes.Compare(a[:], b[:]) })
	r := Ref{Generation: s.ref.Generation + 1, Packs: packs}
	data, err := r.Encode()
	if err != nil {
		return Ref{}, 0, err
	}
	if err := s.b.Put(ctx, s.ns.ref(), data); err != nil {
		return Ref{}, 0, backendErr(s.ns.ref(), err)
	}
	return r, len(data), nil
}

// sealLocked writes the open pack, its index and a ref naming it (in that
// order, FORMAT.md §7) and moves its entries to the sealed packs. The
// caller holds the write lock.
func (s *Store) sealLocked(ctx context.Context) error {
	if s.open == nil || s.open.Count() == 0 {
		return nil
	}
	ps, _, err := s.writePack(ctx, s.open, s.openEntries)
	if err != nil {
		return err
	}
	if slices.Contains(s.ref.Packs, ps.id) {
		// Only possible if identical bytes were already committed.
		s.open, s.openEntries = nil, nil
		s.rebuildLocs()
		return nil
	}
	ref, size, err := s.writeRef(ctx, append(slices.Clone(s.ref.Packs), ps.id))
	if err != nil {
		return err
	}
	s.ref, s.refSize = ref, size
	s.packs = append(s.packs, ps)
	i := int32(len(s.packs) - 1)
	for _, e := range ps.idx.Entries {
		s.locs[e.ID] = loc{pack: i, e: e}
	}
	s.open, s.openEntries = nil, nil
	return nil
}

// fetchLocked fetches and verifies one chunk; the caller holds s.mu (read
// or write). missing is the code reported when the id is absent.
func (s *Store) fetchLocked(ctx context.Context, id ChunkID, missing Code) (IndexEntry, []byte, error) {
	if s.closed {
		return IndexEntry{}, nil, ErrClosed
	}
	l, ok := s.locs[id]
	if !ok {
		return IndexEntry{}, nil, errf(missing, id.String(), "not in the store")
	}
	if l.pack < 0 {
		return l.e, slices.Clone(s.open.record(l.e.Offset, l.e.CompLen)), nil
	}
	name := s.ns.pack(s.packs[l.pack].id)
	rec, err := s.b.GetRange(ctx, name, int64(l.e.Offset), int(l.e.RecordSize()))
	if errors.Is(err, ErrObjectNotFound) {
		return l.e, nil, errf(CodePackMissing, name, "pack of chunk %s is missing", id)
	}
	if err != nil {
		return l.e, nil, backendErr(name, err)
	}
	return l.e, rec, nil
}

// readChunk fetches a chunk under the read lock and verifies it outside.
func (s *Store) readChunk(ctx context.Context, id ChunkID, missing Code) ([]byte, error) {
	s.mu.RLock()
	e, rec, err := s.fetchLocked(ctx, id, missing)
	s.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	return s.codec.openRecord(e, rec)
}

// readChunkLocked is readChunk for a caller that already holds s.mu.
func (s *Store) readChunkLocked(ctx context.Context, id ChunkID, missing Code) ([]byte, error) {
	e, rec, err := s.fetchLocked(ctx, id, missing)
	if err != nil {
		return nil, err
	}
	return s.codec.openRecord(e, rec)
}

// decodeManifestChunk decodes a manifest record, naming id in errors.
func decodeManifestChunk(id ManifestID, data []byte) (*Manifest, error) {
	m, err := DecodeManifest(data)
	if err != nil {
		return nil, withObject(err, id.String())
	}
	return m, nil
}

// Get returns a reader of the blob. The manifest is read and verified
// first, and every chunk must be present with lengths summing to the
// manifest's total. Each chunk is then fetched on demand and verified
// (record lengths against the index, the record CRC32C, decompression to
// raw_len, SHA-256 against the id) before any of its bytes are returned:
// a reader never yields bytes that differ from what was Put, it fails
// instead. A blob not named live in a concurrent GC may vanish mid-read
// (CHUNK_MISSING).
func (s *Store) Get(ctx context.Context, id ManifestID) (io.ReadCloser, error) {
	mb, err := s.readChunk(ctx, ChunkID(id), CodeManifestNotFound)
	if err != nil {
		return nil, err
	}
	m, err := decodeManifestChunk(id, mb)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	var sum uint64
	for _, c := range m.Chunks {
		l, ok := s.locs[c]
		if !ok {
			s.mu.RUnlock()
			return nil, errf(CodeChunkMissing, c.String(), "referenced by manifest %s", id)
		}
		sum += uint64(l.e.RawLen)
	}
	s.mu.RUnlock()
	if sum != m.TotalLen {
		return nil, errf(CodeManifestLengthMismatch, id.String(), "chunks hold %d bytes, manifest says %d", sum, m.TotalLen)
	}
	return &blobReader{ctx: ctx, s: s, m: m}, nil
}

// blobReader streams a blob chunk by chunk; it is not safe for concurrent
// use. An error is sticky, and after Close every Read returns ErrClosed.
type blobReader struct {
	ctx  context.Context
	s    *Store
	m    *Manifest
	next int
	cur  []byte
	err  error
}

func (r *blobReader) Read(p []byte) (int, error) {
	for len(r.cur) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if r.next == len(r.m.Chunks) {
			r.err = io.EOF
			return 0, io.EOF
		}
		c, err := r.s.readChunk(r.ctx, r.m.Chunks[r.next], CodeChunkMissing)
		if err != nil {
			r.err = err
			return 0, err
		}
		r.next++
		r.cur = c
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	return n, nil
}

func (r *blobReader) Close() error {
	r.cur, r.err = nil, ErrClosed
	return nil
}

// Stat describes a stored blob without reading its chunks.
func (s *Store) Stat(ctx context.Context, id ManifestID) (ManifestInfo, error) {
	mb, err := s.readChunk(ctx, ChunkID(id), CodeManifestNotFound)
	if err != nil {
		return ManifestInfo{}, err
	}
	m, err := decodeManifestChunk(id, mb)
	if err != nil {
		return ManifestInfo{}, err
	}
	info := ManifestInfo{ID: id, Size: m.TotalLen, Chunks: len(m.Chunks), Chunking: m.Chunking}
	seen := make(map[ChunkID]struct{}, len(m.Chunks))
	s.mu.RLock()
	defer s.mu.RUnlock()
	if l, ok := s.locs[ChunkID(id)]; ok {
		info.StoredBytes += l.e.RecordSize()
	}
	for _, c := range m.Chunks {
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		l, ok := s.locs[c]
		if !ok {
			return ManifestInfo{}, errf(CodeChunkMissing, c.String(), "referenced by manifest %s", id)
		}
		info.UniqueChunks++
		info.StoredBytes += l.e.RecordSize()
	}
	return info, nil
}

// Has reports whether a chunk (or manifest, as ChunkID(id)) is present,
// sealed or in the open pack: the presence cache.
func (s *Store) Has(id ChunkID) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.locs[id]
	return ok
}

// Close seals the open pack and releases the codec. If sealing fails the
// store stays open and the error is returned.
func (s *Store) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if err := s.sealLocked(ctx); err != nil {
		return err
	}
	s.closed = true
	s.codec.close()
	return nil
}

// Metrics are a Store's counters, as values. Ingest fields count the Puts
// made through this Store value since Open; content fields describe what
// the backend holds now.
type Metrics struct {
	// Ingest.
	Puts           uint64
	LogicalBytes   uint64 // input bytes of all Puts
	NewChunks      uint64
	DedupChunks    uint64
	NewRawBytes    uint64 // uncompressed bytes of new data chunks
	NewStoredBytes uint64 // record bytes added (data and manifests)
	// Content.
	Chunks          uint64 // distinct chunks, manifests included
	RawBytes        uint64 // their uncompressed bytes
	CompressedBytes uint64 // their record bytes (16-byte header + payload)
	Packs           int
	ObjectBytes     uint64 // packs + indexes + ref
}

// DedupeRatio is logical input bytes per new unique raw byte (≥ 1; 0
// when nothing was put).
func (m Metrics) DedupeRatio() float64 {
	if m.NewRawBytes == 0 {
		return 0
	}
	return float64(m.LogicalBytes) / float64(m.NewRawBytes)
}

// CompressionRatio is the stored chunks' raw bytes per record byte.
func (m Metrics) CompressionRatio() float64 {
	if m.CompressedBytes == 0 {
		return 0
	}
	return float64(m.RawBytes) / float64(m.CompressedBytes)
}

// StoredRatio is backend bytes per logical input byte.
func (m Metrics) StoredRatio() float64 {
	if m.LogicalBytes == 0 {
		return 0
	}
	return float64(m.ObjectBytes) / float64(m.LogicalBytes)
}

// Metrics returns the current counters.
func (s *Store) Metrics() Metrics {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := s.ingest
	m.Packs = len(s.packs)
	m.ObjectBytes = uint64(s.refSize)
	for _, l := range s.locs {
		m.Chunks++
		m.RawBytes += uint64(l.e.RawLen)
		m.CompressedBytes += l.e.RecordSize()
	}
	for _, p := range s.packs {
		m.ObjectBytes += p.idx.PackSize + uint64(p.idxSize)
	}
	return m
}

// Chunking returns the canonical configuration of the chunker new Puts use.
func (s *Store) Chunking() ChunkerConfig { return s.cfg.Chunking }

// Namespace returns the tenant namespace the store is scoped to.
func (s *Store) Namespace() string { return string(s.ns) }
