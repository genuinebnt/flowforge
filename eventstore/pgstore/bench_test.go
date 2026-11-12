package pgstore_test

import (
	"fmt"
	"testing"

	"github.com/genuinebnt/flowforge/eventstore/storetest"
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
