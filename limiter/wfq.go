package limiter

import (
	"fmt"
	"math"
	"sync"
)

// WFQConfig configures a WFQ.
type WFQConfig struct {
	// DefaultWeight is the weight of tenants without SetWeight. Zero means
	// such tenants are rejected.
	DefaultWeight float64
	// MaxPending bounds the queued requests per tenant; zero is unbounded.
	MaxPending int
}

// Grant is one request released by WFQ.Dequeue.
type Grant[T any] struct {
	Tenant string
	Cost   int
	Item   T
	// Start and Finish are the request's virtual start and finish times.
	Start, Finish float64
}

// WFQ is weighted fair queueing across tenants (spec 14.2). Each tenant i
// has a weight w_i and a virtual finish time F_i. A request of cost c
// arriving at virtual time V gets start = max(V, F_i) and
// F_i = start + c / w_i; Dequeue serves the pending request with the
// smallest finish time (ties in arrival order) and advances V to the
// start time of the one served (never backwards: a request served out of
// start order leaves V where it is). Under saturation each tenant receives
// service in proportion to its weight. It is safe for concurrent use.
type WFQ[T any] struct {
	cfg     WFQConfig
	mu      sync.Mutex
	weights map[string]float64
	finish  map[string]float64
	pending map[string]int
	v       float64
	seq     uint64
	heap    []wfqItem[T]
}

type wfqItem[T any] struct {
	g   Grant[T]
	seq uint64
}

// NewWFQ returns an empty queue.
func NewWFQ[T any](cfg WFQConfig) (*WFQ[T], error) {
	if !validWeight(cfg.DefaultWeight) && cfg.DefaultWeight != 0 {
		return nil, fmt.Errorf("%w: wfq default weight %v", ErrInvalidConfig, cfg.DefaultWeight)
	}
	if cfg.MaxPending < 0 {
		return nil, fmt.Errorf("%w: wfq max pending %d", ErrInvalidConfig, cfg.MaxPending)
	}
	return &WFQ[T]{cfg: cfg, weights: map[string]float64{}, finish: map[string]float64{}, pending: map[string]int{}}, nil
}

func validWeight(w float64) bool { return w > 0 && !math.IsInf(w, 0) && !math.IsNaN(w) }

// SetWeight sets tenant's weight (> 0). It applies to requests enqueued
// afterwards.
func (q *WFQ[T]) SetWeight(tenant string, w float64) error {
	if !validWeight(w) {
		return fmt.Errorf("%w: wfq weight %v", ErrInvalidConfig, w)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.weights[tenant] = w
	return nil
}

// Enqueue adds a request of cost for tenant.
func (q *WFQ[T]) Enqueue(tenant string, cost int, item T) error {
	if cost < 1 {
		return ErrInvalidCost
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	w, ok := q.weights[tenant]
	if !ok {
		if q.cfg.DefaultWeight == 0 {
			return fmt.Errorf("%w: wfq tenant %q has no weight", ErrInvalidKey, tenant)
		}
		w = q.cfg.DefaultWeight
	}
	if q.cfg.MaxPending > 0 && q.pending[tenant] >= q.cfg.MaxPending {
		return fmt.Errorf("%w: tenant %q", ErrQueueFull, tenant)
	}
	start := max(q.v, q.finish[tenant])
	f := start + float64(cost)/w
	q.finish[tenant] = f
	q.pending[tenant]++
	q.seq++
	q.push(wfqItem[T]{g: Grant[T]{Tenant: tenant, Cost: cost, Item: item, Start: start, Finish: f}, seq: q.seq})
	return nil
}

// Dequeue removes and returns the pending request with the smallest
// virtual finish time. ok is false when the queue is empty.
func (q *WFQ[T]) Dequeue() (g Grant[T], ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.heap) == 0 {
		return g, false
	}
	it := q.pop()
	q.v = max(q.v, it.g.Start) // "advance": V never moves backwards
	if q.pending[it.g.Tenant]--; q.pending[it.g.Tenant] == 0 {
		delete(q.pending, it.g.Tenant)
	}
	return it.g, true
}

// Len returns the number of pending requests.
func (q *WFQ[T]) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.heap)
}

func (a wfqItem[T]) less(b wfqItem[T]) bool {
	if a.g.Finish != b.g.Finish {
		return a.g.Finish < b.g.Finish
	}
	return a.seq < b.seq
}

func (q *WFQ[T]) push(it wfqItem[T]) {
	q.heap = append(q.heap, it)
	h := q.heap
	for i := len(h) - 1; i > 0; {
		p := (i - 1) / 2
		if !h[i].less(h[p]) {
			break
		}
		h[i], h[p] = h[p], h[i]
		i = p
	}
}

func (q *WFQ[T]) pop() wfqItem[T] {
	h := q.heap
	top := h[0]
	last := len(h) - 1
	h[0] = h[last]
	var zero wfqItem[T]
	h[last] = zero
	h = h[:last]
	for i := 0; ; {
		l, r, m := 2*i+1, 2*i+2, i
		if l < len(h) && h[l].less(h[m]) {
			m = l
		}
		if r < len(h) && h[r].less(h[m]) {
			m = r
		}
		if m == i {
			break
		}
		h[i], h[m] = h[m], h[i]
		i = m
	}
	q.heap = h
	return top
}
