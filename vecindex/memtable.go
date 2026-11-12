package vecindex

import "github.com/genuinebnt/flowforge/core"

// memtable is the in-RAM part of an index (spec 22.3): a flat, unordered
// array of vectors searched by exact brute force. It is also the storage of
// BruteForce. It does not order writes; callers do that. It is not safe for
// concurrent use; callers synchronise.
//
// The spec switches the memtable to an in-memory Vamana graph above roughly
// 50,000 vectors. Segmented flushes at Config.FlushThreshold (default
// 50,000), so the memtable here always stays in the brute-force regime.
type memtable struct {
	metric Metric
	dim    int
	ids    []core.UUID
	seqs   []uint64
	vecs   []float32 // len(ids) * dim, row-major
	slot   map[core.UUID]int
}

func newMemtable(m Metric, dim int) *memtable {
	return &memtable{metric: m, dim: dim, slot: make(map[core.UUID]int)}
}

// len returns the number of stored vectors.
func (t *memtable) len() int { return len(t.ids) }

// put inserts or replaces id's (already prepared) vector.
func (t *memtable) put(id core.UUID, v []float32, seq uint64) {
	if i, ok := t.slot[id]; ok {
		copy(t.vecs[i*t.dim:(i+1)*t.dim], v)
		t.seqs[i] = seq
		return
	}
	t.slot[id] = len(t.ids)
	t.ids = append(t.ids, id)
	t.seqs = append(t.seqs, seq)
	t.vecs = append(t.vecs, v...)
}

// remove deletes id by moving the last row into its slot. It reports
// whether id was present.
func (t *memtable) remove(id core.UUID) bool {
	i, ok := t.slot[id]
	if !ok {
		return false
	}
	last := len(t.ids) - 1
	if i != last {
		t.ids[i] = t.ids[last]
		t.seqs[i] = t.seqs[last]
		copy(t.vecs[i*t.dim:(i+1)*t.dim], t.vecs[last*t.dim:])
		t.slot[t.ids[i]] = i
	}
	t.ids = t.ids[:last]
	t.seqs = t.seqs[:last]
	t.vecs = t.vecs[:last*t.dim]
	delete(t.slot, id)
	return true
}

// vector returns id's stored vector (aliasing the memtable).
func (t *memtable) vector(id core.UUID) ([]float32, bool) {
	i, ok := t.slot[id]
	if !ok {
		return nil, false
	}
	return t.vecs[i*t.dim : (i+1)*t.dim : (i+1)*t.dim], true
}

// row returns the i-th stored vector.
func (t *memtable) row(i int) []float32 { return t.vecs[i*t.dim : (i+1)*t.dim : (i+1)*t.dim] }

// search returns the exact top k of the rows that pass keep (nil keeps
// all), sorted by compareHit.
func (t *memtable) search(q []float32, k int, keep func(core.UUID) bool) []Hit {
	top := newTopK(k)
	for i, id := range t.ids {
		if keep != nil && !keep(id) {
			continue
		}
		d := sanitize(Distance(t.metric, q, t.row(i)))
		if !top.accepts(d) {
			continue
		}
		top.push(Hit{ID: id, Distance: d})
	}
	return top.sorted()
}

// reset empties the memtable, keeping its capacity.
func (t *memtable) reset() {
	t.ids = t.ids[:0]
	t.seqs = t.seqs[:0]
	t.vecs = t.vecs[:0]
	clear(t.slot)
}

// ramBytes estimates the memtable's heap footprint.
func (t *memtable) ramBytes() uint64 {
	const perEntryOverhead = 16 + 8 + 48 // id, seq, map entry
	return uint64(len(t.ids)) * uint64(t.dim*4+perEntryOverhead)
}
