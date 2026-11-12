package limiter

import (
	"fmt"
	"maps"
	"sync"
	"time"
)

// GCounter is a grow-only counter of admitted cost per node for one fixed
// window (spec 14.4). Merge takes the element-wise maximum, which is
// commutative, associative and idempotent; a counter for a later window
// supersedes one for an earlier window.
type GCounter struct {
	Window int64             // index of the window: floor(now / W)
	Counts map[string]uint64 // node -> admitted cost in the window
}

// Value returns the total admitted cost across nodes.
func (c GCounter) Value() uint64 {
	var v uint64
	for _, n := range c.Counts {
		v += n
	}
	return v
}

// Add records n more admitted cost for node.
func (c *GCounter) Add(node string, n uint64) {
	if c.Counts == nil {
		c.Counts = map[string]uint64{}
	}
	c.Counts[node] += n
}

// Merge folds o into c.
func (c *GCounter) Merge(o GCounter) {
	switch {
	case o.Window < c.Window:
		return
	case o.Window > c.Window:
		c.Window, c.Counts = o.Window, maps.Clone(o.Counts)
		return
	}
	if c.Counts == nil {
		c.Counts = map[string]uint64{}
	}
	for n, v := range o.Counts {
		c.Counts[n] = max(c.Counts[n], v)
	}
}

// Clone returns a deep copy, safe to send to another node.
func (c GCounter) Clone() GCounter {
	return GCounter{Window: c.Window, Counts: maps.Clone(c.Counts)}
}

// CRDTLimiter is a node of the CRDT-counter strategy (spec 14.4). Per key
// it keeps GCounters of admitted cost for the current and the previous
// fixed window of length W = Window and admits a request with the
// sliding-window estimate of SlidingWindowCounter over the merged totals:
//
//	previous * (1 - elapsed_in_window / W) + current + cost <= L (= Burst)
//
// Nodes exchange State and Merge it. Between merges a node sees only part
// of the others' admissions, so the cluster can overshoot during
// partitions and converges once gossip resumes; a single node obeys the
// same bound as SlidingWindowCounter.
type CRDTLimiter struct {
	node     string
	rules    *Rules
	mu       sync.Mutex
	counters map[string]*crdtState
}

type crdtState struct{ prev, cur GCounter }

// NewCRDTLimiter returns the limiter of node configured with rules.
func NewCRDTLimiter(node string, rules ...Rule) (*CRDTLimiter, error) {
	if node == "" {
		return nil, fmt.Errorf("%w: empty node id", ErrInvalidConfig)
	}
	r, err := NewRules(rules...)
	if err != nil {
		return nil, err
	}
	return &CRDTLimiter{node: node, rules: r, counters: map[string]*crdtState{}}, nil
}

// Configure implements Limiter.
func (l *CRDTLimiter) Configure(pattern string, p Params) error {
	return l.rules.Configure(pattern, p)
}

// roll moves the state forward so that cur is window w (never backwards).
func (s *crdtState) roll(w int64) {
	switch {
	case w <= s.cur.Window:
	case w == s.cur.Window+1:
		s.prev, s.cur = s.cur, GCounter{Window: w}
	default:
		s.prev, s.cur = GCounter{Window: w - 1}, GCounter{Window: w}
	}
}

// fold merges one received counter into the state.
func (s *crdtState) fold(o GCounter) {
	switch {
	case o.Window > s.cur.Window:
		s.roll(o.Window)
		s.cur.Merge(o)
	case o.Window == s.cur.Window:
		s.cur.Merge(o)
	case o.Window == s.prev.Window:
		s.prev.Merge(o)
	}
}

func (l *CRDTLimiter) stateLocked(key string, c *compiled, now int64) *crdtState {
	w := floorDiv(now, c.window)
	s := l.counters[key]
	if s == nil {
		s = &crdtState{prev: GCounter{Window: w - 1}, cur: GCounter{Window: w}}
		l.counters[key] = s
	}
	s.roll(w)
	return s
}

// Acquire implements Limiter.
func (l *CRDTLimiter) Acquire(key string, cost int, now time.Duration) (Result, error) {
	return l.AcquireAll([]Request{{key, cost}}, now)
}

// clampCount converts a merged count, which may exceed the limit after a
// partition, to int64 without overflow.
func clampCount(v uint64) int64 { return int64(min(v, uint64(maxSpan))) }

// AcquireAll implements Limiter.
func (l *CRDTLimiter) AcquireAll(reqs []Request, now time.Duration) (Result, error) {
	var buf [8]prepared
	ps, res, err := l.rules.prepareAll(reqs, now, buf[:])
	if err != nil {
		return res, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	n := int64(now)
	ds := make([]decision, 0, len(ps))
	ss := make([]*crdtState, 0, len(ps))
	for _, p := range ps {
		s := l.stateLocked(p.key, p.c, n)
		ss = append(ss, s)
		w, lim := p.c.window, p.c.burst
		// A clock behind the newest window is treated as its start.
		e := max(n-s.cur.Window*w, 0)
		prev, cur := clampCount(s.prev.Value()), clampCount(s.cur.Value())
		if ok, slack := fits(lim, w, prev, cur, p.cost, e); ok {
			ds = append(ds, decision{allowed: true, remaining: slack.div(w), limit: lim})
		} else {
			ds = append(ds, decision{retry: windowRetry(lim, w, prev, cur, p.cost, e), limit: lim})
		}
	}
	out := combine(ds)
	if out.Allowed {
		for i, p := range ps {
			ss[i].cur.Add(l.node, uint64(p.cost))
		}
	}
	return out, nil
}

// State returns copies of key's previous- and current-window counters for
// gossip.
func (l *CRDTLimiter) State(key string) []GCounter {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.counters[key]
	if s == nil {
		return nil
	}
	return []GCounter{s.prev.Clone(), s.cur.Clone()}
}

// Merge folds counters received from another node into key's state.
func (l *CRDTLimiter) Merge(key string, cs ...GCounter) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, o := range cs {
		s := l.counters[key]
		if s == nil {
			s = &crdtState{prev: GCounter{Window: o.Window - 1}, cur: GCounter{Window: o.Window}}
			l.counters[key] = s
		}
		s.fold(o)
	}
}
