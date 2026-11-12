package engine_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/engine/enginetest"
)

// blockingSaga registers a saga with two compensated posts followed by a
// block step that waits until its context is cancelled.
func blockingSaga(t *testing.T, deadline time.Duration) (*enginetest.Harness, engine.Run, chan struct{}) {
	t.Helper()
	h := enginetest.New(t)
	n := 0
	h.Exec.Handle("post", func(ctx context.Context, call engine.ToolCall) (engine.ToolResult, error) {
		n++
		return engine.ToolResult{Output: mustJSON("id-" + call.Path)}, nil
	})
	h.Exec.Handle("undo", enginetest.Output(nil))
	started := make(chan struct{}, 1)
	h.Exec.Handle("block", enginetest.Blocking(started, make(chan struct{})))
	c := enginetest.Analyst(enginetest.TenantA)
	h.Register(c, "saga", enginetest.Saga(enginetest.SagaOptions{Steps: 2, Block: true}), core.Nil)
	r, err := h.SubmitErr(c, "saga", core.Nil, map[string]any{"amount": 1}, "k", deadline)
	if err != nil {
		t.Fatal(err)
	}
	return h, r, started
}

func undoOrder(h *enginetest.Harness) []string {
	var out []string
	for _, c := range h.Exec.CallsTo("undo") {
		var id string
		_ = json.Unmarshal(c.Input, &id)
		out = append(out, id)
	}
	return out
}

func TestEng005_CancelDeadline(t *testing.T) {
	c := enginetest.Analyst(enginetest.TenantA)

	t.Run("cancel_during_tool_call", func(t *testing.T) {
		h, r, started := blockingSaga(t, 0)
		w := h.Engine.NewWorker("w")
		done := make(chan error, 1)
		go func() { _, err := w.RunOnce(h.Ctx()); done <- err }()
		<-started
		got, err := h.Engine.CancelRun(h.Ctx(), c, r.ID, "operator stop")
		if err != nil || got.Status != engine.StatusCancelling {
			t.Fatalf("cancel: %v %v", got.Status, err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		h.RunToEnd(w, r.ID)
		got, _ = h.Engine.GetRun(h.Ctx(), c, r.ID)
		if got.Status != engine.StatusCancelled || got.CompensationOutcome != engine.CompensationCompleted {
			t.Fatalf("run %s compensation %s", got.Status, got.CompensationOutcome)
		}
		if o := undoOrder(h); len(o) != 2 || o[0] != "id-1" || o[1] != "id-0" {
			t.Fatalf("compensations ran %v, want newest first", o)
		}
		types := enginetest.Types(h.Events(r.ID))
		want := []string{engine.EvRunCancelRequested, engine.EvEffectFailed, engine.EvCompensationStarted}
		if !containsSeq(types, want) || types[len(types)-1] != engine.EvRunCancelled {
			t.Fatalf("events %v", types)
		}
		_, err = h.Engine.CancelRun(h.Ctx(), c, r.ID, "again")
		wantReason(t, err, core.KindFailedPrecondition, core.ReasonRunNotCancellable)
	})

	t.Run("deadline_during_tool_call", func(t *testing.T) {
		h, r, started := blockingSaga(t, 10*time.Second)
		w := h.Engine.NewWorker("w")
		done := make(chan error, 1)
		go func() { _, err := w.RunOnce(h.Ctx()); done <- err }()
		<-started
		h.Clock.Advance(11 * time.Second)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		h.RunToEnd(w, r.ID)
		got, _ := h.Engine.GetRun(h.Ctx(), c, r.ID)
		if got.Status != engine.StatusFailed || got.FailureCode != "timeout" || got.CompensationOutcome != engine.CompensationCompleted {
			t.Fatalf("run %s %s compensation %s", got.Status, got.FailureCode, got.CompensationOutcome)
		}
		if len(undoOrder(h)) != 2 {
			t.Fatalf("undo calls %v", undoOrder(h))
		}
	})

	t.Run("deadline_while_waiting", func(t *testing.T) {
		h := enginetest.New(t)
		h.Exec.Handle("post", enginetest.Output("id"))
		c := enginetest.Analyst(enginetest.TenantA)
		h.Register(c, "saga", enginetest.Saga(enginetest.SagaOptions{Approval: true, Steps: 1, TTL: 24 * time.Hour}), core.Nil)
		r, err := h.SubmitErr(c, "saga", core.Nil, map[string]any{"amount": 1}, "k", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		h.RunToEnd(h.Engine.NewWorker("w"), r.ID)
		got, _ := h.Engine.GetRun(h.Ctx(), c, r.ID)
		if got.Status != engine.StatusFailed || got.FailureCode != "timeout" || !got.FinishedAt.Before(enginetest.Start.Add(2*time.Minute)) {
			t.Fatalf("run %s %s at %v", got.Status, got.FailureCode, got.FinishedAt)
		}
	})

	t.Run("cancel_pending_run", func(t *testing.T) {
		h, r, _ := blockingSaga(t, 0)
		if _, err := h.Engine.CancelRun(h.Ctx(), c, r.ID, "never mind"); err != nil {
			t.Fatal(err)
		}
		h.RunToEnd(h.Engine.NewWorker("w"), r.ID)
		types := enginetest.Types(h.Events(r.ID))
		if len(types) != 3 || types[1] != engine.EvRunCancelRequested || types[2] != engine.EvRunCancelled {
			t.Fatalf("events %v", types)
		}
	})
}

// containsSeq reports whether want appears in order (not necessarily
// adjacent) in got.
func containsSeq(got, want []string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}
