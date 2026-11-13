package pgstore_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/eventstore"
	"github.com/genuinebnt/flowforge/eventstore/storetest"
	"github.com/genuinebnt/flowforge/testkit"
)

// BenchmarkES01_Append is b_es_01 for Postgres (skipped without
// FF_TEST_PG_DSN): 1, 16 and 256 concurrent runs (at most 64 pooled
// connections), batches of 1, 10 and 100 events, fsync as configured on the
// server.
func BenchmarkES01_Append(b *testing.B) {
	for _, runs := range []int{1, 16, 256} {
		for _, batch := range []int{1, 10, 100} {
			b.Run(fmt.Sprintf("runs=%d/batch=%d", runs, batch), func(b *testing.B) {
				storetest.BenchAppend(b, pgHarness(b), runs, batch)
			})
		}
	}
}

// BenchmarkES02_LeaseClaim is b_es_02 for Postgres: 10k ready runs (the
// spec's 100k takes minutes to load per sub-benchmark), 1, 16 and 64
// claiming workers through ClaimReady (FOR UPDATE SKIP LOCKED).
func BenchmarkES02_LeaseClaim(b *testing.B) {
	for _, workers := range []int{1, 16, 64} {
		b.Run(fmt.Sprintf("runs=10000/workers=%d", workers), func(b *testing.B) {
			storetest.BenchLeaseClaim(b, pgHarness(b), 10_000, workers)
		})
	}
}

// memHarness is the MemStore harness of package eventstore's tests.
func memHarness() *storetest.Harness {
	clk := testkit.NewFakeClock(time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC))
	clk.SetMonotonic(time.Hour)
	return &storetest.Harness{Store: eventstore.NewMemStore(clk),
		Now:    func(context.Context) (time.Time, error) { return clk.Now(), nil },
		Elapse: func(_ context.Context, d time.Duration) error { clk.Advance(d); return nil }}
}

// BenchmarkESAlgo compares the EventStore implementations (one decision
// point: where run logs live, spec 23.1) on the b_es_01 and b_es_02
// workloads; both pass the same conformance suite (storetest.Run in
// TestES007_AllImplementations and package eventstore). pg needs
// FF_TEST_PG_DSN:
//
//	go test -run '^$' -bench ESAlgo ./eventstore/pgstore
func BenchmarkESAlgo(b *testing.B) {
	stores := []struct {
		name string
		h    func(b *testing.B) *storetest.Harness
	}{
		{"mem", func(*testing.B) *storetest.Harness { return memHarness() }},
		{"pg", func(b *testing.B) *storetest.Harness { return pgHarness(b) }},
	}
	for _, s := range stores {
		b.Run(s.name+"/append/runs=16/batch=10", func(b *testing.B) { storetest.BenchAppend(b, s.h(b), 16, 10) })
		b.Run(s.name+"/lease_claim/runs=10000/workers=16", func(b *testing.B) {
			storetest.BenchLeaseClaim(b, s.h(b), 10_000, 16)
		})
	}
}
