package limiter

import (
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// benchParams admits nearly every request when now advances by 1 µs per
// call, so benchmarks measure the hot path rather than denials.
var benchParams = Params{Rate: 2e6, Burst: 100}

// zeroAlloc lists the algorithms whose per-check path must not allocate
// (b_lim_01). The sliding window log allocates when its ring grows.
var zeroAlloc = map[string]bool{
	"gcra": true, "gcra_atomic": true, "leaky_bucket": true, "token_bucket": true, "sliding_window_counter": true,
}

// allocsPerCheck measures steady-state allocations of Acquire on one key.
func allocsPerCheck(l Limiter) float64 {
	now := time.Duration(0)
	for range 1000 { // warm up: create the key, grow buffers
		now += time.Microsecond
		l.Acquire("key", 1, now)
	}
	return testing.AllocsPerRun(1000, func() {
		now += time.Microsecond
		l.Acquire("key", 1, now)
	})
}

// TestLimZeroAllocs enforces zero allocations per check outside benchmarks.
func TestLimZeroAllocs(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not meaningful under -race")
	}
	for _, v := range allVariants() {
		if !zeroAlloc[v.name] {
			continue
		}
		if n := allocsPerCheck(mustNew(t, v, all(benchParams))); n != 0 {
			t.Errorf("%s: %v allocs per check, want 0", v.name, n)
		}
	}
	g, _ := NewGCRA(all(benchParams))
	reqs := []Request{{"a", 1}, {"b", 1}, {"c", 1}}
	if n := testing.AllocsPerRun(1000, func() { g.AcquireAll(reqs, 0) }); n != 0 {
		t.Errorf("gcra AcquireAll: %v allocs, want 0", n)
	}
}

// BenchmarkLim01_SingleKey is b_lim_01: uncontended single-key check
// latency per algorithm.
func BenchmarkLim01_SingleKey(b *testing.B) {
	for _, v := range allVariants() {
		b.Run(v.name, func(b *testing.B) {
			l := mustNew(b, v, all(benchParams))
			if zeroAlloc[v.name] && !raceEnabled {
				if n := allocsPerCheck(mustNew(b, v, all(benchParams))); n != 0 {
					b.Fatalf("%s: %v allocs per check, want 0", v.name, n)
				}
			}
			b.ReportAllocs()
			now := time.Duration(0)
			for b.Loop() {
				now += time.Microsecond
				l.Acquire("key", 1, now)
			}
		})
	}
}

// BenchmarkLim02_Contention is b_lim_02: parallel callers on one key and
// on 1,000 keys, comparing the sharded atomic GCRA with a token bucket and
// a GCRA behind a single global mutex. Run with -cpu=1,2,4,8,16 for the
// thread-count curve.
func BenchmarkLim02_Contention(b *testing.B) {
	impls := []struct {
		name string
		make func() Limiter
	}{
		{"sharded_atomic_gcra", func() Limiter { l, _ := NewAtomicGCRA(all(benchParams)); return l }},
		{"global_mutex_token_bucket", func() Limiter { l, _ := NewTokenBucket(all(benchParams)); return l }},
		{"global_mutex_gcra", func() Limiter { l, _ := NewGCRA(all(benchParams)); return l }},
	}
	for _, nkeys := range []int{1, 1000} {
		keys := make([]string, nkeys)
		for i := range keys {
			keys[i] = "rl:h:prg_1:host" + strconv.Itoa(i) + ".example"
		}
		for _, impl := range impls {
			b.Run("keys="+strconv.Itoa(nkeys)+"/"+impl.name, func(b *testing.B) {
				l := impl.make()
				for _, k := range keys {
					l.Acquire(k, 1, 0)
				}
				var seed atomic.Int64
				b.ReportAllocs()
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					i := int(seed.Add(7919))
					now := time.Duration(0)
					for pb.Next() {
						i++
						now += time.Microsecond
						l.Acquire(keys[i%nkeys], 1, now)
					}
				})
			})
		}
	}
}
