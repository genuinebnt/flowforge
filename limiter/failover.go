package limiter

import (
	"context"
	"errors"
	"time"
)

// Work says whether a request is active work (it touches a target) or
// passive work (it reads third-party sources), which decides how it is
// treated during a limiter-store outage.
type Work int

// Work kinds.
const (
	WorkActive Work = iota
	WorkPassive
)

// Failover applies the outage policy of spec 14.4 and 23.8 to a
// distributed limiter. While Primary works, its answer stands. When it
// fails with ErrUnavailable, active work is denied (fail closed: per-target
// rates never exceed their ceiling) and passive work falls back to Local,
// a conservative in-process limiter configured with reduced rates. A nil
// Local denies passive work too.
type Failover struct {
	Primary ContextLimiter
	Local   Limiter
}

// Acquire acquires one key under the outage policy.
func (f Failover) Acquire(ctx context.Context, key string, cost int, work Work, now time.Duration) (Result, error) {
	return f.AcquireAll(ctx, []Request{{key, cost}}, work, now)
}

// AcquireAll acquires several keys atomically under the outage policy.
func (f Failover) AcquireAll(ctx context.Context, reqs []Request, work Work, now time.Duration) (Result, error) {
	res, err := f.Primary.AcquireAll(ctx, reqs, now)
	if err == nil || !errors.Is(err, ErrUnavailable) {
		return res, err
	}
	if work == WorkPassive && f.Local != nil {
		return f.Local.AcquireAll(reqs, now)
	}
	return Result{}, err
}
