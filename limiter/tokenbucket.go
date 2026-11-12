package limiter

import "math"

// TokenBucket is the classic token bucket (spec 14.2): a bucket of Burst
// tokens refilled at Rate tokens per second; a request passes when the
// bucket holds at least its cost.
//
// Tokens are kept in fixed point (1e9 units per token, so the refill is
// elapsed_ns * Rate units) instead of the spec's float, which makes the
// bucket exact for integer rates and lets tests compare it with a
// reference model without tolerances. A key starts full. A time earlier
// than the last one seen refills nothing and is otherwise treated as that
// last time, so a backwards clock can never mint tokens (lim_003).
type TokenBucket struct {
	*keyed[bucketState, tokenBucketAlg]
}

// NewTokenBucket returns a TokenBucket limiter configured with rules.
func NewTokenBucket(rules ...Rule) (*TokenBucket, error) {
	k, err := newKeyed[bucketState, tokenBucketAlg](rules)
	if err != nil {
		return nil, err
	}
	return &TokenBucket{k}, nil
}

// bucketState is a fixed-point token bucket. It is shared by TokenBucket,
// AIMD pacing and the in-memory lease store.
type bucketState struct {
	units int64   // tokens * unit, in [0, capUnits]
	frac  float64 // sub-unit refill carried to the next refill, in [0, 1)
	last  int64   // time of the last refill
	seen  bool
}

// refill brings the bucket to now at rate units per nanosecond.
func (s *bucketState) refill(capUnits int64, rate float64, now int64) {
	if !s.seen {
		*s = bucketState{units: capUnits, last: now, seen: true}
		return
	}
	if s.units >= capUnits {
		s.units, s.frac = capUnits, 0 // also clamps after a lowered burst
		s.last = max(s.last, now)
		return
	}
	if now <= s.last {
		return
	}
	add := float64(now-s.last)*rate + s.frac
	s.last = now
	if add >= float64(capUnits-s.units) {
		s.units, s.frac = capUnits, 0
		return
	}
	whole := math.Floor(add)
	s.units += int64(whole)
	s.frac = add - whole
}

// take checks cost tokens against the refilled bucket without charging.
func (s *bucketState) take(c *compiled, rate float64, cost, now int64) decision {
	s.refill(c.capUnits, rate, now)
	need := cost * unit
	if s.units >= need {
		return decision{allowed: true, remaining: (s.units - need) / unit, limit: c.burst}
	}
	retry := int64(math.Ceil((float64(need-s.units) - s.frac) / rate))
	return decision{retry: max(retry, 1), limit: c.burst}
}

type tokenBucketAlg struct{}

func (tokenBucketAlg) check(s *bucketState, c *compiled, cost, now int64) decision {
	return s.take(c, c.rate, cost, now)
}

func (tokenBucketAlg) charge(s *bucketState, _ *compiled, cost, _ int64) {
	s.units -= cost * unit
}

func (tokenBucketAlg) idle(s *bucketState, c *compiled, now int64) bool {
	cp := *s
	cp.refill(c.capUnits, c.rate, now)
	return cp.units >= c.capUnits
}
