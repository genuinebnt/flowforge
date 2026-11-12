package limiter

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// TestLim003_BackwardsClock feeds every algorithm timelines whose readings
// sometimes jump backwards. Nothing panics, results and internal counters
// stay in range, and a backwards reading creates no allowance: placing each
// admitted request at the latest reading seen so far, the spec 25.4 window
// bound still holds. The algorithms that clamp time explicitly (token
// bucket, sliding windows) must moreover decide exactly as if every reading
// had been clamped.
func TestLim003_BackwardsClock(t *testing.T) {
	r := testkit.Rng(t)
	clamps := map[string]bool{"token_bucket": true, "sliding_window_counter": true, "sliding_window_log": true}
	for _, v := range allVariants() {
		t.Run(v.name, func(t *testing.T) {
			for range testkit.Cases() / 4 {
				p := genParams(r)
				raw := mustNew(t, v, all(p))
				clamped := mustNew(t, v, all(p))
				now, latest := int64(0), int64(math.MinInt64)
				var events []admit
				for range 200 {
					if core.IntN(r, 4) == 0 {
						now -= int64(core.IntN(r, 2000)) * int64(time.Millisecond) // backwards
					} else {
						now += genGap(r, p)
					}
					latest = max(latest, now)
					cost := genCost(r, p)
					a, err := raw.Acquire("k", cost, time.Duration(now))
					if err != nil {
						t.Fatal(err)
					}
					if a.Remaining < 0 || a.Remaining > p.Burst || a.RetryAfter < 0 || (a.Allowed && a.RetryAfter != 0) {
						t.Fatalf("bad result %+v", a)
					}
					if a.Allowed {
						events = append(events, admit{latest, int64(cost)})
					}
					if clamps[v.name] {
						b, _ := clamped.Acquire("k", cost, time.Duration(latest))
						if a != b {
							t.Fatalf("now=%v latest=%v: raw %+v, clamped %+v", time.Duration(now), time.Duration(latest), a, b)
						}
					}
				}
				if msg := windowViolation(events, boundFor(v.name, p)); msg != "" {
					t.Fatalf("%+v: %s", p, msg)
				}
				checkInternal(t, raw)
			}
		})
	}
	t.Run("conformance", func(t *testing.T) {
		var c gcraCase
		for _, cf := range testkit.LoadCases[gcraCase](t, "limiter/gcra") {
			if cf.Name == "backwards.json" {
				c = cf.Data
			}
		}
		if c.ID == "" {
			t.Fatal("backwards.json missing")
		}
		for _, v := range gcraVariants() {
			runSteps(t, c.ID, mustNew(t, v, all(c.Config.params())), c.Steps)
		}
	})
}

// checkInternal asserts the state invariants of the keyed limiters.
func checkInternal(t *testing.T, l Limiter) {
	t.Helper()
	switch l := l.(type) {
	case *TokenBucket:
		for k, s := range l.state {
			c, _ := l.rules.lookup(k)
			if s.units < 0 || s.units > c.capUnits || s.frac < 0 || s.frac >= 1 {
				t.Fatalf("token bucket %q: units %d frac %v outside [0, %d]", k, s.units, s.frac, c.capUnits)
			}
		}
	case *SlidingWindowCounter:
		for k, s := range l.state {
			if s.cur < 0 || s.prev < 0 {
				t.Fatalf("sliding window %q: negative counts %+v", k, s)
			}
		}
	case *SlidingWindowLog:
		for k, s := range l.state {
			c, _ := l.rules.lookup(k)
			if s.sum < 0 || s.sum > c.burst {
				t.Fatalf("sliding log %q: sum %d outside [0, %d]", k, s.sum, c.burst)
			}
		}
	}
}

