package alerts

import (
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// Delivery retry policy (spec 15.1): backoff 10 s, 30 s, 2 min, 10 min,
// then 1 h between attempts, each with full jitter; give up once the next
// attempt would start more than GiveUpAfter after the first attempt.
const (
	GiveUpAfter     = 24 * time.Hour
	MinRetryDelay   = time.Second // floor under full jitter so retries never spin
	retryCeilingGap = time.Hour
)

// RetryBackoff returns the un-jittered delay before retry number attempt
// (1 after the first failed attempt): 10s, 30s, 2m, 10m, then 1h.
func RetryBackoff(attempt int) time.Duration {
	switch {
	case attempt <= 1:
		return 10 * time.Second
	case attempt == 2:
		return 30 * time.Second
	case attempt == 3:
		return 2 * time.Minute
	case attempt == 4:
		return 10 * time.Minute
	}
	return retryCeilingGap
}

// RetryDelay applies full jitter to RetryBackoff: uniform in
// [MinRetryDelay, backoff].
func RetryDelay(attempt int, rng core.Rng) time.Duration {
	base := RetryBackoff(attempt)
	d := time.Duration(core.Float64(rng) * float64(base+1))
	return min(max(d, MinRetryDelay), base)
}

// NextAttempt schedules the retry after failed attempt number attempts
// (1-based), made at failedAt, of a delivery first attempted at first. ok
// is false when the delivery must be marked failed instead.
func NextAttempt(attempts int, first, failedAt time.Time, rng core.Rng) (next time.Time, ok bool) {
	next = failedAt.Add(RetryDelay(attempts, rng))
	if next.Sub(first) > GiveUpAfter {
		return time.Time{}, false
	}
	return next, true
}
