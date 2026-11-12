package vecindex

import (
	"fmt"
	"sync"

	"github.com/genuinebnt/flowforge/core"
)

// BruteForce is the exact index: every search scans every vector. It is the
// ground truth for recall measurements and the in-memory fake of the Index
// port (spec 24.3). It is safe for concurrent use.
type BruteForce struct {
	mu        sync.RWMutex
	tab       *memtable
	lastSeq   map[core.UUID]uint64 // newest seq applied per id, deletes included
	watermark uint64
}

var _ Index = (*BruteForce)(nil)

// NewBruteForce returns an empty exact index.
func NewBruteForce(m Metric, dim int) (*BruteForce, error) {
	if !m.Valid() {
		return nil, fmt.Errorf("%w: %v", ErrInvalidMetric, m)
	}
	if dim < 1 || dim > MaxDimension {
		return nil, fmt.Errorf("%w: dimension %d outside 1..%d", ErrInvalidConfig, dim, MaxDimension)
	}
	return &BruteForce{tab: newMemtable(m, dim), lastSeq: make(map[core.UUID]uint64)}, nil
}

// Upsert implements Index.
func (b *BruteForce) Upsert(id core.UUID, v []float32, seq uint64) error {
	pv, err := prepareVector(b.tab.metric, b.tab.dim, v)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.newer(id, seq) {
		return nil
	}
	b.tab.put(id, pv, seq)
	return nil
}

// Delete implements Index.
func (b *BruteForce) Delete(id core.UUID, seq uint64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.newer(id, seq) {
		return nil
	}
	b.tab.remove(id)
	return nil
}

// newer records seq for id and reports whether it is newer than the last
// write applied for id. The caller holds the write lock.
func (b *BruteForce) newer(id core.UUID, seq uint64) bool {
	if last, ok := b.lastSeq[id]; ok && seq <= last {
		return false
	}
	b.lastSeq[id] = seq
	b.watermark = max(b.watermark, seq)
	return true
}

// Search implements Index. L and BeamWidth are ignored: the scan is exact.
func (b *BruteForce) Search(q []float32, p SearchParams) ([]Hit, error) {
	p, err := p.normalized()
	if err != nil {
		return nil, err
	}
	pq, err := prepareVector(b.tab.metric, b.tab.dim, q)
	if err != nil {
		return nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	var keep func(core.UUID) bool
	if p.Filter != nil {
		keep = p.Filter.Keep
	}
	return b.tab.search(pq, p.K, keep), nil
}

// Info implements Index.
func (b *BruteForce) Info() IndexInfo {
	b.mu.RLock()
	defer b.mu.RUnlock()
	n := uint64(b.tab.len())
	return IndexInfo{
		Dimension:       b.tab.dim,
		Metric:          b.tab.metric,
		Vectors:         n,
		MemtableVectors: n,
		RAMBytes:        b.tab.ramBytes() + uint64(len(b.lastSeq))*40,
		WatermarkSeq:    b.watermark,
	}
}

// Len returns the number of live vectors.
func (b *BruteForce) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.tab.len()
}

// Vector returns a copy of id's stored vector (unit length under Cosine).
func (b *BruteForce) Vector(id core.UUID) ([]float32, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	v, ok := b.tab.vector(id)
	if !ok {
		return nil, false
	}
	return append([]float32(nil), v...), true
}
