package textindex

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// Config configures an Index.
type Config struct {
	// Schema declares the fields (required).
	Schema Schema
	// Dir stores segments, tombstones and the commit point. Nil means a
	// fresh MemDirectory.
	Dir Directory
	// TimeField is the numeric field used by since:/until: and as the
	// recency tie-breaker. "" means "at" when the schema has a numeric
	// field of that name; "-" disables both.
	TimeField string
	// MergeFactor is the size-tiered merge factor: when MergeFactor
	// segments share a size tier they are merged (default 10, minimum 2).
	MergeFactor int
	// MaxRegexCandidates caps the documents that a regex without usable
	// trigrams may be verified against (default 10,000).
	MaxRegexCandidates int
	// DisableAutoMerge stops Refresh from running the merge policy; call
	// Merge or ForceMerge explicitly.
	DisableAutoMerge bool

	// Engine selects the implementation NewSearcher returns.
	Engine Engine
	// Codec encodes postings of new segments (nil: VarintCodec). Segments
	// record their codec, so indexes may mix codecs.
	Codec PostingCodec
	// Intersector intersects doc lists (nil: SkipIntersector).
	Intersector Intersector
	// Scorer ranks matches (nil: BM25Scorer).
	Scorer Scorer
	// Regex chooses regex candidate generation (nil: TrigramPrefilter).
	Regex RegexStrategy
}

// Engine selects a Searcher implementation.
type Engine uint8

const (
	// EngineSegmented is Index: segments, postings, trigram index.
	EngineSegmented Engine = iota
	// EngineScan is ScanIndex: brute-force evaluation over an in-memory
	// document map, for tiny corpora and as the test oracle.
	EngineScan
)

// Searcher is the API shared by Index and ScanIndex.
// Both engines return the same matches and scores for the same writes
// (TIDX-INV-01); the methods are documented on Index.
type Searcher interface {
	Add(Document) error                                       // buffer a new document
	Update(Document) error                                    // buffer a replacement
	Delete(id uint64) error                                   // buffer a deletion
	Refresh() error                                           // make buffered writes visible
	Search(q string, opts SearchOptions) (Result, error)      // parse and run q
	SearchQuery(q *Query, opts SearchOptions) (Result, error) // run a parsed query
	Get(id uint64) (Document, bool, error)                    // stored document
	Close() error
}

// NewSearcher opens the engine cfg.Engine selects.
func NewSearcher(cfg Config) (Searcher, error) {
	if cfg.Engine == EngineScan {
		return NewScanIndex(cfg)
	}
	return Open(cfg)
}

// resolveTimeField applies the Config.TimeField rules.
func resolveTimeField(s Schema, name string) (string, int, error) {
	switch name {
	case "-":
		return "", -1, nil
	case "":
		if t, i := s.lookup("at"); t == fieldNumeric {
			return "at", i, nil
		}
		return "", -1, nil
	}
	t, i := s.lookup(name)
	if t != fieldNumeric {
		return "", -1, fmt.Errorf("%w: time field %q is not a numeric field", ErrInvalidConfig, name)
	}
	return name, i, nil
}

// Index is a segmented inverted + trigram index. Writes (Add, Update,
// Delete) are buffered and become visible, durable and searchable
// together at Refresh. Searches read an immutable snapshot and may run
// concurrently with each other and with writes. Methods are safe for
// concurrent use.
type Index struct {
	mu        sync.Mutex
	cfg       Config
	schema    Schema
	dir       Directory
	codec     *codec
	timeName  string
	timeF     int
	segs      []*segView
	pending   map[uint64]pendingDoc
	live      map[uint64]bool
	gen       uint64
	nextSeg   uint64
	commitGen uint64
	snap      atomic.Pointer[[]*segView]
	closed    atomic.Bool
	pc        PostingCodec
	x         execParams
}

// pendingDoc is a buffered write with its generation.
type pendingDoc struct {
	gen uint64
	doc *docFields // nil: delete
}

