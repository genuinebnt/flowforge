package limiter

// LeakyBucket is the leaky bucket as a queue (spec 14.2): requests enter a
// bounded FIFO of Burst cost units per key and leave it at Rate units per
// second. It shapes instead of policing: an admitted request carries a
// Result.Delay, the time it must wait before it is released, and a request
// that would overflow the queue is denied with the time until enough of
// the queue has drained.
//
// The model is virtual scheduling: per key it keeps the time at which the
// queue drains (next_free). A request of cost c at now is released at
// start = max(next_free, now) and occupies the output for c*T, so
//
//	admit when start + c*T - now <= Burst*T; Delay = start - now
//	next_free = start + c*T
//
// Admission is therefore identical to GCRA with the same parameters; the
// difference is that callers wait out Delay instead of retrying. Nothing
// is held in memory per queued request, and callers do the waiting with
// their own timers, so the limiter stays a pure function of its inputs.
type LeakyBucket struct {
	*keyed[gcraState, leakyAlg]
}

// NewLeakyBucket returns a LeakyBucket configured with rules.
func NewLeakyBucket(rules ...Rule) (*LeakyBucket, error) {
	k, err := newKeyed[gcraState, leakyAlg](rules)
	if err != nil {
		return nil, err
	}
	return &LeakyBucket{k}, nil
}

type leakyAlg struct{}

func (leakyAlg) check(s *gcraState, c *compiled, cost, now int64) decision {
	tat := s.get()
	_, d := gcraDecide(tat, c, cost, now)
	if d.allowed {
		d.delay = max(tat, now) - now
	}
	return d
}

func (leakyAlg) charge(s *gcraState, c *compiled, cost, now int64) {
	gcraAlg{}.charge(s, c, cost, now)
}

func (leakyAlg) idle(s *gcraState, c *compiled, now int64) bool {
	return gcraAlg{}.idle(s, c, now)
}
