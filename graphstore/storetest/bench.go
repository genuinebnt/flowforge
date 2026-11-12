package storetest

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/oam"
)

// benchClock measures latencies; core.SystemClock is the sanctioned
// reader of the system clock.
var benchClock = &core.SystemClock{}

// SimulatedToolOutput returns observation i of the b_sys_01 workload: a
// dnsx-style resolution of one of hosts names (FQDN -> IPAddress) followed
// by an httpx-style probe (IP -> Service) every third observation, with
// tool, run and time varying, so the mix is mostly updates of a growing
// graph.
func SimulatedToolOutput(i, hosts int, tenant, program string) graphstore.IngestRequest {
	h := i % hosts
	name := fmt.Sprintf("h%d.acme.com", h)
	ip := fmt.Sprintf("10.%d.%d.%d", (h>>16)&255, (h>>8)&255, h&255)
	o := obs(fmt.Sprintf("bench-%d", i), []string{"dnsx", "subfinder", "httpx"}[i%3], T0.Add(time.Duration(i)*time.Millisecond), 0.9,
		[]oam.ObservedEntity{ent("a", oam.FQDN{Name: name}), ent("b", oam.IPAddress{Address: ip})}, dns("a", "b", 1))
	if i%3 == 2 {
		o.Entities = append(o.Entities, ent("s", oam.Service{UniqueID: fmt.Sprintf("svc-%d", h), ServiceType: "https", Output: fmt.Sprintf("HTTP/1.1 200 build %d", i)}))
		o.Edges = append(o.Edges, port("b", "s", 443))
	}
	return graphstore.IngestRequest{TenantID: tenant, ProgramID: program, RunID: RunID(fmt.Sprint("bench-run-", i/1000)), Observation: o}
}

// BenchIngest is b_sys_01 (spec 25.8): simulated tool observations
// ingested by workers goroutines. It reports observations/s and the p50
// and p99 latency per observation.
func BenchIngest(b *testing.B, s graphstore.Store, workers int) {
	ctx := context.Background()
	tenant, prog := TenantID("bench"), ProgramID(fmt.Sprint("bench-", b.N))
	const hosts = 10_000
	var (
		mu   sync.Mutex
		lats []time.Duration
		next = make(chan int)
		wg   sync.WaitGroup
		errs = make(chan error, workers)
	)
	b.ReportAllocs()
	b.ResetTimer()
	start := benchClock.Monotonic()
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var local []time.Duration
			for i := range next {
				t0 := benchClock.Monotonic()
				if _, err := s.IngestObservation(ctx, SimulatedToolOutput(i, hosts, tenant, prog)); err != nil {
					errs <- err
					break
				}
				local = append(local, benchClock.Monotonic()-t0)
			}
			mu.Lock()
			lats = append(lats, local...)
			mu.Unlock()
		}()
	}
	i := 0
	for b.Loop() {
		select {
		case next <- i:
		case err := <-errs:
			close(next)
			wg.Wait()
			b.Fatal(err)
		}
		i++
	}
	close(next)
	wg.Wait()
	elapsed := benchClock.Monotonic() - start
	select {
	case err := <-errs:
		b.Fatal(err)
	default:
	}
	if len(lats) == 0 {
		return
	}
	slices.Sort(lats)
	q := func(p float64) float64 { return float64(lats[int(p*float64(len(lats)-1))].Microseconds()) }
	b.ReportMetric(float64(len(lats))/elapsed.Seconds(), "obs/s")
	b.ReportMetric(q(0.50), "p50-us")
	b.ReportMetric(q(0.99), "p99-us")
}