// Open opens the index stored in cfg.Dir (or creates an empty one). It
// validates the commit point, every referenced segment and tombstone file,
// and removes unreferenced index files left by an interrupted write.
func Open(cfg Config) (*Index, error) {
	if err := cfg.Schema.Validate(); err != nil {
		return nil, err
	}
	if cfg.Dir == nil {
		cfg.Dir = NewMemDirectory()
	}
	if cfg.MergeFactor == 0 {
		cfg.MergeFactor = 10
	}
	if cfg.MergeFactor < 2 {
		return nil, fmt.Errorf("%w: merge factor %d", ErrInvalidConfig, cfg.MergeFactor)
	}
	if cfg.MaxRegexCandidates == 0 {
		cfg.MaxRegexCandidates = 10_000
	}
	if cfg.MaxRegexCandidates < 0 {
		return nil, fmt.Errorf("%w: MaxRegexCandidates %d", ErrInvalidConfig, cfg.MaxRegexCandidates)
	}
	ix := &Index{cfg: cfg, schema: cfg.Schema, dir: cfg.Dir, timeF: -1,
		pending: map[uint64]pendingDoc{}, live: map[uint64]bool{}}
	var err error
	if ix.timeName, ix.timeF, err = resolveTimeField(cfg.Schema, cfg.TimeField); err != nil {
		return nil, err
	}
	if ix.pc, err = checkCodec(cfg.Codec); err != nil {
		return nil, err
	}
	ix.x.scorer, ix.x.isect, ix.x.regex = defaultsFor(cfg)
	ix.x.nt, ix.x.timeF, ix.x.regexLimit = len(cfg.Schema.Text), ix.timeF, cfg.MaxRegexCandidates
	c, err := newCodec()
	if err != nil {
		return nil, err
	}
	ix.codec = c
	if err := ix.load(); err != nil {
		c.close()
		return nil, err
	}
	ix.publish()
	return ix, nil
}

func (ix *Index) load() error {
	b, err := ix.dir.ReadFile(CommitFile)
	if errors.Is(err, fs.ErrNotExist) {
		return ix.collectGarbage()
	}
	if err != nil {
		return err
	}
	cp, err := decodeCommit(b)
	if err != nil {
		return err
	}
	for _, cs := range cp.segs {
		num, ok := parseSegFileName(cs.name)
		if !ok || num >= cp.nextSeg {
			return corruptf("commit point names segment %q", cs.name)
		}
		data, err := ix.dir.ReadFile(cs.name)
		if err != nil {
			return err
		}
		seg, err := openSegment(data, ix.schema, ix.codec)
		if err != nil {
			return fmt.Errorf("%s: %w", cs.name, err)
		}
		if seg.fileCRC != cs.segCRC || seg.n != int(cs.docCount) || seg.maxGen > cp.maxGen {
			return corruptf("segment %s does not match the commit point", cs.name)
		}
		var del bitmap
		if cs.delGen > 0 {
			tb, err := ix.dir.ReadFile(tombFileName(cs.name, cs.delGen))
			if err != nil {
				return err
			}
			if del, err = decodeTombstones(tb, seg.n, seg.fileCRC, cs.delGen); err != nil {
				return err
			}
			if popcount(del) != int(cs.delCount) {
				return corruptf("tombstone count of %s", cs.name)
			}
		}
		v := newSegView(cs.name, seg, del, cs.delGen)
		for d, id := range seg.ids {
			if !v.deleted(uint32(d)) {
				if ix.live[id] {
					return corruptf("document %d is live in two segments", id)
				}
				ix.live[id] = true
			}
		}
		ix.segs = append(ix.segs, v)
	}
	ix.gen, ix.nextSeg, ix.commitGen = cp.maxGen, cp.nextSeg, cp.gen
	return ix.collectGarbage()
}

