package limiter

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"
)

// DefaultLeaseTTL is the lease lifetime of spec 14.4.
const DefaultLeaseTTL = 2 * time.Second

// LeaseRequest asks a Store for a chunk of a key's budget.
type LeaseRequest struct {
	Key    string
	Holder string // the node asking, for diagnostics
	// Min is the least the caller can use: the store grants either nothing
	// or between Min and Want tokens (1 <= Min <= Want).
	Min, Want int
	TTL       time.Duration
	Now       time.Duration
}

// Lease is a chunk of budget held by one node until Expires.
type Lease struct {
	ID      string // empty when nothing was granted
	Key     string
	Granted int
	Expires time.Duration
	// RetryAfter is set when nothing was granted: the time until Min
	// tokens could be available.
	RetryAfter time.Duration
}

// Store is the central budget port of leased local budgets (spec 14.4).
// Implementations wrap I/O failures with ErrUnavailable.
//
// The store never takes budget back by itself when a lease expires,
// because it cannot know how much the holder spent; only Return credits
// unused tokens. An expired lease that is never returned is forfeited, so
// a crashed holder makes the system undershoot, never double spend.
type Store interface {
	Lease(ctx context.Context, req LeaseRequest) (Lease, error)
	// Return credits unused tokens of a lease back to its key and closes
	// the lease. A second Return of the same lease fails with
	// ErrUnknownLease and credits nothing.
	Return(ctx context.Context, leaseID string, unused int, now time.Duration) error
}

// MemStore is the in-memory Store: per key, a fixed-point token bucket
// configured by rules, plus the table of open leases.
type MemStore struct {
	rules   *Rules
	mu      sync.Mutex
	buckets map[string]*bucketState
	open    map[string]openLease
	seq     uint64
}

type openLease struct {
	key     string
	granted int
	expires time.Duration
	ttl     time.Duration
}

// NewMemStore returns a MemStore configured with rules.
func NewMemStore(rules ...Rule) (*MemStore, error) {
	r, err := NewRules(rules...)
	if err != nil {
		return nil, err
	}
	return &MemStore{rules: r, buckets: map[string]*bucketState{}, open: map[string]openLease{}}, nil
}

// Lease implements Store.
func (m *MemStore) Lease(_ context.Context, req LeaseRequest) (Lease, error) {
	if req.Min < 1 || req.Want < req.Min {
		return Lease{}, fmt.Errorf("%w: lease min %d want %d", ErrInvalidCost, req.Min, req.Want)
	}
	if req.TTL <= 0 || req.TTL > time.Duration(maxSpan) {
		return Lease{}, fmt.Errorf("%w: lease ttl %v", ErrInvalidConfig, req.TTL)
	}
	c, err := m.rules.prepare(req.Key, req.Min, req.Now)
	if err != nil {
		return Lease{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(req.Now)
	b := m.buckets[req.Key]
	if b == nil {
		b = new(bucketState)
		m.buckets[req.Key] = b
	}
	if d := b.take(c, c.rate, int64(req.Min), int64(req.Now)); !d.allowed {
		return Lease{Key: req.Key, RetryAfter: time.Duration(d.retry)}, nil
	}
	grant := int(min(int64(req.Want), b.units/unit))
	b.units -= int64(grant) * unit
	m.seq++
	id := "lease-" + strconv.FormatUint(m.seq, 10)
	l := Lease{ID: id, Key: req.Key, Granted: grant, Expires: req.Now + req.TTL}
	m.open[id] = openLease{key: req.Key, granted: grant, expires: l.Expires, ttl: req.TTL}
	return l, nil
}

// Return implements Store.
func (m *MemStore) Return(_ context.Context, id string, unused int, now time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ol, ok := m.open[id]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownLease, id)
	}
	if unused < 0 || unused > ol.granted {
		return fmt.Errorf("%w: return %d of %d", ErrInvalidCost, unused, ol.granted)
	}
	delete(m.open, id)
	if c, err := m.rules.lookup(ol.key); err == nil && unused > 0 {
		b := m.buckets[ol.key]
		b.refill(c.capUnits, c.rate, int64(now))
		b.units = min(c.capUnits, b.units+int64(unused)*unit)
	}
	return nil
}

// Open returns the number of leases not yet returned or pruned.
func (m *MemStore) Open() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.open)
}

// pruneLocked forgets leases that expired more than one TTL ago; their
// unreturned budget is forfeited.
func (m *MemStore) pruneLocked(now time.Duration) {
	for id, ol := range m.open {
		if now-ol.expires > ol.ttl {
			delete(m.open, id)
		}
	}
}

// LeaseConfig configures a LeasedBudget.
type LeaseConfig struct {
	Store  Store
	Holder string // this node's identity
	// Chunk is the number of tokens leased at a time (for example 10% of
	// the key's burst); a larger request leases its own cost.
	Chunk int
	// TTL is the lease lifetime; zero means DefaultLeaseTTL.
	TTL time.Duration
	// Margin stops local spending this long before a lease expires, to
	// absorb clock differences between the node and the store.
	Margin time.Duration
}

// LeasedBudget is the node side of leased local budgets (spec 14.4): it
// leases chunks of each key's budget from a central Store and spends them
// locally until they run out or expire. A lease is closed locally before
// its unused remainder is returned, so no token is ever spent twice; a
// return that fails forfeits the remainder (undershoot). Overshoot is
// bounded by the outstanding leases.
//
// LeasedBudget holds one mutex across store calls, which keeps it simple
// and exact per node at the cost of serializing a node's acquisitions
// during a round trip.
type LeasedBudget struct {
	cfg       LeaseConfig
	mu        sync.Mutex
	leases    map[string]*heldLease
	forfeited int
}

