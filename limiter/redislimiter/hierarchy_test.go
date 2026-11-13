package redislimiter

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/limiter"
	"github.com/genuinebnt/flowforge/testkit"
)

// hierarchyCase is the format of conformance/limiter/hierarchy (lim_008).
type hierarchyCase struct {
	ID    string `json:"id"`
	Rules []struct {
		Pattern  string  `json:"pattern"`
		RatePerS float64 `json:"rate_per_s"`
		Burst    int     `json:"burst"`
	} `json:"rules"`
	Steps []struct {
		TMs      int64 `json:"t_ms"`
		Requests []struct {
			Key  string `json:"key"`
			Cost int    `json:"cost"`
		} `json:"requests"`
		Expect struct {
			Allow     bool   `json:"allow"`
			Remaining *int   `json:"remaining"`
			Error     string `json:"error"`
		} `json:"expect"`
	} `json:"steps"`
}

// TestLim008_HierarchyConformanceRedis runs the lim_008 atomicity vector
// against the Lua script on a real Redis (FF_TEST_REDIS_ADDR), with the
// caller's clock so the vector's times are exact: one script call checks
// and charges every key, so a denial by any key charges none.
func TestLim008_HierarchyConformanceRedis(t *testing.T) {
	for _, cf := range testkit.LoadCases[hierarchyCase](t, "limiter/hierarchy") {
		t.Run(cf.Name, func(t *testing.T) {
			c := client(t, 1)
			pre := prefix(t)
			var rules []limiter.Rule
			for _, r := range cf.Data.Rules {
				rules = append(rules, limiter.Rule{Pattern: pre + r.Pattern, Params: limiter.Params{Rate: r.RatePerS, Burst: r.Burst}})
			}
			l, err := New(c, Options{CallerClock: true}, rules...)
			if err != nil {
				t.Fatal(err)
			}
			for i, s := range cf.Data.Steps {
				reqs := make([]limiter.Request, len(s.Requests))
				for j, r := range s.Requests {
					reqs[j] = limiter.Request{Key: pre + r.Key, Cost: r.Cost}
				}
				got, err := l.AcquireAll(context.Background(), reqs, time.Duration(s.TMs)*time.Millisecond)
				where := fmt.Sprintf("%s step %d", cf.Data.ID, i)
				switch {
				case s.Expect.Error == "cost_exceeds_capacity":
					if !errors.Is(err, limiter.ErrCostExceedsCapacity) || got.Allowed {
						t.Fatalf("%s: %+v %v, want cost_exceeds_capacity", where, got, err)
					}
					continue
				case err != nil:
					t.Fatalf("%s: %v", where, err)
				}
				if got.Allowed != s.Expect.Allow {
					t.Fatalf("%s: allowed %v, want %v (%+v)", where, got.Allowed, s.Expect.Allow, got)
				}
				if s.Expect.Remaining != nil && got.Remaining != *s.Expect.Remaining {
					t.Fatalf("%s: remaining %d, want %d", where, got.Remaining, *s.Expect.Remaining)
				}
				// limit is not checked: this Limiter reports the smallest
				// burst of the request, not the denying key's (see Limiter).
			}
		})
	}
}
