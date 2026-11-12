package limiter

import (
	"errors"
	"testing"
	"time"
)

func ms(n int64) time.Duration { return time.Duration(n) * time.Millisecond }

// ceilMs rounds d up to whole milliseconds, the unit of the data files.
func ceilMs(d time.Duration) int64 { return int64((d + time.Millisecond - 1) / time.Millisecond) }

// variant is a constructor for one keyed Limiter implementation.
type variant struct {
	name string
	make func(rules ...Rule) (Limiter, error)
}

func gcraVariants() []variant {
	return []variant{
		{"gcra", func(r ...Rule) (Limiter, error) { return NewGCRA(r...) }},
		{"gcra_atomic", func(r ...Rule) (Limiter, error) { return NewAtomicGCRA(r...) }},
		{"leaky_bucket", func(r ...Rule) (Limiter, error) { return NewLeakyBucket(r...) }},
	}
}

func allVariants() []variant {
	return append(gcraVariants(),
		variant{"token_bucket", func(r ...Rule) (Limiter, error) { return NewTokenBucket(r...) }},
		variant{"sliding_window_counter", func(r ...Rule) (Limiter, error) { return NewSlidingWindowCounter(r...) }},
		variant{"sliding_window_log", func(r ...Rule) (Limiter, error) { return NewSlidingWindowLog(r...) }},
		variant{"crdt", func(r ...Rule) (Limiter, error) { return NewCRDTLimiter("n1", r...) }},
	)
}

func mustNew(t testing.TB, v variant, rules ...Rule) Limiter {
	t.Helper()
	l, err := v.make(rules...)
	if err != nil {
		t.Fatalf("%s: %v", v.name, err)
	}
	return l
}

func all(p Params) Rule { return Rule{Pattern: "*", Params: p} }

// errorCode maps the error names used in the conformance data.
func errorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrCostExceedsCapacity):
		return "cost_exceeds_capacity"
	case errors.Is(err, ErrInvalidCost):
		return "invalid_cost"
	default:
		return err.Error()
	}
}

// stepExpect is the expected outcome of one acquire in the data files.
type stepExpect struct {
	Allow        bool   `json:"allow"`
	RetryAfterMs *int64 `json:"retry_after_ms"`
	Remaining    *int   `json:"remaining"`
	Error        string `json:"error"`
}

type step struct {
	TMs    int64      `json:"t_ms"`
	Cost   int        `json:"cost"`
	Expect stepExpect `json:"expect"`
}

type caseConfig struct {
	RatePerS float64 `json:"rate_per_s"`
	Burst    int     `json:"burst"`
	WindowMs int64   `json:"window_ms"`
}

func (c caseConfig) params() Params {
	return Params{Rate: c.RatePerS, Burst: c.Burst, Window: ms(c.WindowMs)}
}

// checkStep compares one result with its expectation.
func checkStep(t *testing.T, where string, got Result, err error, want stepExpect) {
	t.Helper()
	if code := errorCode(err); code != want.Error {
		t.Fatalf("%s: error %q, want %q", where, code, want.Error)
	}
	if err != nil {
		if got.Allowed {
			t.Fatalf("%s: allowed together with error %v", where, err)
		}
		if errors.Is(err, ErrCostExceedsCapacity) && got.RetryAfter != Never {
			t.Fatalf("%s: RetryAfter = %v with %v, want Never", where, got.RetryAfter, err)
		}
		return
	}
	if got.Allowed != want.Allow {
		t.Fatalf("%s: allowed = %v, want %v (result %+v)", where, got.Allowed, want.Allow, got)
	}
	if got.Allowed && got.RetryAfter != 0 {
		t.Fatalf("%s: allowed with RetryAfter %v", where, got.RetryAfter)
	}
	if want.RetryAfterMs != nil && ceilMs(got.RetryAfter) != *want.RetryAfterMs {
		t.Fatalf("%s: retry_after = %v, want %d ms", where, got.RetryAfter, *want.RetryAfterMs)
	}
	if want.Remaining != nil && got.Remaining != *want.Remaining {
		t.Fatalf("%s: remaining = %d, want %d", where, got.Remaining, *want.Remaining)
	}
}
