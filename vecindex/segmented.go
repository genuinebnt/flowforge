package vecindex

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/genuinebnt/flowforge/core"
)

// Segmented-index defaults (spec 22.3, 22.7).
const (
	// DefaultFlushThreshold is the memtable size at which it is flushed to
	// a segment; below it the memtable is searched by brute force.
	DefaultFlushThreshold = 50_000
	// DefaultMergeTombstoneRatio triggers a merge of a segment whose
	// tombstones exceed this fraction of its vectors.
	DefaultMergeTombstoneRatio = 0.2
	// DefaultMaxSegments triggers a merge when the live segment count
	// exceeds it.
	DefaultMaxSegments = 8
	// DefaultExactFallbackRatio is the filter selectivity below which a
	// search scans the matching ids exactly ("fewer than 1% of points").
	DefaultExactFallbackRatio = 0.01

	tombstoneOverFetch = 2 // k * 2 to compensate for tombstones
	filterOverFetch    = 4 // k * 4 when a filter is present
)

// Config configures a Segmented index.
type Config struct {
	// Dir holds the index's segment files. It is created if missing. One
	// directory per index: erasure of the index is deleting it.
	Dir string
	// Dim and Metric are fixed for the index's lifetime.
	Dim    int
	Metric Metric
	// ModelID and ModelVersion are echoed in IndexInfo.
	ModelID, ModelVersion string
	// FlushThreshold is the memtable size that triggers a flush; zero
	// means 50,000.
	FlushThreshold int
	// Graph configures Vamana construction of every segment.
	Graph VamanaParams
	// PQ configures the quantiser of every segment; PQ.M zero means
	// max(1, Dim/8).
	PQ PQConfig
	// MergeTombstoneRatio, MaxSegments: merge triggers; zero means 0.2 and
	// 8.
	MergeTombstoneRatio float64
	MaxSegments         int
	// DisableAutoMerge leaves merging to explicit Merge calls.
	DisableAutoMerge bool
	// ExactFallbackRatio is the filter selectivity under which a filter
	// with MatchingIDs is answered by an exact scan; zero means 0.01.
	ExactFallbackRatio float64
	// Seed drives every random choice of segment builds (graph init,
	// insertion order, PQ training), so an index fed the same writes builds
	// byte-identical segments.
	Seed uint64
}

func (c Config) withDefaults() (Config, error) {
	if c.Dir == "" {
		return c, fmt.Errorf("%w: empty Dir", ErrInvalidConfig)
	}
	if !c.Metric.Valid() {
		return c, fmt.Errorf("%w: %v", ErrInvalidMetric, c.Metric)
	}
	if c.Dim < 1 || c.Dim > MaxDimension {
		return c, fmt.Errorf("%w: dimension %d outside 1..%d", ErrInvalidConfig, c.Dim, MaxDimension)
	}
	if c.FlushThreshold == 0 {
		c.FlushThreshold = DefaultFlushThreshold
	}
	if c.MergeTombstoneRatio == 0 {
		c.MergeTombstoneRatio = DefaultMergeTombstoneRatio
	}
	if c.MaxSegments == 0 {
		c.MaxSegments = DefaultMaxSegments
	}
	if c.ExactFallbackRatio == 0 {
		c.ExactFallbackRatio = DefaultExactFallbackRatio
	}
	if c.PQ.M == 0 {
		c.PQ.M = max(1, c.Dim/8)
	}
	switch {
	case c.FlushThreshold < 1:
		return c, fmt.Errorf("%w: flush threshold %d", ErrInvalidConfig, c.FlushThreshold)
	case c.MaxSegments < 1:
		return c, fmt.Errorf("%w: max segments %d", ErrInvalidConfig, c.MaxSegments)
	case c.MergeTombstoneRatio < 0 || c.MergeTombstoneRatio > 1 || math.IsNaN(c.MergeTombstoneRatio):
		return c, fmt.Errorf("%w: merge tombstone ratio %v", ErrInvalidConfig, c.MergeTombstoneRatio)
	case c.ExactFallbackRatio < 0 || math.IsNaN(c.ExactFallbackRatio):
		return c, fmt.Errorf("%w: exact fallback ratio %v", ErrInvalidConfig, c.ExactFallbackRatio)
	case c.PQ.M < 1 || c.PQ.M > c.Dim:
		return c, fmt.Errorf("%w: pq m=%d outside 1..%d", ErrInvalidConfig, c.PQ.M, c.Dim)
	}
	if _, err := c.Graph.withDefaults(); err != nil {
		return c, err
	}
	return c, nil
}

