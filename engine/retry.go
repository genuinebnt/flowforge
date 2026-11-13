package engine

import (
	"math"
	"slices"
	"time"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/core"
)

// Default backoff when a tool declares retries without a backoff.
const (
	defaultInitialMs  = 1000
	defaultMaxMs      = 30000
	defaultMultiplier = 2.0
)

// effectivePolicy returns the retry policy of one tool call: the tool's
// policy, overridden by #[retry(max = n, backoff = exp(initial, max))] on
// the let statement around the call (spec 7.5), with defaults filled in.
func effectivePolicy(spec ToolSpec, site bytecode.SiteAnnotation) RetryPolicy {
	p := spec.Retry
	if site.RetryMax > 0 {
		p.MaxAttempts = int(site.RetryMax)
		if site.BackoffInitial > 0 {
			p.Backoff.InitialMs = time.Duration(site.BackoffInitial).Milliseconds()
		}
		if site.BackoffMax > 0 {
			p.Backoff.MaxMs = time.Duration(site.BackoffMax).Milliseconds()
		}
		p.Backoff.Multiplier = defaultMultiplier
	}
	if p.MaxAttempts < 1 {
		p.MaxAttempts = 1
	}
	if p.Backoff.InitialMs <= 0 {
		p.Backoff.InitialMs = defaultInitialMs
	}
	if p.Backoff.MaxMs <= 0 {
		p.Backoff.MaxMs = max(defaultMaxMs, p.Backoff.InitialMs)
	}
	if p.Backoff.Multiplier < 1 {
		p.Backoff.Multiplier = defaultMultiplier
	}
	return p
}

// BackoffDelay is delay(n) = min(max_ms, initial_ms * multiplier^(n-1))
// for n failed attempts so far (spec 9.5), computed in float64
// milliseconds (math.Pow) and truncated to nanoseconds.
//
// PORT: a max_ms above about 9.2e12 (292 years) overflows the final
// conversion, which Go leaves implementation-defined and Rust's `as`
// saturates; registries never hold such values, but clamp explicitly if
// the port must match on them.
func BackoffDelay(b Backoff, n int) time.Duration {
	if n < 1 {
		n = 1
	}
	ms := float64(b.InitialMs) * math.Pow(b.Multiplier, float64(n-1))
	if ms > float64(b.MaxMs) || math.IsInf(ms, 0) || math.IsNaN(ms) {
		ms = float64(b.MaxMs)
	}
	return time.Duration(ms * float64(time.Millisecond))
}

// FullJitter draws the wait uniformly from [0, d] ("full jitter",
// spec 9.5) with the injected randomness: one Uint64 modulo d+1
// nanoseconds, so seeded tests reproduce exactly.
func FullJitter(rng core.Rng, d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rng.Uint64() % (uint64(d) + 1))
}

// retryable reports whether a tool error may be retried under policy p:
// the tool must mark it retryable and, when the policy lists codes, its
// code must be one of them.
func retryable(p RetryPolicy, te EffectError) bool {
	return te.Retryable && (len(p.RetryOn) == 0 || slices.Contains(p.RetryOn, te.Code))
}
