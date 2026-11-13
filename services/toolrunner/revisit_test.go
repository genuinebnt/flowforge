package toolrunner_test

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/services/toolrunner"
	"github.com/genuinebnt/flowforge/testkit"
)

// TestRevisit_AdaptiveSchedule pins V2_DESIGN §11 item 8: the first visit
// waits the initial 24 h, a change halves the interval, no change doubles
// it, and the interval stays within [1 h, 7 d].
func TestRevisit_AdaptiveSchedule(t *testing.T) {
	p := toolrunner.AdaptiveRevisit{}
	steps := []struct {
		changed bool
		want    time.Duration
	}{
		{false, 24 * time.Hour}, // first visit: initial, whatever it found
		{true, 12 * time.Hour}, {true, 6 * time.Hour}, {true, 3 * time.Hour}, {true, 90 * time.Minute},
		{true, time.Hour}, {true, time.Hour}, // clamped at 1 h
		{false, 2 * time.Hour}, {false, 4 * time.Hour}, {false, 8 * time.Hour}, {false, 16 * time.Hour},
		{false, 32 * time.Hour}, {false, 64 * time.Hour}, {false, 128 * time.Hour},
		{false, 168 * time.Hour}, {false, 168 * time.Hour}, // clamped at 7 d
		{true, 84 * time.Hour},
	}
	var st toolrunner.RevisitState
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	changes := 0
	for i, s := range steps {
		st = st.Visit(p, at, s.changed)
		if s.changed {
			changes++
		}
		if st.Interval != s.want || !st.Due.Equal(at.Add(s.want)) || st.Visits != i+1 || st.Changes != changes {
			t.Fatalf("visit %d (changed %v): %+v, want interval %v", i, s.changed, st, s.want)
		}
		at = st.Due
	}
	if f := (toolrunner.FixedRevisit{}).Next(time.Hour, true); f != 24*time.Hour {
		t.Fatalf("fixed: %v", f)
	}
	for name, want := range map[string]string{"": "adaptive", "adaptive": "adaptive", "fixed": "fixed"} {
		if p, err := toolrunner.ParseRevisitPolicy(name); err != nil || p.Name() != want {
			t.Fatalf("ParseRevisitPolicy(%q) = %v, %v", name, p, err)
		}
	}
	if _, err := toolrunner.ParseRevisitPolicy("random"); err == nil {
		t.Fatal("unknown policy accepted")
	}
}

// TestRevisitProp_Bounded: over random visit histories and custom bounds
// the adaptive interval stays within [Min, Max] and moves by exactly a
// factor of two unless clamped.
func TestRevisitProp_Bounded(t *testing.T) {
	rng := testkit.Rng(t)
	for range testkit.Cases() {
		lo := time.Duration(1+rng.Uint64()%120) * time.Minute
		hi := lo * time.Duration(2+rng.Uint64()%200)
		p := toolrunner.AdaptiveRevisit{Min: lo, Max: hi, Initial: lo + time.Duration(rng.Uint64()%uint64(hi-lo+1))}
		prev := time.Duration(0)
		for range 40 {
			changed := rng.Uint64()%2 == 0
			next := p.Next(prev, changed)
			if next < lo || next > hi {
				t.Fatalf("%+v: interval %v outside [%v, %v]", p, next, lo, hi)
			}
			if prev > 0 {
				raw := prev * 2
				if changed {
					raw = prev / 2
				}
				if want := min(max(raw, lo), hi); next != want {
					t.Fatalf("%+v: after %v (changed %v) got %v, want %v", p, prev, changed, next, want)
				}
			}
			prev = next
		}
	}
}

// revisitSim simulates a URL whose content changes at the given mean
// interval (exponentially distributed gaps, seeded) for 90 days, visited
// as the policy schedules. It returns fetches per day and the mean delay
// in hours from a change to the visit that sees it.
func revisitSim(p toolrunner.RevisitPolicy, mean time.Duration, seed uint64) (fetchesPerDay, delayHours float64) {
	const span = 90 * 24 * time.Hour
	rng := core.NewSplitMix64(seed)
	var changes []time.Duration
	for at := time.Duration(0); ; {
		u := (float64(rng.Uint64()>>11) + 1) / (1 << 53)
		at += time.Duration(-math.Log(u) * float64(mean))
		if at >= span {
			break
		}
		changes = append(changes, at)
	}
	var st toolrunner.RevisitState
	now, ci, fetches := time.Duration(0), 0, 0
	var delay time.Duration
	seen := 0
	base := time.Unix(0, 0)
	for now < span {
		changed := false
		for ci < len(changes) && changes[ci] <= now {
			delay += now - changes[ci]
			ci++
			seen++
			changed = true
		}
		fetches++
		st = st.Visit(p, base.Add(now), changed)
		now = st.Due.Sub(base)
	}
	if seen > 0 {
		delayHours = delay.Hours() / float64(seen)
	}
	return float64(fetches) / 90, delayHours
}

// BenchmarkToolAlgo compares the crawl revisit policies (docs/ALGORITHMS.md,
// "Crawl revisit policy") on simulated pages that change every 6 hours,
// every 3 days and every 30 days on average: fetches per day and mean
// hours from a change to its detection, averaged over 50 seeded histories.
func BenchmarkToolAlgo(b *testing.B) {
	policies := []toolrunner.RevisitPolicy{toolrunner.AdaptiveRevisit{}, toolrunner.FixedRevisit{}}
	for _, p := range policies {
		for _, mean := range []time.Duration{6 * time.Hour, 72 * time.Hour, 720 * time.Hour} {
			b.Run(fmt.Sprintf("revisit/%s/change_every_%dh", p.Name(), int(mean.Hours())), func(b *testing.B) {
				var f, d float64
				for b.Loop() {
					f, d = 0, 0
					for seed := uint64(1); seed <= 50; seed++ {
						fs, ds := revisitSim(p, mean, seed)
						f, d = f+fs/50, d+ds/50
					}
				}
				b.ReportMetric(f, "fetches/day")
				b.ReportMetric(d, "delay_h")
			})
		}
	}
}
