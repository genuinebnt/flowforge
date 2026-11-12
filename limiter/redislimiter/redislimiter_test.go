package redislimiter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/limiter"
	"github.com/genuinebnt/flowforge/testkit"
	"github.com/redis/go-redis/v9"
)

// client returns a Redis client for FF_TEST_REDIS_ADDR or skips the test.
func client(t testing.TB, poolSize int) *redis.Client {
	t.Helper()
	addr := os.Getenv("FF_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("FF_TEST_REDIS_ADDR not set")
	}
	c := redis.NewClient(&redis.Options{Addr: addr, PoolSize: poolSize})
	if err := c.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("redis at %s: %v", addr, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// prefix isolates the keys of one test run.
func prefix(t testing.TB) string {
	return fmt.Sprintf("fftest:%s:%d:", t.Name(), testkit.Seed(t))
}

// TestLim010_RedisMatchesMemory runs the spec 14.4 script against a real
// Redis and compares it with the in-memory GCRA.
//
// Comparing timelines needs a shared clock, but the production script
// reads Redis TIME. Two checks cover it: (1) the script with only its
// clock line replaced to read now from ARGV (Options.CallerClock) must
// match the in-memory GCRA exactly, decision and retry, on random
// single- and multi-key timelines with millisecond times; (2) the
// verbatim TIME script must reproduce the 14.5 burst behaviour, which
// does not depend on elapsed time beyond a small tolerance.
func TestLim010_RedisMatchesMemory(t *testing.T) {
	c := client(t, 10)
	ctx := context.Background()

	t.Run("caller clock timeline", func(t *testing.T) {
		r := testkit.Rng(t)
		pre := prefix(t)
		rules := []limiter.Rule{
			{Pattern: pre + "*", Params: limiter.Params{Rate: 10, Burst: 5}},
			{Pattern: pre + "slow", Params: limiter.Params{Rate: 2, Burst: 2}},
			{Pattern: pre + "fast*", Params: limiter.Params{Rate: 100, Burst: 8}},
		}
		rl, err := New(c, Options{CallerClock: true}, rules...)
		if err != nil {
			t.Fatal(err)
		}
		mem, err := limiter.NewGCRA(rules...)
		if err != nil {
			t.Fatal(err)
		}
		keys := []string{pre + "a", pre + "slow", pre + "fast1"}
		now := time.Duration(1000) * time.Millisecond // Redis's GET-or-0 equals "no history" for now > 0
		for i := range 600 {
			now += time.Duration(core.IntN(r, 120)) * time.Millisecond
			var reqs []limiter.Request
			for _, k := range keys {
				if core.IntN(r, 2) == 0 {
					reqs = append(reqs, limiter.Request{Key: k, Cost: 1 + core.IntN(r, 2)})
				}
			}
			if len(reqs) == 0 {
				reqs = append(reqs, limiter.Request{Key: keys[0], Cost: 1})
			}
			got, err := rl.AcquireAll(ctx, reqs, now)
			if err != nil {
				t.Fatal(err)
			}
			want, err := mem.AcquireAll(reqs, now)
			if err != nil {
				t.Fatal(err)
			}
			if got.Allowed != want.Allowed || got.RetryAfter != want.RetryAfter {
				t.Fatalf("step %d at %v %v: redis %+v, memory %+v", i, now, reqs, got, want)
			}
		}
		// The spec 14.5 vector through the conformance file.
		var vec struct {
			Steps []struct {
				TMs    int64 `json:"t_ms"`
				Cost   int   `json:"cost"`
				Expect struct {
					Allow        bool   `json:"allow"`
					RetryAfterMs *int64 `json:"retry_after_ms"`
				} `json:"expect"`
			} `json:"steps"`
		}
		if err := json.Unmarshal(testkit.ReadFile(t, "limiter/gcra", "basic.json"), &vec); err != nil {
			t.Fatal(err)
		}
		key := pre + "vector"
		base := 10 * time.Second // a positive origin, see above
		for i, s := range vec.Steps {
			got, err := rl.Acquire(ctx, key, s.Cost, base+time.Duration(s.TMs)*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			if got.Allowed != s.Expect.Allow || (s.Expect.RetryAfterMs != nil && got.RetryAfter != time.Duration(*s.Expect.RetryAfterMs)*time.Millisecond) {
				t.Fatalf("vector step %d: %+v", i+1, got)
			}
		}
	})

	t.Run("redis time burst", func(t *testing.T) {
		pre := prefix(t)
		rl, err := New(c, Options{}, limiter.Rule{Pattern: "*", Params: limiter.Params{Rate: 10, Burst: 5}})
		if err != nil {
			t.Fatal(err)
		}
		// Five pass at once; the sixth waits about one emission interval.
		// The calls take a few milliseconds of Redis time, hence the slack.
		for i := range 5 {
			if res, err := rl.Acquire(ctx, pre+"k", 1, 0); err != nil || !res.Allowed {
				t.Fatalf("burst call %d: %+v %v", i, res, err)
			}
		}
		res, err := rl.Acquire(ctx, pre+"k", 1, 0)
		if err != nil || res.Allowed || res.RetryAfter <= 50*time.Millisecond || res.RetryAfter > 100*time.Millisecond {
			t.Fatalf("sixth call: %+v %v, want denied with retry in (50ms, 100ms]", res, err)
		}
		// Multi-key: a denying key charges nothing on the other.
		if res, _ := rl.AcquireAll(ctx, []limiter.Request{{Key: pre + "other", Cost: 1}, {Key: pre + "k", Cost: 1}}, 0); res.Allowed {
			t.Fatalf("multi-key call passed a full key: %+v", res)
		}
		if res, _ := rl.Acquire(ctx, pre+"other", 5, 0); !res.Allowed {
			t.Fatalf("other key was charged by a denied call: %+v", res)
		}
		// TATs expire: the script sets a PX TTL on every key it writes.
		if ttl := c.PTTL(ctx, pre+"k").Val(); ttl <= 0 || ttl > 2*time.Second {
			t.Fatalf("PTTL = %v", ttl)
		}
	})

	t.Run("errors", func(t *testing.T) {
		rl, _ := New(c, Options{}, limiter.Rule{Pattern: "*", Params: limiter.Params{Rate: 10, Burst: 5}})
		if res, err := rl.Acquire(ctx, "x", 6, 0); !errors.Is(err, limiter.ErrCostExceedsCapacity) || res.RetryAfter != limiter.Never {
			t.Fatalf("cost > burst: %+v %v", res, err)
		}
		if _, err := rl.AcquireAll(ctx, nil, 0); !errors.Is(err, limiter.ErrEmptyRequest) {
			t.Fatalf("empty: %v", err)
		}
	})
}

// TestRedisUnavailable needs no server: an unreachable Redis is reported
// as limiter.ErrUnavailable, which Failover turns into fail-closed for
// active work.
func TestRedisUnavailable(t *testing.T) {
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	defer c.Close()
	rl, err := New(c, Options{}, limiter.Rule{Pattern: "*", Params: limiter.Params{Rate: 10, Burst: 5}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := rl.Acquire(ctx, "k", 1, 0); !errors.Is(err, limiter.ErrUnavailable) {
		t.Fatalf("got %v, want ErrUnavailable", err)
	}
	local, _ := limiter.NewGCRA(limiter.Rule{Pattern: "*", Params: limiter.Params{Rate: 1, Burst: 1}})
	f := limiter.Failover{Primary: rl, Local: local}
	if res, err := f.Acquire(ctx, "k", 1, limiter.WorkActive, 0); res.Allowed || !errors.Is(err, limiter.ErrUnavailable) {
		t.Fatalf("active work during outage: %+v %v", res, err)
	}
	if res, err := f.Acquire(ctx, "k", 1, limiter.WorkPassive, 0); !res.Allowed || err != nil {
		t.Fatalf("passive work during outage: %+v %v", res, err)
	}
	if _, err := rl.Acquire(ctx, "", 1, 0); !errors.Is(err, limiter.ErrInvalidKey) {
		t.Fatalf("empty key: %v", err)
	}
}

func TestMsParams(t *testing.T) {
	for _, c := range []struct {
		p      limiter.Params
		t, tau int64
	}{
		{limiter.Params{Rate: 10, Burst: 5}, 100, 400},
		{limiter.Params{Rate: 3, Burst: 2}, 334, 334}, // rounded up: never faster than configured
		{limiter.Params{Rate: 5000, Burst: 1}, 1, 0},
	} {
		if gt, gtau := msParams(c.p); gt != c.t || gtau != c.tau {
			t.Errorf("%+v: T=%d tau=%d, want %d %d", c.p, gt, gtau, c.t, c.tau)
		}
	}
}

// BenchmarkLim03_Redis is b_lim_03: the Lua limiter round trip with 1 and
// 64 client connections, reporting p50 and p99 latency.
func BenchmarkLim03_Redis(b *testing.B) {
	for _, conns := range []int{1, 64} {
		b.Run("conns="+strconv.Itoa(conns), func(b *testing.B) {
			c := client(b, conns)
			rl, err := New(c, Options{}, limiter.Rule{Pattern: "*", Params: limiter.Params{Rate: 1e6, Burst: 1000}})
			if err != nil {
				b.Fatal(err)
			}
			pre := prefix(b)
			var mu sync.Mutex
			var lat []time.Duration
			run := func(i int, local []time.Duration) []time.Duration {
				start := time.Now()
				if _, err := rl.Acquire(context.Background(), pre+strconv.Itoa(i%100), 1, 0); err != nil {
					b.Error(err)
				}
				return append(local, time.Since(start))
			}
			if conns == 1 {
				// One connection, one caller: a plain round-trip loop.
				i := 0
				for b.Loop() {
					i++
					lat = run(i, lat)
				}
			} else {
				b.SetParallelism(max(1, conns/runtime.GOMAXPROCS(0)))
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					local := make([]time.Duration, 0, 1024)
					for i := 0; pb.Next(); i++ {
						local = run(i, local)
					}
					mu.Lock()
					lat = append(lat, local...)
					mu.Unlock()
				})
			}
			b.StopTimer()
			if len(lat) > 0 {
				slices.Sort(lat)
				b.ReportMetric(float64(lat[len(lat)/2].Nanoseconds()), "p50-ns")
				b.ReportMetric(float64(lat[len(lat)*99/100].Nanoseconds()), "p99-ns")
			}
		})
	}
}
