package limiter

import "errors"

// Sentinel errors. Callers match them with errors.Is; the limiter wraps
// them with the offending key or value.
var (
	// ErrCostExceedsCapacity reports a request whose cost is larger than the
	// key's burst (or window limit, or queue length). Such a request can
	// never pass, so the limiter answers with an error and a RetryAfter of
	// Never instead of a retry hint the caller would wait on forever
	// (spec lim_004).
	ErrCostExceedsCapacity = errors.New("limiter: cost exceeds capacity")
	// ErrInvalidCost reports a cost below 1.
	ErrInvalidCost = errors.New("limiter: cost must be at least 1")
	// ErrInvalidTime reports a time argument outside [-MaxTime, MaxTime].
	ErrInvalidTime = errors.New("limiter: time out of range")
	// ErrInvalidParams reports parameters that fail Params.Validate.
	ErrInvalidParams = errors.New("limiter: invalid params")
	// ErrInvalidPattern reports a malformed key pattern.
	ErrInvalidPattern = errors.New("limiter: invalid key pattern")
	// ErrNoRule reports a key that matches no configured pattern. The
	// limiter fails closed: an unconfigured key is an error, not an allow.
	ErrNoRule = errors.New("limiter: no rule matches key")
	// ErrEmptyRequest reports an AcquireAll call without requests.
	ErrEmptyRequest = errors.New("limiter: no requests")
	// ErrInvalidKey reports an empty key or key component.
	ErrInvalidKey = errors.New("limiter: invalid key")
	// ErrUnavailable reports that a distributed limiter's backing store
	// could not be reached. Adapters wrap their I/O errors with it so
	// Failover can tell an outage from a bad request (spec 23.8).
	ErrUnavailable = errors.New("limiter: store unavailable")
	// ErrUnknownLease reports a Return for a lease the store does not know
	// (never issued, already returned or already pruned).
	ErrUnknownLease = errors.New("limiter: unknown lease")
	// ErrInvalidConfig reports a bad configuration struct (AIMD, WFQ,
	// leased budgets).
	ErrInvalidConfig = errors.New("limiter: invalid config")
	// ErrQueueFull reports a WFQ enqueue beyond the per-tenant bound.
	ErrQueueFull = errors.New("limiter: queue full")
)