// Segmented is the index architecture of spec 22.3: a brute-force memtable
// for recent writes, immutable DiskANN segments on disk, a tombstone set
// applied at query time, and merges that physically drop deleted vectors.
//
// Flushes and merges run synchronously inside the Upsert or Delete that
// triggers them, under the index's write lock (the spec's background
// builder is a deployment concern: a service can call Flush and Merge from
// its own goroutine with DisableAutoMerge and a large FlushThreshold).
// Searches take a read lock and fan out over the memtable and every segment
// in parallel. Segmented is safe for concurrent use.
type Segmented struct {
	mu        sync.RWMutex
	cfg       Config
	mem       *memtable
	segs      []*liveSegment
	ver       map[core.UUID]version
	live      int
	watermark uint64
	epoch     uint64 // bumped by every flush and merge
	nextFile  uint64
	merges    int
	closed    bool
}

var _ Index = (*Segmented)(nil)

// version is the newest write applied for an id and, when live, where its
// vector is.
type version struct {
	seq     uint64
	deleted bool
	seg     *liveSegment // nil: in the memtable
	node    uint32
}

// liveSegment is an open segment plus its tombstone bitset.
type liveSegment struct {
	seg       *Segment
	path      string
	dead      []uint64
	deadCount int
}

func (ls *liveSegment) isDead(i uint32) bool { return ls.dead[i/64]&(1<<(i%64)) != 0 }

func (ls *liveSegment) kill(i uint32) {
	if !ls.isDead(i) {
		ls.dead[i/64] |= 1 << (i % 64)
		ls.deadCount++
	}
}

func (ls *liveSegment) liveCount() int { return ls.seg.Len() - ls.deadCount }

// NewSegmented creates an empty index writing segments under cfg.Dir.
// Existing files in Dir are not loaded: the index is derived from
// graph.embeddings (the system of record) and is rebuilt by replaying it.
func NewSegmented(cfg Config) (*Segmented, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("vecindex: create index dir: %w", err)
	}
	return &Segmented{cfg: cfg, mem: newMemtable(cfg.Metric, cfg.Dim), ver: make(map[core.UUID]version)}, nil
}

// Upsert implements Index. When it triggers a flush or merge that fails,
// the write itself stays applied (in the memtable) and the error is
// returned.
func (s *Segmented) Upsert(id core.UUID, v []float32, seq uint64) error {
	pv, err := prepareVector(s.cfg.Metric, s.cfg.Dim, v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	old, ok := s.ver[id]
	if ok && seq <= old.seq {
		return nil // stale or replayed
	}
	if ok && !old.deleted {
		if old.seg != nil {
			old.seg.kill(old.node) // superseded: tombstone the old vector
		}
	} else {
		s.live++
	}
	s.mem.put(id, pv, seq)
	s.ver[id] = version{seq: seq}
	s.watermark = max(s.watermark, seq)
	if s.mem.len() >= s.cfg.FlushThreshold {
		if err := s.flushLocked(); err != nil {
			return err
		}
	}
	return s.autoMergeLocked()
}

// Delete implements Index. A delete of an unknown id is recorded, so a
// stale upsert delivered later cannot resurrect it.
func (s *Segmented) Delete(id core.UUID, seq uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	old, ok := s.ver[id]
	if ok && seq <= old.seq {
		return nil
	}
	if ok && !old.deleted {
		if old.seg != nil {
			old.seg.kill(old.node)
		} else {
			s.mem.remove(id)
		}
		s.live--
	}
	s.ver[id] = version{seq: seq, deleted: true}
	s.watermark = max(s.watermark, seq)
	return s.autoMergeLocked()
}

// Flush writes the memtable to a new segment.
func (s *Segmented) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	return s.flushLocked()
}

// Merge rewrites every segment into one, physically dropping every
// tombstoned vector (the forced merge of a tenant erasure, spec 22.10).
func (s *Segmented) Merge() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if len(s.segs) == 0 {
		return nil
	}
	return s.mergeLocked(slices.Clone(s.segs))
}

func (s *Segmented) flushLocked() error {
	n := s.mem.len()
	if n == 0 {
		return nil
	}
	ids := slices.Clone(s.mem.ids)
	seqs := slices.Clone(s.mem.seqs)
	flat := slices.Clone(s.mem.vecs)
	ls, err := s.writeSegment(ids, seqs, flat)
	if err != nil {
		return err
	}
	for i, id := range ids {
		s.ver[id] = version{seq: seqs[i], seg: ls, node: uint32(i)}
	}
	s.segs = append(s.segs, ls)
	s.mem.reset()
	return nil
}

