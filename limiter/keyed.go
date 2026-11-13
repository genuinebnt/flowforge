package limiter

import (
	"sync"
	"time"
)

// algorithm is the per-key logic of a mutex-guarded limiter. S is the
// per-key state; its zero value means "never seen".
type algorithm[S any] interface {
	// check brings s up to now (refill, window roll, eviction; never a
	// charge) and reports whether cost would pass.
	check(s *S, c *compiled, cost, now int64) decision
	// charge admits cost at now. It is called only right after a check
	// that allowed it.
	charge(s *S, c *compiled, cost, now int64)
	// idle reports whether s carries no information any more at now, so
	// the key can be forgotten without changing any future decision.
	idle(s *S, c *compiled, now int64) bool
}

// keyed is the shared engine of the mutex-based limiters: a rule table, a
// map of per-key state and one mutex. A single mutex keeps AcquireAll
// trivially atomic; AtomicGCRA is the sharded, lock-free alternative.
type keyed[S any, A algorithm[S]] struct {
	rules *Rules
	alg   A
	mu    sync.Mutex
	state map[string]*S
}

func newKeyed[S any, A algorithm[S]](rules []Rule) (*keyed[S, A], error) {
	r, err := NewRules(rules...)
	if err != nil {
		return nil, err
	}
	return &keyed[S, A]{rules: r, state: map[string]*S{}}, nil
}

// Configure implements Limiter.
func (k *keyed[S, A]) Configure(pattern string, p Params) error {
	return k.rules.Configure(pattern, p)
}

// slot returns the state for key, creating it. Callers hold k.mu.
func (k *keyed[S, A]) slot(key string) *S {
	s := k.state[key]
	if s == nil {
		s = new(S)
		k.state[key] = s
	}
	return s
}

// Acquire implements Limiter.
func (k *keyed[S, A]) Acquire(key string, cost int, now time.Duration) (Result, error) {
	c, err := k.rules.prepare(key, cost, now)
	if err != nil {
		if c != nil {
			return cannot(c.burst), err
		}
		return Result{}, err
	}
	k.mu.Lock()
	s := k.slot(key)
	d := k.alg.check(s, c, int64(cost), int64(now))
	if d.allowed {
		k.alg.charge(s, c, int64(cost), int64(now))
	}
	k.mu.Unlock()
	return d.result(), nil
}

// AcquireAll implements Limiter.
func (k *keyed[S, A]) AcquireAll(reqs []Request, now time.Duration) (Result, error) {
	var buf [8]prepared
	ps, res, err := k.rules.prepareAll(reqs, now, buf[:])
	if err != nil {
		return res, err
	}
	var dbuf [8]decision
	var sbuf [8]*S
	ds, ss := dbuf[:0], sbuf[:0]
	n := int64(now)
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, p := range ps {
		s := k.slot(p.key)
		ss = append(ss, s)
		ds = append(ds, k.alg.check(s, p.c, p.cost, n))
	}
	out := combine(ds)
	if out.Allowed {
		for i, p := range ps {
			k.alg.charge(ss[i], p.c, p.cost, n)
		}
	}
	return out, nil
}

// Prune forgets every key whose state no longer affects any decision at
// or after now and returns how many it removed. Call it periodically to
// bound memory; a clock that later reads earlier than now may then see
// slightly more lenient decisions for the pruned keys.
func (k *keyed[S, A]) Prune(now time.Duration) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	n := 0
	for key, s := range k.state {
		c, err := k.rules.lookup(key)
		if err != nil || k.alg.idle(s, c, int64(now)) {
			delete(k.state, key)
			n++
		}
	}
	return n
}

// Len returns the number of keys with state.
func (k *keyed[S, A]) Len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.state)
}
