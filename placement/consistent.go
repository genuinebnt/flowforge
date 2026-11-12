package placement

import (
	"cmp"
	"encoding/binary"
	"slices"
	"sync/atomic"
)

// DefaultVirtualNodes is the number of ring points per unit of weight when
// ConsistentConfig.VirtualNodes is zero.
const DefaultVirtualNodes = 256

// maxWeight bounds Weight so a bad weight function cannot allocate an
// unbounded ring.
const maxWeight = 1024

// ConsistentConfig configures consistent hashing.
type ConsistentConfig struct {
	// VirtualNodes is the number of ring points per unit of weight
	// (default DefaultVirtualNodes).
	VirtualNodes int
	// Weight returns the relative capacity of a worker. It must be a
	// deterministic function of the id that every node agrees on. Nil
	// means every worker has weight 1; results are clamped to [1, 1024].
	Weight func(WorkerID) int
}

func (c ConsistentConfig) vnodes() int {
	if c.VirtualNodes <= 0 {
		return DefaultVirtualNodes
	}
	return c.VirtualNodes
}

func (c ConsistentConfig) weight(w WorkerID) int {
	if c.Weight == nil {
		return 1
	}
	return min(max(c.Weight(w), 1), maxWeight)
}

// Ring is an immutable consistent-hash ring over a fixed worker set. Lookups
// are a binary search, O(log(points)). It is safe for concurrent use.
type Ring struct {
	points  []uint64   // sorted ascending
	owners  []WorkerID // owners[i] owns points[i]
	workers []WorkerID // distinct, sorted
}

// NewRing builds the ring for workers. Virtual node i of worker w sits at
// Hash64(uint64_le(i), w); a key sits at Hash64(key, ""). Equal points are
// ordered by worker id, so the ring does not depend on the order of
// workers.
func NewRing(workers []WorkerID, cfg ConsistentConfig) *Ring {
	ws := distinct(workers)
	type point struct {
		p uint64
		w WorkerID
	}
	var pts []point
	var idx [8]byte
	for _, w := range ws {
		n := cfg.vnodes() * cfg.weight(w)
		for i := range n {
			binary.LittleEndian.PutUint64(idx[:], uint64(i))
			pts = append(pts, point{Hash64(idx[:], w), w})
		}
	}
	slices.SortFunc(pts, func(a, b point) int {
		if c := cmp.Compare(a.p, b.p); c != 0 {
			return c
		}
		return cmp.Compare(a.w, b.w)
	})
	r := &Ring{points: make([]uint64, len(pts)), owners: make([]WorkerID, len(pts)), workers: ws}
	for i, p := range pts {
		r.points[i], r.owners[i] = p.p, p.w
	}
	return r
}

// Workers returns the distinct workers on the ring, sorted.
func (r *Ring) Workers() []WorkerID { return slices.Clone(r.workers) }

// start returns the index of the first point at or after the key's
// position, wrapping to 0.
func (r *Ring) start(key []byte) int {
	h := Hash64(key, "")
	i, _ := slices.BinarySearch(r.points, h)
	if i == len(r.points) {
		i = 0
	}
	return i
}

// Owner returns the worker owning the first point at or after the key's
// position, or false for an empty ring.
func (r *Ring) Owner(key []byte) (WorkerID, bool) {
	if len(r.points) == 0 {
		return "", false
	}
	return r.owners[r.start(key)], true
}

// Ranking returns the distinct workers in the order they are met walking
// the ring clockwise from the key's position.
func (r *Ring) Ranking(key []byte) []WorkerID {
	out := make([]WorkerID, 0, len(r.workers))
	if len(r.points) == 0 {
		return out
	}
	seen := make(map[WorkerID]struct{}, len(r.workers))
	for i, s := 0, r.start(key); i < len(r.points) && len(out) < len(r.workers); i++ {
		w := r.owners[(s+i)%len(r.points)]
		if _, ok := seen[w]; !ok {
			seen[w] = struct{}{}
			out = append(out, w)
		}
	}
	return out
}

// ConsistentHash implements Placement with a ring of virtual nodes. It
// caches the ring for the most recent membership, so repeated lookups
// against an unchanged live set cost one slice comparison plus a binary
// search; a changed set rebuilds the ring. It is safe for concurrent use.
type ConsistentHash struct {
	cfg   ConsistentConfig
	cache atomic.Pointer[ringCache]
}

type ringCache struct {
	live []WorkerID // copy of the input the ring was built from
	ring *Ring
}

var _ Placement = (*ConsistentHash)(nil)

// NewConsistentHash returns a ConsistentHash with cfg.
func NewConsistentHash(cfg ConsistentConfig) *ConsistentHash {
	return &ConsistentHash{cfg: cfg}
}

func (c *ConsistentHash) ring(live []WorkerID) *Ring {
	if rc := c.cache.Load(); rc != nil && slices.Equal(rc.live, live) {
		return rc.ring
	}
	rc := &ringCache{live: slices.Clone(live), ring: NewRing(live, c.cfg)}
	c.cache.Store(rc)
	return rc.ring
}

// Owner implements Placement.
func (c *ConsistentHash) Owner(key []byte, live []WorkerID) (WorkerID, bool) {
	if len(live) == 0 {
		return "", false
	}
	return c.ring(live).Owner(key)
}

// Ranking implements Placement.
func (c *ConsistentHash) Ranking(key []byte, live []WorkerID) []WorkerID {
	if len(live) == 0 {
		return []WorkerID{}
	}
	return c.ring(live).Ranking(key)
}
