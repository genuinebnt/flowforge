package limiter

// GCRA is the generic cell rate algorithm, the default limiter (spec
// 14.2): one theoretical arrival time (TAT) per key.
//
//	inc      = T * cost
//	new_tat  = max(tat, now) + inc
//	allow_at = new_tat - tau - T
//	deny when now < allow_at (retry after allow_at - now), else tat = new_tat
//
// A key with no state behaves as if its TAT were in the distant past (the
// spec's "initially 0" for non-negative clocks). GCRA serializes keys
// behind one mutex; AtomicGCRA is the lock-free, sharded variant with
// identical decisions.
type GCRA struct {
	*keyed[gcraState, gcraAlg]
}

// NewGCRA returns a GCRA limiter configured with rules.
func NewGCRA(rules ...Rule) (*GCRA, error) {
	k, err := newKeyed[gcraState, gcraAlg](rules)
	if err != nil {
		return nil, err
	}
	return &GCRA{k}, nil
}

type gcraState struct {
	tat  int64
	seen bool
}

type gcraAlg struct{}

// gcraNever is the TAT of a key without history.
const gcraNever = -1 << 63

// gcraDecide is the pure GCRA step shared by every GCRA variant. tat is
// gcraNever for a key without history. It returns the TAT to store if the
// request is admitted.
func gcraDecide(tat int64, c *compiled, cost, now int64) (int64, decision) {
	base := max(tat, now)
	newTat := base + c.t*cost
	allowAt := newTat - c.tau - c.t
	if now < allowAt {
		return tat, decision{retry: allowAt - now, limit: c.burst}
	}
	return newTat, decision{allowed: true, remaining: (c.tau + c.t - (newTat - now)) / c.t, limit: c.burst}
}

func (s *gcraState) get() int64 {
	if !s.seen {
		return gcraNever
	}
	return s.tat
}

func (gcraAlg) check(s *gcraState, c *compiled, cost, now int64) decision {
	_, d := gcraDecide(s.get(), c, cost, now)
	return d
}

func (gcraAlg) charge(s *gcraState, c *compiled, cost, now int64) {
	s.tat, _ = gcraDecide(s.get(), c, cost, now)
	s.seen = true
}

func (gcraAlg) idle(s *gcraState, _ *compiled, now int64) bool {
	return !s.seen || s.tat <= now
}