type heldLease struct {
	Lease
	left int
}

// NewLeasedBudget validates cfg and returns a LeasedBudget.
func NewLeasedBudget(cfg LeaseConfig) (*LeasedBudget, error) {
	if cfg.TTL == 0 {
		cfg.TTL = DefaultLeaseTTL
	}
	switch {
	case cfg.Store == nil:
		return nil, fmt.Errorf("%w: lease store is nil", ErrInvalidConfig)
	case cfg.Chunk < 1:
		return nil, fmt.Errorf("%w: lease chunk %d", ErrInvalidConfig, cfg.Chunk)
	case cfg.TTL < 0 || cfg.Margin < 0 || cfg.Margin >= cfg.TTL:
		return nil, fmt.Errorf("%w: lease ttl %v margin %v", ErrInvalidConfig, cfg.TTL, cfg.Margin)
	}
	return &LeasedBudget{cfg: cfg, leases: map[string]*heldLease{}}, nil
}

func unavailable(err error) error {
	if err == nil || errors.Is(err, ErrUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

// usable reports whether l may still be spent from at now.
func (b *LeasedBudget) usable(l *heldLease, now time.Duration) bool {
	return now < l.Expires-b.cfg.Margin
}

// closeLocked closes key's lease and returns its remainder to the store.
// The lease is removed before the return so it can never be spent again.
func (b *LeasedBudget) closeLocked(ctx context.Context, key string, now time.Duration) {
	l := b.leases[key]
	if l == nil {
		return
	}
	delete(b.leases, key)
	if l.left > 0 {
		// A failed return forfeits the remainder: undershoot, not overshoot.
		if err := b.cfg.Store.Return(ctx, l.ID, l.left, now); err != nil {
			b.forfeited += l.left
		}
	}
}

// Forfeited returns the number of unspent leased tokens this node could
// not return (the store was unreachable or had already pruned the lease).
func (b *LeasedBudget) Forfeited() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.forfeited
}

// ensureLocked makes key's lease hold at least cost tokens if the store
// grants them. It returns the denial retry hint when it cannot.
func (b *LeasedBudget) ensureLocked(ctx context.Context, key string, cost int, now time.Duration) (ok bool, retry time.Duration, err error) {
	if l := b.leases[key]; l != nil {
		if b.usable(l, now) && l.left >= cost {
			return true, 0, nil
		}
		b.closeLocked(ctx, key, now)
	}
	lease, err := b.cfg.Store.Lease(ctx, LeaseRequest{
		Key: key, Holder: b.cfg.Holder, Min: cost, Want: max(cost, b.cfg.Chunk), TTL: b.cfg.TTL, Now: now,
	})
	if err != nil {
		if errors.Is(err, ErrCostExceedsCapacity) || errors.Is(err, ErrNoRule) || errors.Is(err, ErrInvalidCost) ||
			errors.Is(err, ErrInvalidKey) || errors.Is(err, ErrInvalidTime) {
			return false, Never, err
		}
		return false, 0, unavailable(err)
	}
	if lease.Granted < cost {
		if lease.ID != "" {
			_ = b.cfg.Store.Return(ctx, lease.ID, lease.Granted, now)
		}
		return false, max(lease.RetryAfter, 1), nil
	}
	b.leases[key] = &heldLease{Lease: lease, left: lease.Granted}
	return true, 0, nil
}

// Acquire implements ContextLimiter.
func (b *LeasedBudget) Acquire(ctx context.Context, key string, cost int, now time.Duration) (Result, error) {
	return b.AcquireAll(ctx, []Request{{key, cost}}, now)
}

// AcquireAll implements ContextLimiter. Every key's lease is topped up
// first; tokens are spent only if all keys have enough, so a denial
// charges nothing (the leases stay held for later requests).
func (b *LeasedBudget) AcquireAll(ctx context.Context, reqs []Request, now time.Duration) (Result, error) {
	if len(reqs) == 0 {
		return Result{}, ErrEmptyRequest
	}
	merged := make([]Request, 0, len(reqs))
next:
	for _, q := range reqs {
		if q.Key == "" {
			return Result{}, ErrInvalidKey
		}
		if q.Cost < 1 {
			return Result{}, ErrInvalidCost
		}
		for i := range merged {
			if merged[i].Key == q.Key {
				merged[i].Cost += q.Cost
				continue next
			}
		}
		merged = append(merged, q)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	worst := time.Duration(0)
	for _, q := range merged {
		ok, retry, err := b.ensureLocked(ctx, q.Key, q.Cost, now)
		if err != nil {
			if retry == Never {
				return cannot(0), err
			}
			return Result{}, err
		}
		if !ok {
			worst = max(worst, retry)
		}
	}
	if worst > 0 {
		return Result{RetryAfter: worst}, nil
	}
	out := Result{Allowed: true, Remaining: math.MaxInt}
	for _, q := range merged {
		l := b.leases[q.Key]
		l.left -= q.Cost
		if l.left < out.Remaining {
			out.Remaining, out.Limit = l.left, l.Granted
		}
	}
	return out, nil
}

// Close returns every held lease's remainder to the store.
func (b *LeasedBudget) Close(ctx context.Context, now time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for key := range b.leases {
		b.closeLocked(ctx, key, now)
	}
}
