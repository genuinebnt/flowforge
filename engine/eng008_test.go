package engine_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/engine/enginetest"
	"github.com/genuinebnt/flowforge/eventstore"
	"github.com/genuinebnt/flowforge/testkit"
)

// multiHarness registers the multi-step workflow with idempotent tools.
func multiHarness(t testing.TB, configure ...func(*enginetest.Harness)) (*enginetest.Harness, engine.Run) {
	t.Helper()
	h := enginetest.New(t, configure...)
	h.Exec.Handle("echo", enginetest.Echo())
	h.Exec.Handle("httpx", func(ctx context.Context, call engine.ToolCall) (engine.ToolResult, error) {
		return engine.ToolResult{
			Output:       mustJSON(map[string]any{"url": "https://api.acme.com/", "status": 200}),
			Observations: []jsonRaw{mustJSON(map[string]any{"entities": []any{map[string]any{"ref": "u"}}, "edges": []any{}})},
		}, nil
	})
	c := enginetest.Analyst(enginetest.TenantA)
	h.Register(c, "multi", enginetest.Multi, enginetest.ProgramA)
	r := h.Submit(c, "multi", enginetest.ProgramA, map[string]any{"roots": []string{"api.acme.com"}}, "k")
	return h, r
}

// runWithCrashes runs the multi-step workflow, killing a worker after
// each number of appends in crashes (counted per worker), and finishes
// with a healthy worker. It returns the normalized log and the output.
func runWithCrashes(t testing.TB, crashes []int, configure ...func(*enginetest.Harness)) (string, string) {
	t.Helper()
	h, r := multiHarness(t, configure...)
	for i, n := range crashes {
		cs := enginetest.NewCrashStore(h.Store, n)
		w := h.NewEngine(cs).NewWorker(eventstore.WorkerID(fmt.Sprintf("doomed-%d", i)))
		for j := 0; j < 1000 && !cs.Crashed(); j++ {
			ok, err := w.RunOnce(h.Ctx())
			if err != nil {
				break
			}
			if !ok {
				info, _ := h.Store.GetRun(h.Ctx(), r.ID)
				if engine.Terminal(info.Status) {
					break
				}
				h.Clock.Advance(max(info.NextWakeAt.Sub(h.Clock.Now()), time.Millisecond))
			}
		}
		h.Clock.Advance(h.Engine.Config().LeaseTTL + time.Second)
	}
	if st := h.RunToEnd(h.Engine.NewWorker("survivor"), r.ID); st != engine.StatusSucceeded {
		t.Fatalf("crashes %v: status %s\n%s", crashes, st, enginetest.NormalJSON(enginetest.Normalize(h.Events(r.ID))))
	}
	got, err := h.Engine.GetRun(h.Ctx(), enginetest.Analyst(enginetest.TenantA), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	return enginetest.NormalJSON(enginetest.Normalize(h.Events(r.ID))), string(got.Output)
}

// appendsOf counts the appends of an uninterrupted run.
func appendsOf(t *testing.T) int {
	h, r := multiHarness(t)
	cs := enginetest.NewCrashStore(h.Store, -1)
	w := h.NewEngine(cs).NewWorker("counter")
	for {
		if st := h.RunToEnd(w, r.ID); engine.Terminal(st) {
			return cs.Appends()
		}
	}
}

func TestEng008_CrashRecovery(t *testing.T) {
	want, wantOut := runWithCrashes(t, nil)
	if wantOut != "203" {
		t.Fatalf("output %s", wantOut)
	}
	n := appendsOf(t)
	if n < 10 {
		t.Fatalf("only %d appends", n)
	}
	for k := 0; k < n; k++ {
		got, out := runWithCrashes(t, []int{k})
		if got != want || out != wantOut {
			t.Fatalf("crash after %d appends: log differs from the uninterrupted run\n got: %s\nwant: %s", k, got, want)
		}
	}
}

func TestEngProp_CrashAnywhere(t *testing.T) {
	want, _ := runWithCrashes(t, nil)
	n := appendsOf(t)
	rng := testkit.Rng(t)
	cases := max(testkit.Cases()/20, 5)
	for c := 0; c < cases; c++ {
		crashes := make([]int, 1+core.IntN(rng, 4))
		for i := range crashes {
			crashes[i] = core.IntN(rng, n)
		}
		got, _ := runWithCrashes(t, crashes)
		if got != want {
			t.Fatalf("crashes %v: log differs\n got: %s\nwant: %s", crashes, got, want)
		}
	}
}

func TestEngSnapshots_AreACache(t *testing.T) {
	plain, out := runWithCrashes(t, nil)
	snap := func(h *enginetest.Harness) { h.Config.SnapshotEveryEffects = 2 }
	withSnaps, out2 := runWithCrashes(t, []int{9, 4}, snap)
	if out != out2 {
		t.Fatalf("outputs %s and %s", out, out2)
	}
	// The logs agree once snapshot_taken events are removed.
	strip := func(s string) []string {
		var keep []string
		for _, line := range splitLines(s) {
			if !containsStr(line, `"snapshot_taken"`) {
				keep = append(keep, stripSeq(line))
			}
		}
		return keep
	}
	a, b := strip(plain), strip(withSnaps)
	if len(a) != len(b) {
		t.Fatalf("logs differ in length: %d vs %d\n%s", len(a), len(b), withSnaps)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("event %d differs:\n%s\n%s", i, a[i], b[i])
		}
	}
	if !containsStr(withSnaps, `"snapshot_taken"`) {
		t.Fatal("no snapshot was taken")
	}
}