// collectGarbage removes index files the current commit does not use.
func (ix *Index) collectGarbage() error {
	names, err := ix.dir.List()
	if err != nil {
		return err
	}
	keep := map[string]bool{CommitFile: true}
	for _, v := range ix.segs {
		keep[v.name] = true
		if v.delGen > 0 {
			keep[tombFileName(v.name, v.delGen)] = true
		}
	}
	for _, n := range names {
		if keep[n] || !strings.HasPrefix(n, "seg_") || !(strings.HasSuffix(n, ".fftx") || strings.HasSuffix(n, ".fftd")) {
			continue
		}
		if err := ix.dir.Remove(n); err != nil {
			return err
		}
	}
	return nil
}

func (ix *Index) publish() {
	views := slices.Clone(ix.segs)
	ix.snap.Store(&views)
}

func (ix *Index) views() []*segView { return *ix.snap.Load() }

// Close releases the index. Further calls fail with ErrClosed; a search
// running concurrently with Close may fail with ErrCorrupt (its stored
// fields can no longer be decompressed).
func (ix *Index) Close() error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.closed.Swap(true) {
		return ErrClosed
	}
	ix.codec.close()
	return nil
}

// Add buffers a new document. It fails with ErrDuplicateID if the id is
// live (committed or pending); use Update to replace.
func (ix *Index) Add(d Document) error {
	return ix.write(d, false)
}

// Update buffers a replacement: delete of the current version (if any)
// plus add, with a newer generation.
func (ix *Index) Update(d Document) error {
	return ix.write(d, true)
}

// write buffers d with the next generation (Add when !replace, Update
// otherwise).
func (ix *Index) write(d Document, replace bool) error {
	f, err := normalize(ix.schema, d)
	if err != nil {
		return err
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.closed.Load() {
		return ErrClosed
	}
	if !replace && ix.live[d.ID] {
		return fmt.Errorf("%w: %d", ErrDuplicateID, d.ID)
	}
	ix.gen++
	f.gen = ix.gen
	ix.pending[d.ID] = pendingDoc{gen: ix.gen, doc: &f}
	ix.live[d.ID] = true
	return nil
}

// Delete buffers the deletion of id. Deleting an absent id is a no-op.
func (ix *Index) Delete(id uint64) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.closed.Load() {
		return ErrClosed
	}
	if !ix.live[id] {
		return nil
	}
	ix.gen++
	ix.pending[id] = pendingDoc{gen: ix.gen}
	delete(ix.live, id)
	return nil
}

// Refresh applies the buffered writes: superseded and deleted versions
// are tombstoned, buffered documents are written as one new segment, and
// the commit point is swapped atomically. Then, unless disabled, the
// merge policy runs.
func (ix *Index) Refresh() error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.closed.Load() {
		return ErrClosed
	}
	if err := ix.refreshLocked(); err != nil {
		return err
	}
	if !ix.cfg.DisableAutoMerge {
		return ix.mergeLocked(false, 0)
	}
	return nil
}

