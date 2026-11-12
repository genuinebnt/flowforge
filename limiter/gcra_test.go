package limiter

import (
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/testkit"
)

// TestLim001_GCRAVector checks the section 14.5 table: the decisions and
// retry_after come from conformance/limiter/gcra/basic.json; the TAT after
// each call is the table's last column.
func TestLim001_GCRAVector(t *testing.T) {
	var c gcraCase
	if err := json.Unmarshal(testkit.ReadFile(t, "limiter/gcra", "basic.json"), &c); err != nil {
		t.Fatal(err)
	}
	tatAfter := []int64{100, 200, 300, 400, 500, 500, 600} // spec 14.5, "tat after"
	if len(c.Steps) != len(tatAfter) {
		t.Fatalf("basic.json has %d steps, the spec table has %d", len(c.Steps), len(tatAfter))
	}
	p := c.Config.params()
	if p.EmissionInterval() != 100*time.Millisecond || p.Tolerance() != 400*time.Millisecond {
		t.Fatalf("T = %v, tau = %v; want 100ms, 400ms", p.EmissionInterval(), p.Tolerance())
	}
	g, err := NewGCRA(all(p))
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewAtomicGCRA(all(p))
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range c.Steps {
		where := fmt.Sprintf("step %d", i+1)
		got, err := g.Acquire("k", s.Cost, ms(s.TMs))
		checkStep(t, "gcra "+where, got, err, s.Expect)
		got, err = a.Acquire("k", s.Cost, ms(s.TMs))
		checkStep(t, "gcra_atomic "+where, got, err, s.Expect)
		if tat := g.state["k"].tat; tat != int64(ms(tatAfter[i])) {
			t.Fatalf("gcra %s: tat = %v, want %d ms", where, time.Duration(tat), tatAfter[i])
		}
		if tat := a.cell("k").Load(); tat != int64(ms(tatAfter[i])) {
			t.Fatalf("gcra_atomic %s: tat = %v, want %d ms", where, time.Duration(tat), tatAfter[i])
		}
	}
	// The sixth call's retry is exactly 100 ms, not merely 100 after rounding.
	g2, _ := NewGCRA(all(p))
	for range 5 {
		g2.Acquire("k", 1, 0)
	}
	if got, _ := g2.Acquire("k", 1, 0); got.RetryAfter != 100*time.Millisecond {
		t.Fatalf("retry_after = %v, want exactly 100ms", got.RetryAfter)
	}
}

// TestLim009_ConcurrentAtomicGCRA is the Go form of the spec's loom test:
// several goroutines hammer one key (and a pair of keys through
// AcquireAll) with interleaved, not necessarily ordered, clock readings.
// Under -race it also checks the memory-safety of the CAS loop. Whatever
// the interleaving, total admitted cost never exceeds
// burst + rate * (latest reading - earliest reading).
func TestLim009_ConcurrentAtomicGCRA(t *testing.T) {
	for _, threads := range []int{2, 3, 8} {
		t.Run(fmt.Sprintf("threads=%d", threads), func(t *testing.T) {
			const rate, burst = 1000, 20 // T = 1 ms
			g, err := NewAtomicGCRA(all(Params{Rate: rate, Burst: burst}))
			if err != nil {
				t.Fatal(err)
			}
			var clock atomic.Int64 // shared simulated monotonic clock, ns
			var allowedA, allowedB atomic.Int64
			var wg sync.WaitGroup
			iters := 20000
			if testing.Short() {
				iters = 2000
			}
			for w := range threads {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := range iters {
						// Advance by 0..99 µs; readers may observe readings
						// out of order relative to each other.
						now := clock.Add(int64((i*7+w*13)%100) * int64(time.Microsecond))
						if i%5 == 0 {
							res, err := g.AcquireAll([]Request{{"a", 1}, {"b", 2}}, time.Duration(now))
							if err != nil {
								t.Error(err)
								return
							}
							if res.Allowed {
								allowedA.Add(1)
								allowedB.Add(2)
							}
							continue
						}
						res, err := g.Acquire("a", 1, time.Duration(now))
						if err != nil {
							t.Error(err)
							return
						}
						if res.Allowed {
							allowedA.Add(1)
						}
					}
				}()
			}
			wg.Wait()
			elapsed := time.Duration(clock.Load())
			bound := int64(burst) + int64(elapsed/time.Millisecond)
			for key, got := range map[string]int64{"a": allowedA.Load(), "b": allowedB.Load()} {
				if got > bound {
					t.Fatalf("key %s: admitted %d > burst + rate*elapsed = %d", key, got, bound)
				}
				if got == 0 {
					t.Fatalf("key %s: nothing admitted", key)
				}
			}
			// The limiter was saturated, so it should also be close to the bound.
			if a := allowedA.Load(); a < bound/2 {
				t.Fatalf("key a admitted %d, far below the bound %d", a, bound)
			}
		})
	}
}

