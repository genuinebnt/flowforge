package storetest

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/eventstore"
	"github.com/genuinebnt/flowforge/testkit"
)

// benchClock measures latencies (core.SystemClock is the only sanctioned
// reader of the system clock).
var benchClock = &core.SystemClock{}

func benchEnv(b *testing.B, h *Harness) *env {
	rng := testkit.Rng(b)
	return &env{t: b, h: h, s: h.Store, ctx: context.Background(), rng: rng, ids: &idGen{rng: rng}}
}

// latencies collects per-operation durations from several goroutines.
type latencies struct {
	mu sync.Mutex
	d  []time.Duration
}

func (l *latencies) add(d []time.Duration) {
	l.mu.Lock()
	l.d = append(l.d, d...)
	l.mu.Unlock()
}

func (l *latencies) report(b *testing.B) {
	if len(l.d) == 0 {
		return
	}
	slices.Sort(l.d)
	q := func(p float64) float64 { return float64(l.d[int(p*float64(len(l.d)-1))].Nanoseconds()) }
	b.ReportMetric(q(0.50), "p50-ns")
	b.ReportMetric(q(0.99), "p99-ns")
}

// createRuns creates n runs, using up to 16 goroutines.
func createRuns(e *env, n int) []eventstore.RunID {
	specs := make([]eventstore.RunSpec, n)
	for i := range specs {
		specs[i] = e.spec()
	}
	var (
		wg   sync.WaitGroup
		next = make(chan int)
		errs = make(chan error, 16)
	)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				if err := e.s.CreateRun(e.ctx, specs[i], nil); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	for i := range specs {
		select {
		case next <- i:
		case err := <-errs:
			close(next)
			wg.Wait()
			e.t.Fatalf("CreateRun: %v", err)
		}
	}
	close(next)
	wg.Wait()
	select {
	case err := <-errs:
		e.t.Fatalf("CreateRun: %v", err)
	default:
	}
	ids := make([]eventstore.RunID, n)
	for i, s := range specs {
		ids[i] = s.ID
	}
	return ids
}

// BenchAppend is b_es_01 (spec 25.8): append throughput and latency with
// `runs` concurrently written runs and `batch` events per append. One
// operation is one append; it reports events/s and p50/p99 latency.
func BenchAppend(b *testing.B, h *Harness, runs, batch int) {
	e := benchEnv(b, h)
	ids := createRuns(e, runs)
	leases := make([]eventstore.Lease, runs)
	for i, id := range ids {
		leases[i] = e.acquire(id, eventstore.WorkerID(fmt.Sprintf("bench-%d", i)), 24*time.Hour)
	}
	events := make([]eventstore.Event, batch)
	for i := range events {
		events[i] = ev("log", "engine", map[string]any{"level": "info", "message": "benchmark event", "i": i})
	}
	var (
		lat   latencies
		wg    sync.WaitGroup
		work  = make(chan struct{}) // unbuffered: the loop waits for a free writer
		total int
	)
	appendLoop := func(i int, tokens <-chan struct{}) {
		defer wg.Done()
		var (
			head uint64
			own  []time.Duration
		)
		for range tokens {
			start := benchClock.Monotonic()
			h, err := e.s.Append(e.ctx, ids[i], head, leases[i].Fence, events)
			own = append(own, benchClock.Monotonic()-start)
			if err != nil {
				b.Errorf("append: %v", err)
				break
			}
			head = h
		}
		for range tokens { // drain after an error
		}
		lat.add(own)
	}
	for i := range runs {
		wg.Add(1)
		go appendLoop(i, work)
	}
	b.ReportAllocs()
	start := benchClock.Monotonic()
	for b.Loop() {
		work <- struct{}{}
		total++
	}
	close(work)
	wg.Wait()
	elapsed := benchClock.Monotonic() - start
	b.ReportMetric(float64(total*batch)/elapsed.Seconds(), "events/s")
	b.ReportMetric(float64(elapsed.Nanoseconds())/float64(total), "ns/append")
	lat.report(b)
}

// BenchLeaseClaim is b_es_02 (spec 25.8): `workers` goroutines claim ready
// runs out of `runs` with ClaimReady and release them as ready again. One
// operation is one claim plus release; it reports claims/s, p99 latency and
// the spread of claims between workers (min/max, 1 = perfectly fair).
func BenchLeaseClaim(b *testing.B, h *Harness, runs, workers int) {
	e := benchEnv(b, h)
	createRuns(e, runs)
	var (
		lat    latencies
		wg     sync.WaitGroup
		work   = make(chan struct{}) // unbuffered: the loop waits for a free worker
		counts = make([]int, workers)
	)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			owner := eventstore.WorkerID(fmt.Sprintf("claimer-%d", w))
			var own []time.Duration
			for range work {
				start := benchClock.Monotonic()
				ls, err := e.s.ClaimReady(e.ctx, eventstore.AllShards, owner, time.Minute, 1)
				if err != nil {
					b.Errorf("claim: %v", err)
					continue
				}
				for _, l := range ls {
					if err := e.s.ReleaseLease(e.ctx, l, eventstore.Next{}); err != nil {
						b.Errorf("release: %v", err)
					}
				}
				own = append(own, benchClock.Monotonic()-start)
				counts[w] += len(ls)
			}
			lat.add(own)
		}()
	}
	b.ReportAllocs()
	start := benchClock.Monotonic()
	total := 0
	for b.Loop() {
		work <- struct{}{}
		total++
	}
	close(work)
	wg.Wait()
	elapsed := benchClock.Monotonic() - start
	claims := 0
	for _, c := range counts {
		claims += c
	}
	b.ReportMetric(float64(claims)/elapsed.Seconds(), "claims/s")
	b.ReportMetric(float64(elapsed.Nanoseconds())/float64(total), "ns/claim")
	lat.report(b)
	if total >= 10*workers {
		b.ReportMetric(float64(slices.Min(counts))/float64(max(slices.Max(counts), 1)), "fairness")
	}
}