func (ix *Index) refreshLocked() error {
	if len(ix.pending) == 0 {
		return nil
	}
	ids := make([]uint64, 0, len(ix.pending))
	for id := range ix.pending {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	// Every pending id, add or delete, supersedes its committed version: an
	// Add after a Delete of the same id overwrites the pending delete, so
	// the tombstone must not depend on the pending entry's kind.
	touched := map[int]bitmap{}
	for _, id := range ids {
		for si, v := range ix.segs {
			d, ok := v.seg.findID(id)
			if !ok {
				continue
			}
			bm, ok := touched[si]
			if !ok {
				if v.del != nil {
					bm = v.del.clone()
				} else {
					bm = newBitmap(v.seg.n)
				}
			}
			if !bm.get(d) { // at most one segment holds the live version
				bm.set(d)
				touched[si] = bm
				break
			}
		}
	}
	var docs []docFields
	for _, id := range ids {
		if p := ix.pending[id]; p.doc != nil {
			docs = append(docs, *p.doc)
		}
	}
	var next []*segView
	var obsolete, written []string
	fail := func(err error) error {
		for _, n := range written {
			_ = ix.dir.Remove(n)
		}
		return err
	}
	for si, v := range ix.segs {
		bm, ok := touched[si]
		if !ok {
			next = append(next, v)
			continue
		}
		if v.delGen > 0 {
			obsolete = append(obsolete, tombFileName(v.name, v.delGen))
		}
		if popcount(bm) == v.seg.n {
			obsolete = append(obsolete, v.name)
			continue
		}
		g := v.delGen + 1
		tn := tombFileName(v.name, g)
		if err := ix.dir.WriteFile(tn, encodeTombstones(v.seg.n, v.seg.fileCRC, g, bm)); err != nil {
			return fail(err)
		}
		written = append(written, tn)
		next = append(next, newSegView(v.name, v.seg, bm, g))
	}
	nextSeg := ix.nextSeg
	if len(docs) > 0 {
		v, err := ix.writeSegment(docs, nextSeg)
		if err != nil {
			return fail(err)
		}
		written = append(written, v.name)
		nextSeg++
		next = append(next, v)
	}
	// The commit point is written last: until it is swapped, the files
	// written above are unreferenced and a crash leaves the old state.
	if err := ix.commit(next, nextSeg); err != nil {
		return fail(err)
	}
	ix.pending = map[uint64]pendingDoc{}
	for _, n := range obsolete {
		_ = ix.dir.Remove(n)
	}
	return nil
}

func (ix *Index) writeSegment(docs []docFields, num uint64) (*segView, error) {
	data, err := encodeSegment(ix.schema, docs, ix.codec, ix.pc)
	if err != nil {
		return nil, err
	}
	seg, err := openSegment(data, ix.schema, ix.codec)
	if err != nil {
		return nil, fmt.Errorf("textindex: self-check of new segment: %w", err)
	}
	name := segFileName(num)
	if err := ix.dir.WriteFile(name, data); err != nil {
		return nil, err
	}
	return newSegView(name, seg, nil, 0), nil
}

// commit writes the commit point for segs and installs them.
func (ix *Index) commit(segs []*segView, nextSeg uint64) error {
	cp := &commitPoint{gen: ix.commitGen + 1, maxGen: ix.gen, nextSeg: nextSeg}
	for _, v := range segs {
		cp.segs = append(cp.segs, commitSeg{name: v.name, docCount: uint32(v.seg.n), segCRC: v.seg.fileCRC,
			delGen: v.delGen, delCount: uint32(v.delCount)})
	}
	if err := ix.dir.WriteFile(CommitFile, encodeCommit(cp)); err != nil {
		return err
	}
	ix.segs, ix.nextSeg, ix.commitGen = segs, nextSeg, cp.gen
	ix.publish()
	return nil
}

// Merge runs the size-tiered merge policy until no tier holds MergeFactor
// segments.
func (ix *Index) Merge() error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.closed.Load() {
		return ErrClosed
	}
	return ix.mergeLocked(false, 0)
}

// ForceMerge merges the smallest segments until at most maxSegments
// remain (minimum 1). Tombstoned documents are dropped.
func (ix *Index) ForceMerge(maxSegments int) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.closed.Load() {
		return ErrClosed
	}
	return ix.mergeLocked(true, max(maxSegments, 1))
}

// tier is the size tier of a segment with n live docs:
// floor(log_factor(n)), 0 for n < factor.
func tier(n, factor int) int {
	t := 0
	for ; n >= factor; n /= factor {
		t++
	}
	return t
}

func (ix *Index) mergeLocked(force bool, maxSegs int) error {
	for {
		var pick []int
		if force {
			if len(ix.segs) <= maxSegs {
				return nil
			}
			order := make([]int, len(ix.segs))
			for i := range order {
				order[i] = i
			}
			slices.SortStableFunc(order, func(a, b int) int { return ix.segs[a].live() - ix.segs[b].live() })
			pick = order[:len(ix.segs)-maxSegs+1]
			slices.Sort(pick)
		} else {
			pick = ix.policy()
			if pick == nil {
				return nil
			}
		}
		if err := ix.mergeSegs(pick); err != nil {
			return err
		}
	}
}

