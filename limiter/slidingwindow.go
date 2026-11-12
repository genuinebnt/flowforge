package limiter

import "math"

// SlidingWindowCounter approximates a sliding window with two fixed
// windows (spec 14.2): with L = Burst and W = Window,
//
//	estimate = previous * (1 - elapsed_in_window / W) + current
//	allow when estimate + cost <= L
//
// Windows are aligned to multiples of W from the monotonic origin. The
// comparison is exact (128-bit integer arithmetic, no float rounding).
//
// The counter is an approximation: it assumes the previous window's
// requests were spread evenly, so a burst at the end of one window blocks
// the start of the next (lim_005) but an arbitrary sliding window of
// length W may hold up to about 2L. Its guarantee is the spec 25.4 bound:
// at most L + (L/W)*w in any interval of length w. SlidingWindowLog is the
// exact baseline.
type SlidingWindowCounter struct {
	*keyed[windowState, windowAlg]
}

// NewSlidingWindowCounter returns a SlidingWindowCounter configured with
// rules.
func NewSlidingWindowCounter(rules ...Rule) (*SlidingWindowCounter, error) {
	k, err := newKeyed[windowState, windowAlg](rules)
	if err != nil {
		return nil, err
	}
	return &SlidingWindowCounter{k}, nil
}

type windowState struct {
	idx       int64 // index of the current window
	cur, prev int64
	last      int64
	seen      bool
}

type windowAlg struct{}

// advance rolls the windows to now and returns the (clamped) time.
func (s *windowState) advance(w, now int64) int64 {
	if !s.seen {
		*s = windowState{idx: floorDiv(now, w), last: now, seen: true}
		return now
	}
	now = max(now, s.last)
	s.last = now
	switch i := floorDiv(now, w); {
	case i == s.idx+1:
		s.prev, s.cur, s.idx = s.cur, 0, i
	case i > s.idx+1:
		s.prev, s.cur, s.idx = 0, 0, i
	}
	return now
}

// fits reports whether prev*(W-e) + (cur+cost)*W <= L*W and returns the
// slack L*W - lhs when it does.
func fits(l, w, prev, cur, cost, e int64) (bool, u128) {
	lhs := mul(prev, w-e).add(mul(cur+cost, w))
	rhs := mul(l, w)
	if lhs.cmp(rhs) > 0 {
		return false, u128{}
	}
	return true, rhs.sub(lhs)
}

func (windowAlg) check(s *windowState, c *compiled, cost, now int64) decision {
	w, l := c.window, c.burst
	now = s.advance(w, now)
	e := now - s.idx*w
	if ok, slack := fits(l, w, s.prev, s.cur, cost, e); ok {
		return decision{allowed: true, remaining: slack.div(w), limit: l}
	}
	return decision{retry: windowRetry(l, w, s.prev, s.cur, cost, e), limit: l}
}

// windowRetry estimates how long until a denied request would fit. The
// estimate is computed in floating point and then nudged forward until the
// exact test passes, so it never under-reports by more than rounding of
// the final nanosecond.
func windowRetry(l, w, prev, cur, cost, e int64) int64 {
	if cur+cost <= l {
		// Wait inside this window for the previous window's weight to decay.
		at := w - int64(math.Floor(float64(l-cur-cost)*float64(w)/float64(prev)))
		for i := 0; i < 4 && at < w; i++ {
			if ok, _ := fits(l, w, prev, cur, cost, at); ok {
				break
			}
			at++
		}
		if at < w {
			return max(at-e, 1)
		}
		// Rounding pushed the answer past the window: fall through.
	}
	// Wait for the next window, in which this window becomes "previous".
	if cur == 0 {
		return w - e
	}
	at := max(w-int64(math.Floor(float64(l-cost)*float64(w)/float64(cur))), 0)
	for i := 0; i < 4 && at < w; i++ {
		if ok, _ := fits(l, w, cur, 0, cost, at); ok {
			break
		}
		at++
	}
	return w - e + at
}

func (windowAlg) charge(s *windowState, _ *compiled, cost, _ int64) {
	s.cur += cost
}

func (windowAlg) idle(s *windowState, c *compiled, now int64) bool {
	return !s.seen || floorDiv(max(now, s.last), c.window) > s.idx+1
}
