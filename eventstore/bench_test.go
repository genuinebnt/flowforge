package eventstore_test

import (
	"fmt"
	"testing"

	"github.com/genuinebnt/flowforge/eventstore/storetest"
)

// BenchmarkES01_Append is b_es_01 for MemStore: 1, 16 and 256 concurrent
// runs, batches of 1, 10 and 100 events.
func BenchmarkES01_Append(b *testing.B) {
	for _, runs := range []int{1, 16, 256} {
		for _, batch := range []int{1, 10, 100} {
			b.Run(fmt.Sprintf("runs=%d/batch=%d", runs, batch), func(b *testing.B) {
				storetest.BenchAppend(b, memHarness(b), runs, batch)
			})
		}
	}
}

// BenchmarkES02_LeaseClaim is b_es_02 for MemStore: 100k runs, 1 to 1k
// workers.
func BenchmarkES02_LeaseClaim(b *testing.B) {
	for _, workers := range []int{1, 16, 1000} {
		b.Run(fmt.Sprintf("runs=100000/workers=%d", workers), func(b *testing.B) {
			storetest.BenchLeaseClaim(b, memHarness(b), 100_000, workers)
		})
	}
}