// policy returns the first MergeFactor segments (commit order) of the
// lowest tier that has at least MergeFactor segments.
func (ix *Index) policy() []int {
	f := ix.cfg.MergeFactor
	byTier := map[int][]int{}
	maxT := 0
	for i, v := range ix.segs {
		t := tier(v.live(), f)
		byTier[t] = append(byTier[t], i)
		maxT = max(maxT, t)
	}
	for t := 0; t <= maxT; t++ {
		if len(byTier[t]) >= f {
			return byTier[t][:f]
		}
	}
	return nil
}

func (ix *Index) mergeSegs(pick []int) error {
	var docs []docFields
	var obsolete []string
	for _, si := range pick {
		v := ix.segs[si]
		var cache storedCache
		for d := 0; d < v.seg.n; d++ {
			if v.deleted(uint32(d)) {
				continue
			}
			f, err := v.seg.docFieldsAt(uint32(d), &cache)
			if err != nil {
				return err
			}
			docs = append(docs, f)
		}
		obsolete = append(obsolete, v.name)
		if v.delGen > 0 {
			obsolete = append(obsolete, tombFileName(v.name, v.delGen))
		}
	}
	var next []*segView
	for i, v := range ix.segs {
		if !slices.Contains(pick, i) {
			next = append(next, v)
		}
	}
	nextSeg := ix.nextSeg
	written := ""
	if len(docs) > 0 {
		v, err := ix.writeSegment(docs, nextSeg)
		if err != nil {
			return err
		}
		nextSeg++
		next = append(next, v)
		written = v.name
	}
	if err := ix.commit(next, nextSeg); err != nil {
		// Remove only the segment this merge wrote. When every picked
		// document was deleted nothing was written, and seg_<nextSeg-1> is
		// a committed segment that the unchanged commit point still names.
		if written != "" {
			_ = ix.dir.Remove(written)
		}
		return err
	}
	for _, n := range obsolete {
		_ = ix.dir.Remove(n)
	}
	return nil
}

// Search parses and runs q against the last refreshed snapshot.
func (ix *Index) Search(q string, opts SearchOptions) (Result, error) {
	pq, err := Parse(q)
	if err != nil {
		return Result{}, err
	}
	return ix.SearchQuery(pq, opts)
}

// SearchQuery runs a parsed query against the last refreshed snapshot.
func (ix *Index) SearchQuery(q *Query, opts SearchOptions) (Result, error) {
	if ix.closed.Load() {
		return Result{}, ErrClosed
	}
	p, err := lowerQuery(q, ix.schema, ix.timeName, opts.Now)
	if err != nil {
		return Result{}, err
	}
	x := ix.x
	x.k = opts.K
	return execute(ix.views(), p, x)
}

// Get returns the stored document id from the last refreshed snapshot.
func (ix *Index) Get(id uint64) (Document, bool, error) {
	if ix.closed.Load() {
		return Document{}, false, ErrClosed
	}
	for _, v := range ix.views() {
		if d, ok := v.seg.findID(id); ok && !v.deleted(d) {
			f, err := v.seg.docFieldsAt(d, nil)
			if err != nil {
				return Document{}, false, err
			}
			return f.document(ix.schema), true, nil
		}
	}
	return Document{}, false, nil
}

// Stats describes the index.
type Stats struct {
	Segments int    // segments in the snapshot
	Live     int    // live documents in the snapshot
	Deleted  int    // tombstoned documents not yet merged away
	Bytes    int    // total segment bytes
	Pending  int    // buffered operations not yet refreshed
	MaxGen   uint64 // last generation assigned
}

// Stats returns a description of the current snapshot and buffer.
func (ix *Index) Stats() Stats {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	s := Stats{Pending: len(ix.pending), MaxGen: ix.gen}
	for _, v := range ix.segs {
		s.Segments++
		s.Live += v.live()
		s.Deleted += v.delCount
		s.Bytes += v.seg.Size()
	}
	return s
}

// SegmentNames lists the segment files of the snapshot in commit order.
func (ix *Index) SegmentNames() []string {
	var out []string
	for _, v := range ix.views() {
		out = append(out, v.name)
	}
	return out
}