// writeSegment builds, writes, syncs and opens a new segment file.
func (s *Segmented) writeSegment(ids []core.UUID, seqs []uint64, flat []float32) (*liveSegment, error) {
	s.epoch++
	path := filepath.Join(s.cfg.Dir, fmt.Sprintf("seg-%08d.ffda", s.nextFile))
	s.nextFile++
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("vecindex: create segment: %w", err)
	}
	rng := core.NewSplitMix64(s.cfg.Seed ^ (s.epoch * 0x9e3779b97f4a7c15))
	cfg := SegmentConfig{Metric: s.cfg.Metric, Graph: s.cfg.Graph, PQ: s.cfg.PQ, TombstoneEpoch: s.epoch}
	_, werr := buildSegment(f, ids, seqs, flat, s.cfg.Dim, cfg, rng)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		os.Remove(tmp)
		return nil, fmt.Errorf("vecindex: write segment %s: %w", path, werr)
	}
	seg, err := OpenSegmentFile(path)
	if err != nil {
		os.Remove(path)
		return nil, err
	}
	return &liveSegment{seg: seg, path: path, dead: make([]uint64, (seg.Len()+63)/64)}, nil
}

// autoMergeLocked applies the merge policy of spec 22.3: segments whose
// tombstones exceed the ratio are rewritten, and when there are more than
// MaxSegments segments the smallest are combined.
func (s *Segmented) autoMergeLocked() error {
	if s.cfg.DisableAutoMerge || len(s.segs) == 0 {
		return nil
	}
	var pick []*liveSegment
	for _, ls := range s.segs {
		if float64(ls.deadCount) > s.cfg.MergeTombstoneRatio*float64(ls.seg.Len()) {
			pick = append(pick, ls)
		}
	}
	if len(s.segs) > s.cfg.MaxSegments {
		bySize := slices.Clone(s.segs)
		slices.SortStableFunc(bySize, func(a, b *liveSegment) int { return cmp.Compare(a.liveCount(), b.liveCount()) })
		for _, ls := range bySize {
			if len(s.segs)-len(pick)+1 <= s.cfg.MaxSegments && len(pick) >= 2 {
				break
			}
			if !slices.Contains(pick, ls) {
				pick = append(pick, ls)
			}
		}
	}
	if len(pick) == 0 {
		return nil
	}
	return s.mergeLocked(pick)
}

// mergeLocked replaces the segments in pick by one segment holding their
// live vectors (a rebuild: new Vamana graph and new PQ codebooks). Merged
// segments' files are deleted after the swap.
func (s *Segmented) mergeLocked(pick []*liveSegment) error {
	var ids []core.UUID
	var seqs []uint64
	var flat []float32
	for _, ls := range pick {
		err := ls.seg.forEach(func(i uint32, vec []float32) error {
			if ls.isDead(i) {
				return nil
			}
			id, seq, _ := ls.seg.Entry(i)
			ids = append(ids, id)
			seqs = append(seqs, seq)
			flat = append(flat, vec...)
			return nil
		})
		if err != nil {
			return fmt.Errorf("vecindex: merge: %w", err)
		}
	}
	var merged *liveSegment
	if len(ids) > 0 {
		var err error
		if merged, err = s.writeSegment(ids, seqs, flat); err != nil {
			return fmt.Errorf("vecindex: merge: %w", err)
		}
		for i, id := range ids {
			s.ver[id] = version{seq: seqs[i], seg: merged, node: uint32(i)}
		}
	}
	s.segs = slices.DeleteFunc(s.segs, func(ls *liveSegment) bool { return slices.Contains(pick, ls) })
	if merged != nil {
		s.segs = append(s.segs, merged)
	}
	var errs []error
	for _, ls := range pick {
		if err := ls.seg.Close(); err != nil {
			errs = append(errs, err)
		}
		if err := os.Remove(ls.path); err != nil {
			errs = append(errs, err)
		}
	}
	s.merges++
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("vecindex: merge cleanup: %w", err)
	}
	return nil
}

