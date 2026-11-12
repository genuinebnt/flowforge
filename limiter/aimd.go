package limiter

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// Outcome classifies the response of a target for adaptive limiting.
type Outcome int

// Outcomes. Client errors other than 429 count as OutcomeOK: they say
// nothing about the target's capacity.
const (
	OutcomeOK          Outcome = iota // success (or a non-429 client error)
	OutcomeThrottled                  // HTTP 429 or an equivalent signal
	OutcomeTimeout                    // no answer in time
	OutcomeServerError                // HTTP 5xx
)

// AIMDConfig configures an AIMD controller.
type AIMDConfig struct {
	// MinRate and MaxRate bound the rate, in requests per second
	// (0 < MinRate <= MaxRate).
	MinRate, MaxRate float64
	// InitialRate is the starting rate; 0 means MinRate (start cautiously
	// and probe upwards).
	InitialRate float64
	// Window is N, the number of outcomes per evaluation (>= 1).
	Window int
	// ErrorThreshold is the fraction of 5xx outcomes in a window above
	// which the rate decreases, in [0, 1].
	ErrorThreshold float64
	// Increase is the additive step a; 0 means 0.1 * MaxRate.
	Increase float64
	// Decrease is the multiplicative factor; 0 means 0.5.
	Decrease float64
	// Burst is the pacing bucket size used by Acquire; 0 means 1.
	Burst int
}

// AIMD is an adaptive rate controller for one target (spec 14.2). After
// every Window outcomes it evaluates the window:
//
//	if any 429, any timeout, or 5xx fraction > ErrorThreshold:
//	    rate = max(MinRate, rate * Decrease)    // multiplicative decrease
//	else:
//	    rate = min(MaxRate, rate + Increase)    // additive increase
//
// A Retry-After passed to Observe suspends Acquire until it has passed.
// Acquire paces requests with a token bucket of Burst tokens refilled at
// the current rate. Callers keep one AIMD per target key. It is safe for
// concurrent use.
type AIMD struct {
	cfg    AIMDConfig
	bucket *compiled // capacity of the pacing bucket

	mu                            sync.Mutex
	rate                          float64
	n, throttled, timeouts, fails int
	suspendedUntil                int64
	suspended                     bool
	pace                          bucketState
}

// NewAIMD validates cfg, fills in defaults and returns a controller.
func NewAIMD(cfg AIMDConfig) (*AIMD, error) {
	bad := func(f string, v any) error { return fmt.Errorf("%w: aimd %s = %v", ErrInvalidConfig, f, v) }
	finite := func(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }
	if cfg.InitialRate == 0 {
		cfg.InitialRate = cfg.MinRate
	}
	if cfg.Increase == 0 {
		cfg.Increase = 0.1 * cfg.MaxRate
	}
	if cfg.Decrease == 0 {
		cfg.Decrease = 0.5
	}
	if cfg.Burst == 0 {
		cfg.Burst = 1
	}
	switch {
	case !finite(cfg.MinRate) || cfg.MinRate <= 0 || cfg.MinRate > 1e9:
		return nil, bad("MinRate", cfg.MinRate)
	case !finite(cfg.MaxRate) || cfg.MaxRate < cfg.MinRate || cfg.MaxRate > 1e9:
		return nil, bad("MaxRate", cfg.MaxRate)
	case !finite(cfg.InitialRate) || cfg.InitialRate < cfg.MinRate || cfg.InitialRate > cfg.MaxRate:
		return nil, bad("InitialRate", cfg.InitialRate)
	case cfg.Window < 1:
		return nil, bad("Window", cfg.Window)
	case !finite(cfg.ErrorThreshold) || cfg.ErrorThreshold < 0 || cfg.ErrorThreshold > 1:
		return nil, bad("ErrorThreshold", cfg.ErrorThreshold)
	case !finite(cfg.Increase) || cfg.Increase < 0:
		return nil, bad("Increase", cfg.Increase)
	case !finite(cfg.Decrease) || cfg.Decrease <= 0 || cfg.Decrease >= 1:
		return nil, bad("Decrease", cfg.Decrease)
	}
	c, err := compile(Params{Rate: cfg.MinRate, Burst: cfg.Burst})
	if err != nil {
		return nil, err
	}
	return &AIMD{cfg: cfg, bucket: c, rate: cfg.InitialRate}, nil
}

// Rate returns the current rate in requests per second.
func (a *AIMD) Rate() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rate
}

// Observe records the outcome of one request at now and returns the rate
// after it. retryAfter is the target's Retry-After (0 if none); a positive
// value suspends Acquire until now + retryAfter.
func (a *AIMD) Observe(o Outcome, retryAfter, now time.Duration) float64 {
	now = min(max(now, -MaxTime), MaxTime) // keep the arithmetic in range
	a.mu.Lock()
	defer a.mu.Unlock()
	if retryAfter > 0 {
		until := int64(now) + int64(min(retryAfter, MaxTime))
		if !a.suspended || until > a.suspendedUntil {
			a.suspendedUntil, a.suspended = until, true
		}
	}
	switch o {
	case OutcomeThrottled:
		a.throttled++
	case OutcomeTimeout:
		a.timeouts++
	case OutcomeServerError:
		a.fails++
	}
	a.n++
	if a.n < a.cfg.Window {
		return a.rate
	}
	// Tokens accrued so far were earned at the old rate.
	a.pace.refill(a.bucket.capUnits, a.rate, int64(now))
	if a.throttled > 0 || a.timeouts > 0 || float64(a.fails) > a.cfg.ErrorThreshold*float64(a.n) {
		a.rate = max(a.cfg.MinRate, a.rate*a.cfg.Decrease)
	} else {
		a.rate = min(a.cfg.MaxRate, a.rate+a.cfg.Increase)
	}
	a.n, a.throttled, a.timeouts, a.fails = 0, 0, 0, 0
	return a.rate
}

// Acquire paces one request of cost at the current rate. While a
// Retry-After suspension is active it denies with the remaining time.
func (a *AIMD) Acquire(cost int, now time.Duration) (Result, error) {
	if now > MaxTime || now < -MaxTime {
		return Result{}, ErrInvalidTime
	}
	if cost < 1 {
		return Result{}, ErrInvalidCost
	}
	if int64(cost) > a.bucket.burst {
		return cannot(a.bucket.burst), ErrCostExceedsCapacity
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.suspended && int64(now) < a.suspendedUntil {
		return Result{RetryAfter: time.Duration(a.suspendedUntil - int64(now)), Limit: int(a.bucket.burst)}, nil
	}
	d := a.pace.take(a.bucket, a.rate, int64(cost), int64(now))
	if d.allowed {
		a.pace.units -= int64(cost) * unit
	}
	return d.result(), nil
}
