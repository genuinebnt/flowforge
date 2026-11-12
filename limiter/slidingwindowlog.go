package limiter

// SlidingWindowLog is the exact sliding window (spec 14.2): it keeps the
// time and cost of every admitted request in the last W = Window and
// admits a request when the admitted cost in (now - W, now] plus its cost
// is at most L = Burst. Memory grows with L per key; it is the accuracy
// baseline for the other algorithms.
type SlidingWindowLog struct {
	*keyed[logState, logAlg]
}

// NewSlidingWindowLog returns a SlidingWindowLog configured with rules.
func NewSlidingWindowLog(rules ...Rule) (*SlidingWindowLog, error) {
	k, err := newKeyed[logState, logAlg](rules)
	if err != nil {
		return nil, err
	}
	return &SlidingWindowLog{k}, nil
}

type logEntry struct{ t, cost int64 }

// logState is a ring buffer of admitted requests, oldest first.
type logState struct {
	buf     []logEntry
	head, n int
	sum     int64
	last    int64
	seen    bool
}

func (s *logState) at(i int) *logEntry { return &s.buf[(s.head+i)%len(s.buf)] }

// evict drops entries outside (now - w, now] and returns the clamped time.
func (s *logState) evict(w, now int64) int64 {
	if s.seen {
		now = max(now, s.last)
	}
	s.last, s.seen = now, true
	for s.n > 0 {
		e := s.at(0)
		if e.t > now-w {
			break
		}
		s.sum -= e.cost
		s.head = (s.head + 1) % len(s.buf)
		s.n--
	}
	return now
}

func (s *logState) push(t, cost int64) {
	if s.n > 0 {
		if e := s.at(s.n - 1); e.t == t {
			e.cost += cost
			s.sum += cost
			return
		}
	}
	if s.n == len(s.buf) {
		grown := make([]logEntry, max(4, 2*len(s.buf)))
		for i := 0; i < s.n; i++ {
			grown[i] = *s.at(i)
		}
		s.buf, s.head = grown, 0
	}
	*s.at(s.n) = logEntry{t, cost}
	s.n++
	s.sum += cost
}

type logAlg struct{}

func (logAlg) check(s *logState, c *compiled, cost, now int64) decision {
	now = s.evict(c.window, now)
	if s.sum+cost <= c.burst {
		return decision{allowed: true, remaining: c.burst - s.sum - cost, limit: c.burst}
	}
	// Find the oldest entry whose eviction makes room.
	acc := int64(0)
	for i := 0; i < s.n; i++ {
		e := s.at(i)
		acc += e.cost
		if s.sum-acc+cost <= c.burst {
			return decision{retry: max(e.t+c.window-now, 1), limit: c.burst}
		}
	}
	// Unreachable for cost <= burst; answer conservatively.
	return decision{retry: c.window, limit: c.burst}
}

func (logAlg) charge(s *logState, _ *compiled, cost, now int64) {
	s.push(max(now, s.last), cost)
}

func (logAlg) idle(s *logState, c *compiled, now int64) bool {
	cp := *s
	cp.evict(c.window, now)
	return cp.n == 0
}
