package engine_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/engine/enginetest"
	"github.com/genuinebnt/flowforge/eventstore"
)

// flakyUntil fails the first n calls with a retryable error.
func flakyUntil(n int32, calls *atomic.Int32) enginetest.ToolHandler {
	return func(ctx context.Context, call engine.ToolCall) (engine.ToolResult, error) {
		if calls.Add(1) <= n {
			return engine.ToolResult{}, &engine.ToolError{Code: "unavailable", Message: "upstream 503", Retryable: true}
		}
		return engine.ToolResult{Output: call.Input}, nil
	}
}

type retryEvent struct {
	failedAt time.Time
	attempt  int
	retryAt  time.Time
}

func retries(t *testing.T, evs []eventstore.Event) (started []int, rs []retryEvent) {
	t.Helper()
	var lastFail time.Time
	for _, e := range evs {
		switch e.Type {
		case engine.EvEffectStarted:
			var p engine.EffectStartedPayload
			_ = json.Unmarshal(e.Payload, &p)
			started = append(started, p.Attempt)
		case engine.EvEffectFailed:
			lastFail = e.At
		case engine.EvEffectRetryScheduled:
			var p engine.EffectRetryScheduledPayload
			_ = json.Unmarshal(e.Payload, &p)
			at, err := core.ParseTime(p.RetryAt)
			if err != nil {
				t.Fatal(err)
			}
			rs = append(rs, retryEvent{failedAt: lastFail, attempt: p.Attempt, retryAt: at})
		}
	}
	return started, rs
}

func TestEng002_RetryBackoff(t *testing.T) {
	policy := enginetest.FlakySpec.Retry.Backoff
	run := func(t *testing.T, seed uint64, failures int32) (engine.Run, []eventstore.Event, int32) {
		h := enginetest.New(t, func(h *enginetest.Harness) { h.Seed = seed })
		var calls atomic.Int32
		h.Exec.Handle("flaky", flakyUntil(failures, &calls))
		c := enginetest.Analyst(enginetest.TenantA)
		h.Register(c, "flaky", enginetest.Flaky, core.Nil)
		r := h.Submit(c, "flaky", core.Nil, map[string]any{}, "k")
		h.RunToEnd(h.Engine.NewWorker("w"), r.ID)
		got, err := h.Engine.GetRun(h.Ctx(), c, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got, h.Events(r.ID), calls.Load()
	}

	t.Run("succeeds_after_retries", func(t *testing.T) {
		got, evs, calls := run(t, 11, 3)
		if got.Status != engine.StatusSucceeded || calls != 4 {
			t.Fatalf("status %s after %d calls", got.Status, calls)
		}
		started, rs := retries(t, evs)
		if len(started) != 4 || started[0] != 1 || started[3] != 4 {
			t.Fatalf("attempts %v", started)
		}
		if len(rs) != 3 {
			t.Fatalf("retries %v", rs)
		}
		for i, r := range rs {
			delay := engine.BackoffDelay(policy, r.attempt)
			wait := r.retryAt.Sub(r.failedAt)
			if r.attempt != i+1 || wait < -time.Millisecond || wait > delay {
				t.Fatalf("retry %d: attempt %d waits %v, want within [0, %v]", i, r.attempt, wait, delay)
			}
		}
	})
	t.Run("seeded_jitter_reproduces", func(t *testing.T) {
		_, a, _ := run(t, 99, 3)
		_, b, _ := run(t, 99, 3)
		_, ra := retries(t, a)
		_, rb := retries(t, b)
		for i := range ra {
			if !ra[i].retryAt.Equal(rb[i].retryAt) {
				t.Fatalf("retry %d at %v and %v with the same seed", i, ra[i].retryAt, rb[i].retryAt)
			}
		}
	})
	t.Run("exhausted", func(t *testing.T) {
		got, evs, calls := run(t, 5, 100)
		if got.Status != engine.StatusFailed || got.FailureCode != "effect_failed" || calls != 4 {
			t.Fatalf("status %s %s after %d calls", got.Status, got.FailureCode, calls)
		}
		started, rs := retries(t, evs)
		if len(started) != 4 || len(rs) != 3 {
			t.Fatalf("attempts %v retries %d", started, len(rs))
		}
	})
	t.Run("permanent_error_not_retried", func(t *testing.T) {
		h := enginetest.New(t)
		var calls atomic.Int32
		h.Exec.Handle("flaky", func(ctx context.Context, call engine.ToolCall) (engine.ToolResult, error) {
			calls.Add(1)
			return engine.ToolResult{}, &engine.ToolError{Code: "invalid_input", Message: "400", Retryable: false}
		})
		c := enginetest.Analyst(enginetest.TenantA)
		h.Register(c, "flaky", enginetest.Flaky, core.Nil)
		r := h.Submit(c, "flaky", core.Nil, map[string]any{}, "k")
		if st := h.RunToEnd(h.Engine.NewWorker("w"), r.ID); st != engine.StatusFailed || calls.Load() != 1 {
			t.Fatalf("status %s calls %d", st, calls.Load())
		}
	})
	t.Run("deadline_stops_retries", func(t *testing.T) {
		h := enginetest.New(t)
		var calls atomic.Int32
		h.Exec.Handle("flaky", flakyUntil(100, &calls))
		c := enginetest.Analyst(enginetest.TenantA)
		h.Register(c, "flaky", enginetest.Flaky, core.Nil)
		r, err := h.SubmitErr(c, "flaky", core.Nil, map[string]any{}, "k", 50*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		h.RunToEnd(h.Engine.NewWorker("w"), r.ID)
		got, _ := h.Engine.GetRun(h.Ctx(), c, r.ID)
		if got.Status != engine.StatusFailed || calls.Load() >= 4 {
			t.Fatalf("status %s after %d calls", got.Status, calls.Load())
		}
	})
	t.Run("delay_formula", func(t *testing.T) {
		b := engine.Backoff{InitialMs: 1000, MaxMs: 30000, Multiplier: 2}
		for n, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 5: 16 * time.Second, 6: 30 * time.Second, 50: 30 * time.Second} {
			if got := engine.BackoffDelay(b, n); got != want {
				t.Errorf("delay(%d) = %v, want %v", n, got, want)
			}
		}
		rng := core.NewSplitMix64(1)
		for range 1000 {
			if w := engine.FullJitter(rng, time.Second); w < 0 || w > time.Second {
				t.Fatalf("jitter %v", w)
			}
		}
	})
}