// TestLim004_CostExceedsCapacity checks that a cost above the burst is an
// immediate error with RetryAfter Never on every algorithm and path, and
// that it charges nothing.
func TestLim004_CostExceedsCapacity(t *testing.T) {
	p := Params{Rate: 10, Burst: 3}
	for _, v := range allVariants() {
		t.Run(v.name, func(t *testing.T) {
			l := mustNew(t, v, all(p))
			for i, call := range []func() (Result, error){
				func() (Result, error) { return l.Acquire("k", 4, 0) },
				func() (Result, error) { return l.AcquireAll([]Request{{"j", 1}, {"k", 4}}, 0) },
				func() (Result, error) { return l.AcquireAll([]Request{{"k", 2}, {"k", 2}}, 0) }, // merged to 4
			} {
				res, err := call()
				if !errors.Is(err, ErrCostExceedsCapacity) || res.Allowed || res.RetryAfter != Never || res.Limit != 3 {
					t.Fatalf("call %d: %+v, %v; want ErrCostExceedsCapacity, RetryAfter Never, Limit 3", i, res, err)
				}
			}
			// Nothing was charged: the full burst is still there.
			if res, err := l.Acquire("j", 3, 0); err != nil || !res.Allowed {
				t.Fatalf("j after failed calls: %+v, %v", res, err)
			}
			if res, err := l.Acquire("k", 3, 0); err != nil || !res.Allowed {
				t.Fatalf("k after failed calls: %+v, %v", res, err)
			}
		})
	}
	t.Run("aimd", func(t *testing.T) {
		a, err := NewAIMD(AIMDConfig{MinRate: 1, MaxRate: 2, Window: 1, Burst: 2})
		if err != nil {
			t.Fatal(err)
		}
		if res, err := a.Acquire(3, 0); !errors.Is(err, ErrCostExceedsCapacity) || res.RetryAfter != Never {
			t.Fatalf("aimd: %+v, %v", res, err)
		}
	})
}

// TestLimInputValidation covers the remaining error paths.
func TestLimInputValidation(t *testing.T) {
	p := Params{Rate: 10, Burst: 3}
	for _, v := range allVariants() {
		l := mustNew(t, v, Rule{Pattern: "a*", Params: p})
		cases := []struct {
			name string
			call func() (Result, error)
			want error
		}{
			{"zero cost", func() (Result, error) { return l.Acquire("a", 0, 0) }, ErrInvalidCost},
			{"negative cost", func() (Result, error) { return l.Acquire("a", -1, 0) }, ErrInvalidCost},
			{"empty key", func() (Result, error) { return l.Acquire("", 1, 0) }, ErrInvalidKey},
			{"no rule", func() (Result, error) { return l.Acquire("b", 1, 0) }, ErrNoRule},
			{"time too large", func() (Result, error) { return l.Acquire("a", 1, MaxTime+1) }, ErrInvalidTime},
			{"time too small", func() (Result, error) { return l.Acquire("a", 1, -MaxTime-1) }, ErrInvalidTime},
			{"empty request", func() (Result, error) { return l.AcquireAll(nil, 0) }, ErrEmptyRequest},
			{"no rule in batch", func() (Result, error) { return l.AcquireAll([]Request{{"a", 1}, {"b", 1}}, 0) }, ErrNoRule},
		}
		for _, c := range cases {
			res, err := c.call()
			if !errors.Is(err, c.want) || res.Allowed {
				t.Errorf("%s %s: %+v, %v; want %v", v.name, c.name, res, err, c.want)
			}
		}
		// Extreme but valid times work.
		for _, now := range []time.Duration{MaxTime, -MaxTime, 0} {
			if _, err := l.Acquire("a", 1, now); err != nil {
				t.Errorf("%s: now=%v: %v", v.name, now, err)
			}
		}
	}
}

func TestLimParamsValidate(t *testing.T) {
	bad := []Params{
		{Rate: 0, Burst: 1}, {Rate: -1, Burst: 1}, {Rate: 2e9, Burst: 1},
		{Rate: 1, Burst: 0}, {Rate: 1, Burst: MaxBurst + 1},
		{Rate: 1e-9, Burst: 1 << 20}, // span overflows
		{Rate: 1, Burst: 1, Window: -1},
	}
	for _, p := range bad {
		if err := p.Validate(); !errors.Is(err, ErrInvalidParams) {
			t.Errorf("%+v: %v, want ErrInvalidParams", p, err)
		}
	}
	if err := (Params{Rate: 0.5, Burst: 10, Window: time.Minute}).Validate(); err != nil {
		t.Fatal(err)
	}
}
