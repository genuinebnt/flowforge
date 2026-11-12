package limiter

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// TestLim002_TokenBucketTimeline runs the scripted timeline of
// conformance/limiter/models (model token_bucket), checks after every step
// that the bucket never holds more than its capacity, and checks the same
// timeline against the reference model.
func TestLim002_TokenBucketTimeline(t *testing.T) {
	for _, c := range loadModels(t, "token_bucket") {
		p := c.Config.params()
		tb, err := NewTokenBucket(all(p))
		if err != nil {
			t.Fatal(err)
		}
		cp, _ := compile(p)
		ref := &refBucket{b: cp.burst, T: cp.t}
		for i, s := range c.Steps {
			where := fmt.Sprintf("%s step %d", c.ID, i)
			got, err := tb.Acquire("k", s.Cost, ms(s.TMs))
			checkStep(t, where, got, err, s.Expect)
			if st := tb.state["k"]; st.units > cp.capUnits || st.units < 0 {
				t.Fatalf("%s: bucket holds %d units, capacity %d", where, st.units, cp.capUnits)
			}
			if err != nil {
				continue
			}
			if ok, retry := ref.acquire(int64(s.Cost), int64(ms(s.TMs))); ok != got.Allowed || retry != int64(got.RetryAfter) {
				t.Fatalf("%s: reference (%v, %v), bucket %+v", where, ok, time.Duration(retry), got)
			}
		}
	}
	// A fractional rate refills without losing the sub-unit remainder.
	tb, _ := NewTokenBucket(all(Params{Rate: 0.5, Burst: 1}))
	tb.Acquire("k", 1, 0)
	for now := time.Duration(0); now < 2*time.Second; now += time.Nanosecond * 999_999 {
		if res, _ := tb.Acquire("k", 1, now); res.Allowed {
			t.Fatalf("token appeared early at %v", now)
		}
	}
	if res, _ := tb.Acquire("k", 1, 2*time.Second); !res.Allowed {
		t.Fatalf("token missing at 2s: %+v", res)
	}
}

// TestLim005_SlidingWindowBoundary: requests at the end of one window and
// the start of the next. The exact log never exceeds L in any window of
// length W; the counter blocks the start of the next window (the
// conformance timelines) and, on random boundary-heavy timelines, keeps
// the spec 25.4 bound.
func TestLim005_SlidingWindowBoundary(t *testing.T) {
	for _, model := range []string{"sliding_window_counter", "sliding_window_log"} {
		for _, c := range loadModels(t, model) {
			runSteps(t, c.ID, modelLimiter(t, c), c.Steps)
		}
	}
	r := testkit.Rng(t)
	for range testkit.Cases() / 4 {
		l, w := int64(1+core.IntN(r, 6)), int64(ms(int64(100*(1+core.IntN(r, 10)))))
		p := Params{Rate: 1, Burst: int(l), Window: time.Duration(w)}
		swlog, _ := NewSlidingWindowLog(all(p))
		counter, _ := NewSlidingWindowCounter(all(p))
		var logged, counted []admit
		for k := int64(0); k < 30; k++ {
			// A cluster of requests just before and just after each boundary.
			for _, at := range []int64{k*w - int64(core.IntN(r, 3)), k * w, k*w + int64(core.IntN(r, 3))} {
				if res, _ := swlog.Acquire("k", 1, time.Duration(at)); res.Allowed {
					logged = append(logged, admit{at, 1})
				}
				if res, _ := counter.Acquire("k", 1, time.Duration(at)); res.Allowed {
					counted = append(counted, admit{at, 1})
				}
			}
		}
		for i := range logged {
			n := int64(0)
			for j := i; j < len(logged) && logged[j].t < logged[i].t+w; j++ {
				n++
			}
			if n > l {
				t.Fatalf("log: %d admitted in [%v, +W) with L = %d", n, time.Duration(logged[i].t), l)
			}
		}
		if msg := windowViolation(counted, rateBound{l, l, w}); msg != "" {
			t.Fatal("counter:", msg)
		}
	}
}

// TestLim006_AIMDTrace runs the rate trace of conformance/limiter/models
// (model aimd) and checks the floor and ceiling on random outcome streams.
func TestLim006_AIMDTrace(t *testing.T) {
	for _, c := range loadModels(t, "aimd") {
		runAIMD(t, c)
	}
	r := testkit.Rng(t)
	a, err := NewAIMD(AIMDConfig{MinRate: 2, MaxRate: 50, Window: 3, ErrorThreshold: 0.1})
	if err != nil {
		t.Fatal(err)
	}
	if a.Rate() != 2 {
		t.Fatalf("initial rate %v, want MinRate", a.Rate())
	}
	prev := a.Rate()
	for i := range 10000 {
		o := Outcome(core.IntN(r, 4))
		if core.IntN(r, 2) == 0 {
			o = OutcomeOK
		}
		rate := a.Observe(o, 0, ms(int64(i)))
		if rate < 2 || rate > 50 {
			t.Fatalf("rate %v escaped [2, 50]", rate)
		}
		if rate != prev && (i+1)%3 != 0 {
			t.Fatalf("rate changed mid-window at outcome %d", i)
		}
		if rate > prev && math.Abs(rate-min(50, prev+5)) > 1e-9 {
			t.Fatalf("increase %v -> %v is not additive 5", prev, rate)
		}
		if rate < prev && math.Abs(rate-max(2, prev*0.5)) > 1e-9 {
			t.Fatalf("decrease %v -> %v is not x0.5", prev, rate)
		}
		prev = rate
	}
	for _, bad := range []AIMDConfig{
		{MinRate: 0, MaxRate: 1, Window: 1},
		{MinRate: 2, MaxRate: 1, Window: 1},
		{MinRate: 1, MaxRate: 2, Window: 0},
		{MinRate: 1, MaxRate: 2, Window: 1, ErrorThreshold: 2},
		{MinRate: 1, MaxRate: 2, Window: 1, Decrease: 1},
		{MinRate: 1, MaxRate: 2, Window: 1, InitialRate: 3},
		{MinRate: 1, MaxRate: math.NaN(), Window: 1},
	} {
		if _, err := NewAIMD(bad); err == nil {
			t.Errorf("NewAIMD(%+v) accepted", bad)
		}
	}
}