// TestLimGCRA_AtomicMatchesMutexMultiKey drives the same random
// multi-key sequence through both GCRA implementations.
func TestLimGCRA_AtomicMatchesMutexMultiKey(t *testing.T) {
	r := testkit.Rng(t)
	keys := []string{"a", "b", "c", "d"}
	rules := []Rule{all(Params{Rate: 10, Burst: 4}), {Pattern: "c", Params: Params{Rate: 2, Burst: 2}}}
	g, _ := NewGCRA(rules...)
	a, _ := NewAtomicGCRA(rules...)
	now := time.Duration(0)
	for i := range 5000 {
		now += time.Duration(r.Uint64()%60) * time.Millisecond
		var reqs []Request
		for _, k := range keys {
			if r.Uint64()%2 == 0 {
				reqs = append(reqs, Request{k, 1 + int(r.Uint64()%2)})
			}
		}
		if len(reqs) == 0 {
			continue
		}
		r1, e1 := g.AcquireAll(reqs, now)
		r2, e2 := a.AcquireAll(reqs, now)
		if r1 != r2 || errorCode(e1) != errorCode(e2) {
			t.Fatalf("call %d %v: gcra %+v %v, atomic %+v %v", i, reqs, r1, e1, r2, e2)
		}
	}
}

func TestLimGCRA_Prune(t *testing.T) {
	p := Params{Rate: 10, Burst: 2}
	g, _ := NewGCRA(all(p))
	a, _ := NewAtomicGCRA(all(p))
	for _, l := range []interface {
		Limiter
		Prune(time.Duration) int
		Len() int
	}{g, a} {
		l.Acquire("x", 1, 0)
		l.Acquire("y", 2, 0)
		if n := l.Prune(ms(150)); n != 1 || l.Len() != 1 {
			t.Fatalf("%T: pruned %d, len %d; want 1, 1", l, n, l.Len())
		}
		// y's TAT is 200 ms: still constraining at 150 ms.
		if res, _ := l.Acquire("y", 2, ms(150)); res.Allowed {
			t.Fatalf("%T: pruning lost y's state", l)
		}
	}
}

// TestLimAtomicGCRA_PruneRace runs Prune concurrently with Acquire and
// AcquireAll. Workers use a key per 100 ms epoch of their own monotonic
// clock, so old keys go idle and are pruned while lagging workers may
// still touch them. Each worker publishes its time before using it, so
// pruning at the minimum published time is safe. The per-key bound must
// hold, which fails if a charge lands in a deleted cell.
func TestLimAtomicGCRA_PruneRace(t *testing.T) {
	const workers, iters, rate, burst = 4, 6000, 1000, 5
	const epoch = 100 * time.Millisecond
	g, _ := NewAtomicGCRA(all(Params{Rate: rate, Burst: burst}))
	var published [workers]atomic.Int64
	var mu sync.Mutex
	admitted := map[string]int64{}
	var maxNow atomic.Int64
	var done atomic.Bool
	var wg, pwg sync.WaitGroup
	pruned := 0
	pwg.Add(1)
	go func() {
		defer pwg.Done()
		for !done.Load() {
			low := published[0].Load()
			for i := 1; i < workers; i++ {
				low = min(low, published[i].Load())
			}
			pruned += g.Prune(time.Duration(low))
			runtime.Gosched()
		}
	}()
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			now := int64(0)
			for i := range iters {
				now += int64((i*31+w*17)%400) * int64(time.Microsecond)
				published[w].Store(now)
				for m := maxNow.Load(); now > m && !maxNow.CompareAndSwap(m, now); m = maxNow.Load() {
				}
				key := fmt.Sprint("r", now/int64(epoch))
				reqs := []Request{{key, 1}}
				if i%7 == 0 {
					reqs = append(reqs, Request{"shared", 1})
				}
				res, err := g.AcquireAll(reqs, time.Duration(now))
				if err != nil {
					t.Error(err)
					return
				}
				if res.Allowed {
					mu.Lock()
					for _, r := range reqs {
						admitted[r.Key]++
					}
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	done.Store(true)
	pwg.Wait()
	perEpoch := int64(burst) + int64(epoch/time.Millisecond)
	for k, n := range admitted {
		bound := perEpoch
		if k == "shared" {
			bound = burst + maxNow.Load()/int64(time.Millisecond)
		}
		if n > bound {
			t.Fatalf("key %s admitted %d, bound %d", k, n, bound)
		}
	}
	g.Prune(time.Duration(maxNow.Load() + int64(time.Hour)))
	if g.Len() != 0 {
		t.Fatalf("%d keys left after a final prune", g.Len())
	}
	t.Logf("pruned %d cells while running; %d keys used", pruned, len(admitted))
}
