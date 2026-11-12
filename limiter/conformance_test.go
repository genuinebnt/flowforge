package limiter

import (
	"fmt"
	"math"
	"testing"

	"github.com/genuinebnt/flowforge/testkit"
)

type gcraCase struct {
	ID     string     `json:"id"`
	Config caseConfig `json:"config"`
	Steps  []step     `json:"steps"`
}

// runSteps feeds a timeline to l on key "k" and checks every step.
func runSteps(t *testing.T, id string, l Limiter, steps []step) {
	t.Helper()
	for i, s := range steps {
		got, err := l.Acquire("k", s.Cost, ms(s.TMs))
		checkStep(t, fmt.Sprintf("%s step %d (t=%dms cost=%d)", id, i, s.TMs, s.Cost), got, err, s.Expect)
	}
}

// TestLimConformance_GCRA runs conformance/limiter/gcra against every GCRA
// implementation (the leaky bucket admits exactly like GCRA).
func TestLimConformance_GCRA(t *testing.T) {
	for _, cf := range testkit.LoadCases[gcraCase](t, "limiter/gcra") {
		for _, v := range gcraVariants() {
			t.Run(cf.Name+"/"+v.name, func(t *testing.T) {
				runSteps(t, cf.Data.ID, mustNew(t, v, all(cf.Data.Config.params())), cf.Data.Steps)
			})
		}
	}
}

type aimdCase struct {
	MinRate        float64 `json:"min_rate"`
	MaxRate        float64 `json:"max_rate"`
	InitialRate    float64 `json:"initial_rate"`
	Window         int     `json:"window"`
	ErrorThreshold float64 `json:"error_threshold"`
}

type aimdEvent struct {
	TMs          int64    `json:"t_ms"`
	Outcome      string   `json:"outcome"`
	RetryAfterMs int64    `json:"retry_after_ms"`
	ExpectRate   *float64 `json:"expect_rate"`
	Acquire      *step    `json:"acquire"`
}

type modelCase struct {
	ID     string      `json:"id"`
	Model  string      `json:"model"`
	Config caseConfig  `json:"config"`
	Steps  []step      `json:"steps"`
	AIMD   aimdCase    `json:"aimd"`
	Events []aimdEvent `json:"events"`
}

func loadModels(t *testing.T, model string) []modelCase {
	t.Helper()
	var out []modelCase
	for _, cf := range testkit.LoadCases[modelCase](t, "limiter/models") {
		if model == "" || cf.Data.Model == model {
			out = append(out, cf.Data)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no limiter/models case for model %q", model)
	}
	return out
}

var outcomes = map[string]Outcome{"ok": OutcomeOK, "429": OutcomeThrottled, "timeout": OutcomeTimeout, "5xx": OutcomeServerError}

func runAIMD(t *testing.T, c modelCase) {
	t.Helper()
	a, err := NewAIMD(AIMDConfig{
		MinRate: c.AIMD.MinRate, MaxRate: c.AIMD.MaxRate, InitialRate: c.AIMD.InitialRate,
		Window: c.AIMD.Window, ErrorThreshold: c.AIMD.ErrorThreshold,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range c.Events {
		where := fmt.Sprintf("%s event %d (t=%dms)", c.ID, i, e.TMs)
		if e.Acquire != nil {
			got, err := a.Acquire(e.Acquire.Cost, ms(e.TMs))
			checkStep(t, where, got, err, e.Acquire.Expect)
			continue
		}
		o, ok := outcomes[e.Outcome]
		if !ok {
			t.Fatalf("%s: unknown outcome %q", where, e.Outcome)
		}
		rate := a.Observe(o, ms(e.RetryAfterMs), ms(e.TMs))
		if e.ExpectRate != nil && math.Abs(rate-*e.ExpectRate) > 1e-9 {
			t.Fatalf("%s: rate = %v, want %v", where, rate, *e.ExpectRate)
		}
	}
}

func modelLimiter(t *testing.T, c modelCase) Limiter {
	t.Helper()
	r := all(c.Config.params())
	var (
		l   Limiter
		err error
	)
	switch c.Model {
	case "token_bucket":
		l, err = NewTokenBucket(r)
	case "sliding_window_counter":
		l, err = NewSlidingWindowCounter(r)
	case "sliding_window_log":
		l, err = NewSlidingWindowLog(r)
	case "gcra":
		l, err = NewGCRA(r)
	default:
		t.Fatalf("%s: unknown model %q", c.ID, c.Model)
	}
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// TestLimConformance_Models runs every timeline in conformance/limiter/models.
func TestLimConformance_Models(t *testing.T) {
	for _, c := range loadModels(t, "") {
		t.Run(c.ID, func(t *testing.T) {
			if c.Model == "aimd" {
				runAIMD(t, c)
				return
			}
			runSteps(t, c.ID, modelLimiter(t, c), c.Steps)
		})
	}
}