// Search implements Index.
func (s *Segmented) Search(q []float32, p SearchParams) ([]Hit, error) {
	p, err := p.normalized()
	if err != nil {
		return nil, err
	}
	pq, err := prepareVector(s.cfg.Metric, s.cfg.Dim, q)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrClosed
	}
	if f := p.Filter; f != nil && f.MatchingIDs != nil {
		ids := f.MatchingIDs()
		if float64(len(ids)) < s.cfg.ExactFallbackRatio*float64(s.live) {
			return s.exactScan(pq, ids, p)
		}
	}
	var keep func(core.UUID) bool
	over := tombstoneOverFetch
	if p.Filter != nil {
		keep = p.Filter.Keep
		over = filterOverFetch
	}
	fetch := p.K * over
	L := max(p.L, fetch)
	memHits := s.mem.search(pq, p.K, keep)
	for {
		lists := make([][]Hit, len(s.segs)+1)
		lists[0] = memHits
		short := make([]bool, len(s.segs))
		errs := make([]error, len(s.segs))
		parallelFor(len(s.segs), len(s.segs), func(_, i int) {
			ls := s.segs[i]
			raw, _, err := ls.seg.search(pq, fetch, L, p.BeamWidth)
			if err != nil {
				errs[i] = err
				return
			}
			out := make([]Hit, 0, len(raw))
			for _, h := range raw {
				if ls.isDead(h.Node) || (keep != nil && !keep(h.ID)) {
					continue
				}
				out = append(out, Hit{ID: h.ID, Distance: h.Distance})
			}
			lists[i+1] = out
			// The segment may hold more survivors than this fetch saw.
			short[i] = len(raw) == fetch && len(out) < p.K && fetch < ls.seg.Len()
		})
		if err := errors.Join(errs...); err != nil {
			return nil, fmt.Errorf("vecindex: search: %w", err)
		}
		merged := mergeSorted(lists, p.K)
		if len(merged) >= p.K || !slices.Contains(short, true) {
			return merged, nil
		}
		// Fewer than k survived tombstones and filters: retry with a
		// larger list (spec 22.7).
		fetch *= 2
		L = max(L*2, fetch)
	}
}

// exactScan answers a highly selective filter from the matching ids.
func (s *Segmented) exactScan(q []float32, ids []core.UUID, p SearchParams) ([]Hit, error) {
	top := newTopK(p.K)
	seen := make(map[core.UUID]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		v, ok := s.ver[id]
		if !ok || v.deleted || !p.keep(id) {
			continue
		}
		var vec []float32
		if v.seg == nil {
			vec, _ = s.mem.vector(id)
		} else {
			var err error
			if vec, err = v.seg.seg.Vector(v.node); err != nil {
				return nil, fmt.Errorf("vecindex: exact scan: %w", err)
			}
		}
		top.push(Hit{ID: id, Distance: sanitize(Distance(s.cfg.Metric, q, vec))})
	}
	return top.sorted(), nil
}

// Info implements Index.
func (s *Segmented) Info() IndexInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	info := IndexInfo{
		ModelID:         s.cfg.ModelID,
		ModelVersion:    s.cfg.ModelVersion,
		Dimension:       s.cfg.Dim,
		Metric:          s.cfg.Metric,
		Vectors:         uint64(s.live),
		Segments:        len(s.segs),
		MemtableVectors: uint64(s.mem.len()),
		RAMBytes:        s.mem.ramBytes() + uint64(len(s.ver))*48,
		WatermarkSeq:    s.watermark,
	}
	for _, ls := range s.segs {
		info.Tombstones += uint64(ls.deadCount)
		info.RAMBytes += ls.seg.RAMBytes() + uint64(len(ls.dead))*8
		info.DiskBytes += ls.seg.DiskBytes()
	}
	return info
}

// SegmentPaths returns the files of the live segments, oldest first.
func (s *Segmented) SegmentPaths() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.segs))
	for i, ls := range s.segs {
		out[i] = ls.path
	}
	return out
}

// Close closes every segment file. The files stay on disk; Destroy removes
// them.
func (s *Segmented) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeLocked()
}

func (s *Segmented) closeLocked() error {
	if s.closed {
		return nil
	}
	s.closed = true
	var errs []error
	for _, ls := range s.segs {
		errs = append(errs, ls.seg.Close())
	}
	return errors.Join(errs...)
}

// Destroy closes the index and deletes its segment files: the erasure of a
// whole (tenant, program, model) index is a file deletion (spec 22.3).
func (s *Segmented) Destroy() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	errs := []error{s.closeLocked()}
	for _, ls := range s.segs {
		if err := os.Remove(ls.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	s.segs = nil
	return errors.Join(errs...)
}

// popcount of the tombstone bitset, for invariant checks in tests.
func (ls *liveSegment) countDead() int {
	n := 0
	for _, w := range ls.dead {
		n += bits.OnesCount64(w)
	}
	return n
}
