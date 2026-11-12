package limiter

import (
	"math"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// gcraShards is the number of shards of an AtomicGCRA (a power of two).
const gcraShards = 64

// Cell sentinels. Real TATs stay below MaxTime + maxSpan < 2^63 - 2, so
// these values can never be confused with one.
const (
	tatLocked    = math.MaxInt64     // held by a multi-key AcquireAll
	tatTombstone = math.MaxInt64 - 1 // removed from the map by Prune
)

// AtomicGCRA is GCRA with one atomic int64 TAT per key, kept in a sharded
// concurrent map so that many keys scale with cores (spec 14.2, b_lim_02).
//
// Concurrency design:
//
//   - Each shard is a sync.Map from key to *atomic.Int64. Lookups of
//     existing keys take no lock and do not allocate.
//   - Acquire runs the GCRA step in a compare-and-swap loop on the key's
//     cell: concurrent acquirers of one key serialize only on the CAS, and
//     a denial writes nothing.
//   - AcquireAll locks the cells of all its keys by swapping in a
//     tatLocked sentinel, in key order (so two AcquireAll calls cannot
//     deadlock), decides, and then stores either every new TAT or every
//     old one. Single-key acquirers that meet a locked cell yield and
//     retry. This makes multi-key acquisition exact: if any key denies,
//     none is charged.
//   - Prune swaps tatTombstone into idle cells before deleting them; an
//     acquirer that meets a tombstone looks the key up again, so no charge
//     can land in a deleted cell.
//
// Its decisions are identical to GCRA's for the same sequence of calls.
type AtomicGCRA struct {
	rules  *Rules
	shards [gcraShards]gcraShard
}

type gcraShard struct {
	m sync.Map // string -> *atomic.Int64
	_ [64]byte // keep neighbouring shards on separate cache lines
}

// NewAtomicGCRA returns an AtomicGCRA configured with rules.
func NewAtomicGCRA(rules ...Rule) (*AtomicGCRA, error) {
	r, err := NewRules(rules...)
	if err != nil {
		return nil, err
	}
	return &AtomicGCRA{rules: r}, nil
}

// Configure implements Limiter.
func (g *AtomicGCRA) Configure(pattern string, p Params) error {
	return g.rules.Configure(pattern, p)
}

// shardOf hashes key with FNV-1a.
func shardOf(key string) int {
	h := uint64(14695981039346656037)
	for i := 0; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= 1099511628211
	}
	return int(h & (gcraShards - 1))
}

// cell returns key's TAT cell, creating it on first use.
func (g *AtomicGCRA) cell(key string) *atomic.Int64 {
	sh := &g.shards[shardOf(key)]
	if v, ok := sh.m.Load(key); ok {
		return v.(*atomic.Int64)
	}
	fresh := new(atomic.Int64)
	fresh.Store(gcraNever)
	v, _ := sh.m.LoadOrStore(strings.Clone(key), fresh)
	return v.(*atomic.Int64)
}

// casGCRA runs the GCRA step on cell until it denies or wins the
// compare-and-swap. ok is false if the cell was pruned meanwhile.
func casGCRA(cell *atomic.Int64, c *compiled, cost, now int64) (d decision, ok bool) {
	for {
		old := cell.Load()
		switch old {
		case tatTombstone:
			return d, false
		case tatLocked:
			runtime.Gosched()
			continue
		}
		newTat, d := gcraDecide(old, c, cost, now)
		if !d.allowed || cell.CompareAndSwap(old, newTat) {
			return d, true
		}
	}
}

// Acquire implements Limiter.
func (g *AtomicGCRA) Acquire(key string, cost int, now time.Duration) (Result, error) {
	c, err := g.rules.prepare(key, cost, now)
	if err != nil {
		if c != nil {
			return cannot(c.burst), err
		}
		return Result{}, err
	}
	for {
		if d, ok := casGCRA(g.cell(key), c, int64(cost), int64(now)); ok {
			return d.result(), nil
		}
	}
}

// lock swaps tatLocked into key's cell and returns the cell and the TAT
// it held.
func (g *AtomicGCRA) lock(key string) (*atomic.Int64, int64) {
	for {
		cell := g.cell(key)
		for {
			old := cell.Load()
			if old == tatTombstone {
				break // pruned: look the key up again
			}
			if old == tatLocked {
				runtime.Gosched()
				continue
			}
			if cell.CompareAndSwap(old, tatLocked) {
				return cell, old
			}
		}
	}
}

// AcquireAll implements Limiter.
func (g *AtomicGCRA) AcquireAll(reqs []Request, now time.Duration) (Result, error) {
	var buf [8]prepared
	ps, res, err := g.rules.prepareAll(reqs, now, buf[:])
	if err != nil {
		return res, err
	}
	if len(ps) == 1 {
		return g.Acquire(ps[0].key, int(ps[0].cost), now)
	}
	// Lock in key order; decide in request order.
	var orderBuf [8]int
	order := orderBuf[:0]
	for i := range ps {
		order = append(order, i)
	}
	slices.SortFunc(order, func(a, b int) int { return strings.Compare(ps[a].key, ps[b].key) })
	var cellBuf [8]*atomic.Int64
	var oldBuf, newBuf [8]int64
	var dBuf [8]decision
	cells, olds, news, ds := cellBuf[:len(ps)], oldBuf[:len(ps)], newBuf[:len(ps)], dBuf[:len(ps)]
	if len(ps) > len(cellBuf) {
		cells, olds, news, ds = make([]*atomic.Int64, len(ps)), make([]int64, len(ps)), make([]int64, len(ps)), make([]decision, len(ps))
	}
	for _, i := range order {
		cells[i], olds[i] = g.lock(ps[i].key)
	}
	n := int64(now)
	for i, p := range ps {
		news[i], ds[i] = gcraDecide(olds[i], p.c, p.cost, n)
	}
	out := combine(ds)
	for i, cell := range cells {
		if out.Allowed {
			cell.Store(news[i])
		} else {
			cell.Store(olds[i])
		}
	}
	return out, nil
}

// Prune forgets every key whose TAT is at or before now (see GCRA.Prune).
func (g *AtomicGCRA) Prune(now time.Duration) int {
	n := 0
	for i := range g.shards {
		sh := &g.shards[i]
		sh.m.Range(func(k, v any) bool {
			cell := v.(*atomic.Int64)
			old := cell.Load()
			if old != tatLocked && old != tatTombstone && old <= int64(now) && cell.CompareAndSwap(old, tatTombstone) {
				sh.m.CompareAndDelete(k, v)
				n++
			}
			return true
		})
	}
	return n
}

// Len returns the number of keys with state.
func (g *AtomicGCRA) Len() int {
	n := 0
	for i := range g.shards {
		g.shards[i].m.Range(func(_, _ any) bool { n++; return true })
	}
	return n
}
