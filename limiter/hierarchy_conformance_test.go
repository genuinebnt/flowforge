package limiter

import (
	"fmt"
	"testing"

	"github.com/genuinebnt/flowforge/testkit"
)

// hierarchyCase is the format of
// conformance/limiter/hierarchy (lim_008).
type hierarchyCase struct {
	ID    string `json:"id"`
	Rules []struct {
		Pattern string `json:"pattern"`
		caseConfig
	} `json:"rules"`
	Steps []struct {
		TMs      int64 `json:"t_ms"`
		Requests []struct {
			Key  string `json:"key"`
			Cost int    `json:"cost"`
		} `json:"requests"`
		Expect struct {
			stepExpect
			Limit *int `json:"limit"`
		} `json:"expect"`
	} `json:"steps"`
}

// TestLim008_HierarchyConformance runs conformance/limiter/hierarchy (lim_008
// hierarchy atomicity: no partial charge when any key denies) against every
// Algorithm. limiter/redislimiter runs the same vector against Redis.
func TestLim008_HierarchyConformance(t *testing.T) {
	for _, cf := range testkit.LoadCases[hierarchyCase](t, "limiter/hierarchy") {
		for _, v := range allVariants() {
			t.Run(cf.Name+"/"+v.name, func(t *testing.T) {
				var rules []Rule
				for _, r := range cf.Data.Rules {
					rules = append(rules, Rule{Pattern: r.Pattern, Params: r.params()})
				}
				l := mustNew(t, v, rules...)
				for i, s := range cf.Data.Steps {
					reqs := make([]Request, len(s.Requests))
					for j, r := range s.Requests {
						reqs[j] = Request{Key: r.Key, Cost: r.Cost}
					}
					got, err := l.AcquireAll(reqs, ms(s.TMs))
					where := fmt.Sprintf("%s step %d", cf.Data.ID, i)
					checkStep(t, where, got, err, s.Expect.stepExpect)
					if s.Expect.Limit != nil && got.Limit != *s.Expect.Limit {
						t.Fatalf("%s: limit %d, want %d (%+v)", where, got.Limit, *s.Expect.Limit, got)
					}
				}
			})
		}
	}
}
