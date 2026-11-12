package engine_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/engine/enginetest"
)

// BenchmarkEng01_RunThroughput is b_eng_01: runs of 5, 20 and 100 tool
// steps with FakeToolExec at 0 ms (MemStore, MemRepo). It reports runs/s
// and the per-step engine overhead (all time is engine time, since the
// tool takes none).
func BenchmarkEng01_RunThroughput(b *testing.B) {
	for _, steps := range []int{5, 20, 100} {
		b.Run(fmt.Sprintf("steps=%d", steps), func(b *testing.B) {
			h := enginetest.New(b)
			h.Exec.Handle("echo", enginetest.Echo())
			c := enginetest.Analyst(enginetest.TenantA)
			h.Register(c, "chain", enginetest.Chain(steps), core.Nil)
			w := h.Engine.NewWorker("bench")
			b.ReportAllocs()
			i := 0
			start := time.Now()
			for b.Loop() {
				i++
				r := h.Submit(c, "chain", core.Nil, map[string]any{}, fmt.Sprint(i))
				if st := h.RunToEnd(w, r.ID); st != engine.StatusSucceeded {
					b.Fatalf("status %s", st)
				}
			}
			el := time.Since(start)
			b.ReportMetric(float64(i)/el.Seconds(), "runs/s")
			b.ReportMetric(float64(el.Nanoseconds())/float64(i*steps), "ns/step")
		})
	}
}
