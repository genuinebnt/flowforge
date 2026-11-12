package limiter

import (
	"fmt"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// admit is one admitted request of a timeline.
type admit struct{ t, cost int64 }

// rateBound expresses "burst + rate * w" as burst + w * num / den, with w
// in nanoseconds, so it can be checked in exact integer arithmetic.
type rateBound struct{ burst, num, den int64 }

// boundFor returns the spec 25.4 bound of variant name under p.
func boundFor(name string, p Params) rateBound {
	c, err := compile(p)
	if err != nil {
		panic(err)
	}
	switch name {
	case "sliding_window_counter", "sliding_window_log", "crdt":
		return rateBound{c.burst, c.burst, c.window} // rate = L / W
	}
	return rateBound{c.burst, int64(p.Rate), 1e9} // integer rates only
}

// windowViolation returns a description of the first window [t_i, t_j]
// whose admitted cost exceeds the bound, or "" if there is none. It
// suffices to check windows whose ends are admitted requests.
func windowViolation(events []admit, b rateBound) string {
	for i := range events {
		sum := int64(0)
		for j := i; j < len(events); j++ {
			sum += events[j].cost
			w := events[j].t - events[i].t
			if (sum-b.burst)*b.den > w*b.num {
				return fmt.Sprintf("cost %d admitted in [%v, %v] exceeds %d + %d/%d*w", sum,
					time.Duration(events[i].t), time.Duration(events[j].t), b.burst, b.num, b.den)
			}
		}
	}
	return ""
}

var integerRates = []float64{1, 2, 4, 5, 8, 10, 20, 25, 50, 100, 1000}

// genParams draws parameters with an integer rate that divides 1e9, so
// T is exact and the reference models apply without rounding.
func genParams(r core.Rng) Params {
	return Params{Rate: integerRates[core.IntN(r, len(integerRates))], Burst: 1 + core.IntN(r, 10)}
}

// genGap draws the time to the next request, mixing bursts, gaps around
// the emission interval and long idle periods.
func genGap(r core.Rng, p Params) int64 {
	t := int64(p.EmissionInterval())
	switch core.IntN(r, 6) {
	case 0, 1:
		return 0
	case 2:
		return int64(core.IntN(r, int(t)+1))
	case 3:
		return t
	case 4:
		return int64(core.IntN(r, 3*int(t)+1))
	default:
		return int64(core.IntN(r, 20*int(t)+1))
	}
}

func genCost(r core.Rng, p Params) int {
	if core.IntN(r, 3) == 0 {
		return 1 + core.IntN(r, p.Burst)
	}
	return 1
}

// TestLimProp_WindowBound: for any timeline, total admitted cost in any
// window [t, t + w] is at most burst + rate * w (spec 25.4).
func TestLimProp_WindowBound(t *testing.T) {
	r := testkit.Rng(t)
	for _, v := range allVariants() {
		t.Run(v.name, func(t *testing.T) {
			for c := range testkit.Cases() {
				p := genParams(r)
				l := mustNew(t, v, all(p))
				var events []admit
				now := int64(core.IntN(r, 1000))
				for range 150 {
					now += genGap(r, p)
					cost := genCost(r, p)
					res, err := l.Acquire("k", cost, time.Duration(now))
					if err != nil {
						t.Fatal(err)
					}
					if res.Allowed {
						events = append(events, admit{now, int64(cost)})
					}
				}
				if msg := windowViolation(events, boundFor(v.name, p)); msg != "" {
					t.Fatalf("case %d %+v: %s", c, p, msg)
				}
			}
		})
	}
}

// refBucket is the reference model shared by GCRA, the token bucket and
// the leaky bucket's admission: a list of admitted (time, cost). A request
// of cost c at now passes when c <= b and, for every admitted t_j,
// (A_j + c - b) * T <= now - t_j, where A_j is the cost admitted at or
// after t_j. When it fails, the retry is the largest shortfall.
type refBucket struct {
	b, T int64
	log  []admit
}

func (m *refBucket) acquire(cost, now int64) (bool, int64) {
	retry, suffix := int64(0), int64(0)
	for j := len(m.log) - 1; j >= 0; j-- {
		suffix += m.log[j].cost
		if short := (suffix+cost-m.b)*m.T - (now - m.log[j].t); short > retry {
			retry = short
		}
	}
	if retry > 0 {
		return false, retry
	}
	m.log = append(m.log, admit{now, cost})
	return true, 0
}

// refLog is the reference of the sliding window log: admitted cost with
// t > now - W, counted by scanning the whole history.
type refLog struct {
	l, w int64
	log  []admit
}

func (m *refLog) acquire(cost, now int64) bool {
	sum := int64(0)
	for _, e := range m.log {
		if e.t > now-m.w {
			sum += e.cost
		}
	}
	if sum+cost > m.l {
		return false
	}
	m.log = append(m.log, admit{now, cost})
	return true
}

// refCounter is the reference of the sliding window counter: the counts
// of the previous and current fixed windows are recomputed from the list
// of admitted timestamps and the estimate is evaluated in rationals.
type refCounter struct {
	l, w int64
	log  []admit
}

func (m *refCounter) acquire(cost, now int64) bool {
	idx := floorDiv(now, m.w)
	var prev, cur int64
	for _, e := range m.log {
		switch floorDiv(e.t, m.w) {
		case idx:
			cur += e.cost
		case idx - 1:
			prev += e.cost
		}
	}
	e := now - idx*m.w
	// prev * (1 - e/W) + cur + cost <= L  <=>  prev*(W-e) + (cur+cost)*W <= L*W
	if prev*(m.w-e)+(cur+cost)*m.w > m.l*m.w {
		return false
	}
	m.log = append(m.log, admit{now, cost})
	return true
}

// TestLimProp_MatchesReference: every variant agrees with a simple
// timestamp-list reference model on generated timelines, and the atomic
// GCRA agrees with the mutex GCRA exactly, retry hints included.
func TestLimProp_MatchesReference(t *testing.T) {
	r := testkit.Rng(t)
	for c := range testkit.Cases() {
		p := genParams(r)
		cp, _ := compile(p)
		gcra, _ := NewGCRA(all(p))
		atomicGCRA, _ := NewAtomicGCRA(all(p))
		leaky, _ := NewLeakyBucket(all(p))
		bucket, _ := NewTokenBucket(all(p))
		counter, _ := NewSlidingWindowCounter(all(p))
		swlog, _ := NewSlidingWindowLog(all(p))
		crdt, _ := NewCRDTLimiter("n1", all(p))
		refG := &refBucket{b: cp.burst, T: cp.t}
		refT := &refBucket{b: cp.burst, T: cp.t}
		refC := &refCounter{l: cp.burst, w: cp.window}
		refD := &refCounter{l: cp.burst, w: cp.window}
		refL := &refLog{l: cp.burst, w: cp.window}
		now := int64(core.IntN(r, 1000))
		for i := range 150 {
			now += genGap(r, p)
			cost := genCost(r, p)
			at := time.Duration(now)
			where := fmt.Sprintf("case %d %+v step %d (now=%v cost=%d)", c, p, i, at, cost)

			g, _ := gcra.Acquire("k", cost, at)
			a, _ := atomicGCRA.Acquire("k", cost, at)
			if g != a {
				t.Fatalf("%s: gcra %+v != atomic %+v", where, g, a)
			}
			ok, retry := refG.acquire(int64(cost), now)
			if g.Allowed != ok || int64(g.RetryAfter) != retry {
				t.Fatalf("%s: gcra %+v, reference (%v, %v)", where, g, ok, time.Duration(retry))
			}
			lb, _ := leaky.Acquire("k", cost, at)
			if lb.Allowed != g.Allowed || lb.RetryAfter != g.RetryAfter || lb.Remaining != g.Remaining {
				t.Fatalf("%s: leaky %+v vs gcra %+v", where, lb, g)
			}
			tb, _ := bucket.Acquire("k", cost, at)
			ok, retry = refT.acquire(int64(cost), now)
			if tb.Allowed != ok || int64(tb.RetryAfter) != retry {
				t.Fatalf("%s: token bucket %+v, reference (%v, %v)", where, tb, ok, time.Duration(retry))
			}
			if tb.Allowed && tb.Remaining != g.Remaining {
				t.Fatalf("%s: token bucket remaining %d, gcra %d", where, tb.Remaining, g.Remaining)
			}
			if sc, _ := counter.Acquire("k", cost, at); sc.Allowed != refC.acquire(int64(cost), now) {
				t.Fatalf("%s: sliding counter %+v disagrees with reference", where, sc)
			}
			if d, _ := crdt.Acquire("k", cost, at); d.Allowed != refD.acquire(int64(cost), now) {
				t.Fatalf("%s: crdt %+v disagrees with reference", where, d)
			}
			if sl, _ := swlog.Acquire("k", cost, at); sl.Allowed != refL.acquire(int64(cost), now) {
				t.Fatalf("%s: sliding log %+v disagrees with reference", where, sl)
			}
		}
	}
}

// TestLimProp_RetryIsHonest: after a denial, the same request passes at
// now + RetryAfter when there is no other traffic, for every algorithm.
// (TestLimProp_MatchesReference shows GCRA's and the token bucket's
// RetryAfter is also the earliest such time.)
func TestLimProp_RetryIsHonest(t *testing.T) {
	r := testkit.Rng(t)
	for _, v := range allVariants() {
		t.Run(v.name, func(t *testing.T) {
			for range testkit.Cases() / 4 {
				p := genParams(r)
				now := int64(0)
				l := mustNew(t, v, all(p))
				for range 40 {
					now += genGap(r, p)
					cost := genCost(r, p)
					res, _ := l.Acquire("k", cost, time.Duration(now))
					if res.Allowed {
						continue
					}
					at := time.Duration(now) + res.RetryAfter
					if again, _ := l.Acquire("k", cost, at); !again.Allowed {
						t.Fatalf("%+v: denied at now+RetryAfter (%v): %+v", p, res.RetryAfter, again)
					}
					now = int64(at)
				}
			}
		})
	}
}
