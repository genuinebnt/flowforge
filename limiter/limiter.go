package limiter

import (
	"context"
	"math"
	"time"
)

// Never is the RetryAfter of a request that can never pass, such as one
// whose cost exceeds the key's capacity. It always comes with an error.
const Never time.Duration = math.MaxInt64

// MaxTime bounds the time arguments the limiters accept (about 146 years
// either side of the monotonic origin). Keeping times and parameter spans
// below 2^62 and 2^61 nanoseconds lets every algorithm use plain int64
// arithmetic without overflow.
const MaxTime time.Duration = 1 << 62

// Result is the outcome of one acquire (spec 14.1).
type Result struct {
	// Allowed reports whether the request was admitted and charged.
	Allowed bool
	// RetryAfter is the earliest time, relative to now, at which the same
	// request could pass. It is zero when Allowed is true and Never when
	// the request can never pass.
	RetryAfter time.Duration
	// Remaining is a best-effort count of further unit-cost requests that
	// would pass at the same instant. For AcquireAll it is the minimum over
	// the keys.
	Remaining int
	// Limit is the burst (or window limit, or queue length) of the key that
	// decided the result: the strictest key for AcquireAll.
	Limit int
	// Delay is set only by shaping algorithms (LeakyBucket): the admitted
	// request must wait this long before it is released. Policing
	// algorithms always leave it zero.
	Delay time.Duration
}

// Request is one (key, cost) pair of an AcquireAll call.
type Request struct {
	Key  string
	Cost int
}

// Limiter is the in-process contract every algorithm implements (spec
// 14.1). Time is always an argument: now is a monotonic reading such as
// core.Clock.Monotonic(), so every algorithm is testable with a fake clock.
// Implementations are safe for concurrent use.
type Limiter interface {
	// Acquire charges cost against key if the key's budget allows it.
	Acquire(key string, cost int, now time.Duration) (Result, error)
	// AcquireAll charges every request atomically: if any key denies, none
	// is charged and RetryAfter is the maximum of the denials. Requests
	// naming the same key are merged by summing their costs.
	AcquireAll(reqs []Request, now time.Duration) (Result, error)
	// Configure sets the parameters for keys matching pattern (see Rules).
	Configure(pattern string, p Params) error
}

// ContextLimiter is the contract of limiters that do I/O (a central
// store, a lease server). Errors caused by the store wrap ErrUnavailable.
type ContextLimiter interface {
	Acquire(ctx context.Context, key string, cost int, now time.Duration) (Result, error)
	AcquireAll(ctx context.Context, reqs []Request, now time.Duration) (Result, error)
}

// decision is the algorithm-internal form of a Result, in nanoseconds.
type decision struct {
	allowed   bool
	retry     int64
	remaining int64
	limit     int64
	delay     int64
}

func (d decision) result() Result {
	return Result{
		Allowed:    d.allowed,
		RetryAfter: time.Duration(d.retry),
		Remaining:  int(d.remaining),
		Limit:      int(d.limit),
		Delay:      time.Duration(d.delay),
	}
}

// combine folds the per-key decisions of an AcquireAll into one Result. If
// any key denies, the result is denied with the largest retry; otherwise it
// carries the strictest key's remaining count and the largest delay.
func combine(ds []decision) Result {
	out := decision{allowed: true, remaining: math.MaxInt64}
	for _, d := range ds {
		if !d.allowed {
			if out.allowed || d.retry > out.retry {
				out = decision{retry: d.retry, limit: d.limit}
			}
			continue
		}
		if !out.allowed {
			continue
		}
		if d.remaining < out.remaining {
			out.remaining, out.limit = d.remaining, d.limit
		}
		out.delay = max(out.delay, d.delay)
	}
	return out.result()
}

// cannot is the Result returned with ErrCostExceedsCapacity.
func cannot(limit int64) Result {
	return Result{RetryAfter: Never, Limit: int(limit)}
}