// TestLim007_WFQWeights: two always-backlogged tenants with weights 3:1
// are served within 5% of 3:1 over 10,000 grants.
func TestLim007_WFQWeights(t *testing.T) {
	for _, costs := range [][2]int{{1, 1}, {2, 1}, {1, 3}} {
		t.Run(fmt.Sprintf("costs=%v", costs), func(t *testing.T) {
			q, err := NewWFQ[int](WFQConfig{})
			if err != nil {
				t.Fatal(err)
			}
			if err := q.SetWeight("a", 3); err != nil {
				t.Fatal(err)
			}
			if err := q.SetWeight("b", 1); err != nil {
				t.Fatal(err)
			}
			cost := map[string]int{"a": costs[0], "b": costs[1]}
			for range 4 {
				q.Enqueue("a", cost["a"], 0)
				q.Enqueue("b", cost["b"], 0)
			}
			served := map[string]int{}
			lastV := 0.0
			for i := range 10000 {
				g, ok := q.Dequeue()
				if !ok {
					t.Fatal("queue drained under saturation")
				}
				if q.v < lastV {
					t.Fatalf("grant %d: virtual time went backwards", i)
				}
				lastV = q.v
				served[g.Tenant] += g.Cost
				if err := q.Enqueue(g.Tenant, cost[g.Tenant], i); err != nil {
					t.Fatal(err)
				}
			}
			ratio := float64(served["a"]) / float64(served["b"])
			if math.Abs(ratio-3)/3 > 0.05 {
				t.Fatalf("served cost a:b = %d:%d (ratio %.3f), want 3:1 within 5%%", served["a"], served["b"], ratio)
			}
		})
	}
}

func TestLimWFQ_Errors(t *testing.T) {
	q, _ := NewWFQ[string](WFQConfig{MaxPending: 1})
	if err := q.Enqueue("x", 1, ""); err == nil {
		t.Fatal("tenant without weight accepted")
	}
	q.SetWeight("x", 1)
	if err := q.Enqueue("x", 0, ""); err == nil {
		t.Fatal("zero cost accepted")
	}
	if err := q.Enqueue("x", 1, "first"); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue("x", 1, "second"); err == nil {
		t.Fatal("MaxPending not enforced")
	}
	if g, ok := q.Dequeue(); !ok || g.Item != "first" || q.Len() != 0 {
		t.Fatalf("dequeue %+v %v", g, ok)
	}
	if _, ok := q.Dequeue(); ok {
		t.Fatal("dequeue from empty queue")
	}
	if err := q.SetWeight("x", -1); err == nil {
		t.Fatal("negative weight accepted")
	}
	if _, err := NewWFQ[int](WFQConfig{DefaultWeight: math.Inf(1)}); err == nil {
		t.Fatal("infinite default weight accepted")
	}
	// An idle tenant does not bank credit: it restarts at the current V.
	d, _ := NewWFQ[int](WFQConfig{DefaultWeight: 1})
	for range 10 {
		d.Enqueue("busy", 1, 0)
	}
	for range 5 {
		d.Dequeue()
	}
	d.Enqueue("late", 1, 0)
	// V = 4 (start of the fifth busy request); late starts there, F = 5,
	// ahead of busy's next request (F = 6).
	if g, _ := d.Dequeue(); g.Tenant != "late" || g.Start != 4 {
		t.Fatalf("got %+v, want tenant late starting at V = 4", g)
	}
}

func TestLimLeakyBucket_Delay(t *testing.T) {
	// Queue of 3 at 10/s: releases at 0, 100, 200 ms; the fourth overflows.
	lb, _ := NewLeakyBucket(all(Params{Rate: 10, Burst: 3}))
	for i, want := range []time.Duration{0, ms(100), ms(200)} {
		res, err := lb.Acquire("k", 1, 0)
		if err != nil || !res.Allowed || res.Delay != want || res.Remaining != 2-i {
			t.Fatalf("request %d: %+v %v; want delay %v", i, res, err, want)
		}
	}
	if res, _ := lb.Acquire("k", 1, 0); res.Allowed || res.RetryAfter != ms(100) {
		t.Fatalf("overflow: %+v", res)
	}
	if res, _ := lb.Acquire("k", 1, ms(150)); !res.Allowed || res.Delay != ms(150) {
		t.Fatalf("after partial drain: %+v", res)
	}
}
