package engine_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/engine/enginetest"
	"github.com/genuinebnt/flowforge/eventstore"
)

func countType(evs []eventstore.Event, typ, path string) int {
	n := 0
	for _, e := range evs {
		if e.Type != typ {
			continue
		}
		var p struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		if path == "" || p.Path == path {
			n++
		}
	}
	return n
}

// crashAfter runs the run with a worker whose store dies after n appends,
// then lets the lease expire.
func crashAfter(t *testing.T, h *enginetest.Harness, n int) {
	t.Helper()
	cs := enginetest.NewCrashStore(h.Store, n)
	w := h.NewEngine(cs).NewWorker("doomed")
	for {
		ok, err := w.RunOnce(h.Ctx())
		if err != nil || !ok {
			break
		}
	}
	if !cs.Crashed() {
		t.Fatalf("worker did not reach crash point %d", n)
	}
	h.Clock.Advance(h.Engine.Config().LeaseTTL + time.Second)
}

func TestEng003_Idempotency(t *testing.T) {
	t.Run("idempotent_tool_reexecuted_with_same_key", func(t *testing.T) {
		h := enginetest.New(t)
		h.Exec.Handle("echo", enginetest.Echo())
		c := enginetest.Analyst(enginetest.TenantA)
		h.Register(c, "chain", enginetest.Chain(2), core.Nil)
		r := h.Submit(c, "chain", core.Nil, map[string]any{}, "k")
		// Appends: run_started, effect_scheduled 0, effect_started 0, effect_completed 0 <- crash.
		crashAfter(t, h, 3)
		if st := h.RunToEnd(h.Engine.NewWorker("w2"), r.ID); st != engine.StatusSucceeded {
			t.Fatalf("status %s", st)
		}
		calls := h.Exec.CallsTo("echo")
		var first []engine.ToolCall
		for _, c := range calls {
			if c.Path == "0" {
				first = append(first, c)
			}
		}
		if len(first) != 2 || first[0].IdempotencyKey != first[1].IdempotencyKey ||
			first[0].IdempotencyKey != engine.IdempotencyKey(r.ID, "0") {
			t.Fatalf("calls of effect 0: %+v", first)
		}
		evs := h.Events(r.ID)
		if countType(evs, engine.EvEffectCompleted, "0") != 1 || countType(evs, engine.EvEffectStarted, "0") != 1 {
			t.Fatalf("effect 0 recorded more than once:\n%s", enginetest.NormalJSON(enginetest.Normalize(evs)))
		}
		got, _ := h.Engine.GetRun(h.Ctx(), c, r.ID)
		if string(got.Output) != "2" {
			t.Fatalf("output %s", got.Output)
		}
	})

	t.Run("non_idempotent_outcome_unknown", func(t *testing.T) {
		for _, tc := range []struct {
			resolution string
			result     json.RawMessage
			want       engine.RunStatus
			posts      int
		}{
			{engine.ResolveAssumeCompleted, json.RawMessage(`"id-0"`), engine.StatusSucceeded, 1},
			{engine.ResolveRetry, nil, engine.StatusSucceeded, 2},
			{engine.ResolveAssumeFailed, nil, engine.StatusFailed, 1},
		} {
			t.Run(tc.resolution, func(t *testing.T) {
				h := enginetest.New(t)
				h.Exec.Handle("post", enginetest.Output("id-0"))
				h.Exec.Handle("block", enginetest.Echo())
				h.Exec.Handle("undo", enginetest.Output(nil))
				c := enginetest.Analyst(enginetest.TenantA)
				h.Register(c, "saga", enginetest.Saga(enginetest.SagaOptions{Steps: 1}), core.Nil)
				r := h.Submit(c, "saga", core.Nil, map[string]any{"amount": 10}, "k")
				crashAfter(t, h, 3) // dies after post ran, before its result is recorded
				w := h.Engine.NewWorker("w2")
				if st := h.RunToEnd(w, r.ID); st != engine.StatusWaiting {
					t.Fatalf("status %s, want waiting for an operator", st)
				}
				got, _ := h.Engine.GetRun(h.Ctx(), c, r.ID)
				if got.WaitingOn == nil || got.WaitingOn.Kind != "operator" || got.WaitingOn.Path != "0" {
					t.Fatalf("waiting on %+v", got.WaitingOn)
				}
				if _, err := h.Engine.ResolveUnknownEffect(h.Ctx(), c, r.ID, "0", "bogus", nil); err == nil {
					t.Fatal("bogus resolution accepted")
				}
				if _, err := h.Engine.ResolveUnknownEffect(h.Ctx(), c, r.ID, "0", tc.resolution, tc.result); err != nil {
					t.Fatal(err)
				}
				if st := h.RunToEnd(w, r.ID); st != tc.want {
					t.Fatalf("status %s, want %s", st, tc.want)
				}
				if n := len(h.Exec.CallsTo("post")); n != tc.posts {
					t.Fatalf("post called %d times, want %d", n, tc.posts)
				}
				evs := h.Events(r.ID)
				if countType(evs, engine.EvEffectOutcomeUnknown, "0") != 1 {
					t.Fatal("no effect_outcome_unknown")
				}
			})
		}
	})
}
