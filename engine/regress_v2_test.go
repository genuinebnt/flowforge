package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/engine/enginetest"
	"github.com/genuinebnt/flowforge/eventstore"
)

// failedPayloads returns the effect_failed payloads of a log.
func failedPayloads(t *testing.T, evs []eventstore.Event) []engine.EffectFailedPayload {
	t.Helper()
	var out []engine.EffectFailedPayload
	for _, e := range evs {
		if e.Type == engine.EvEffectFailed {
			var p engine.EffectFailedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			out = append(out, p)
		}
	}
	return out
}

// TestRegress_G_SVC_09_RetryOnCodes: the engine retries a retryable error
// only when its code is in the tool's retry_on list (spec 6.2, 9.5), and
// at most max_attempts times.
func TestRegress_G_SVC_09_RetryOnCodes(t *testing.T) {
	for _, tc := range []struct {
		code  string
		calls int32
	}{{"unavailable", 3}, {"flaky_upstream", 1}} {
		t.Run(tc.code, func(t *testing.T) {
			h := enginetest.New(t)
			spec := enginetest.FlakySpec
			spec.Retry.MaxAttempts, spec.Retry.RetryOn = 3, []string{"unavailable", "deadline_exceeded"}
			h.Tools.Register(spec)
			var calls atomic.Int32
			h.Exec.Handle("flaky", func(context.Context, engine.ToolCall) (engine.ToolResult, error) {
				calls.Add(1)
				return engine.ToolResult{}, &engine.ToolError{Code: tc.code, Message: "503", Retryable: true}
			})
			c := enginetest.Analyst(enginetest.TenantA)
			h.Register(c, "flaky", enginetest.Flaky, core.Nil)
			r := h.Submit(c, "flaky", core.Nil, map[string]any{}, "k")
			if st := h.RunToEnd(h.Engine.NewWorker("w"), r.ID); st != engine.StatusFailed || calls.Load() != tc.calls {
				t.Fatalf("status %s after %d calls, want failed after %d", st, calls.Load(), tc.calls)
			}
		})
	}
}

// TestRegress_G_SVC_09_IdempotencyClass: after a crash between the call
// and its recorded result, a keyed tool is re-executed with the same
// idempotency key and a non_idempotent one waits for an operator; the
// class takes precedence over the idempotent flag (fail safe).
func TestRegress_G_SVC_09_IdempotencyClass(t *testing.T) {
	for _, tc := range []struct {
		name        string
		idempotent  bool
		class       string
		wantStatus  engine.RunStatus
		wantUnknown int
	}{
		{"keyed", true, engine.IdempotencyKeyed, engine.StatusSucceeded, 0},
		{"non_idempotent_wins_over_flag", true, engine.IdempotencyNone, engine.StatusWaiting, 1},
		{"flag_only", false, "", engine.StatusWaiting, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := enginetest.New(t)
			spec := enginetest.EchoSpec
			spec.Idempotent, spec.Idempotency = tc.idempotent, tc.class
			h.Tools.Register(spec)
			h.Exec.Handle("echo", enginetest.Echo())
			c := enginetest.Analyst(enginetest.TenantA)
			h.Register(c, "chain", enginetest.Chain(2), core.Nil)
			r := h.Submit(c, "chain", core.Nil, map[string]any{}, "k")
			crashAfter(t, h, 3) // after echo 0 ran, before its result is recorded
			if st := h.RunToEnd(h.Engine.NewWorker("w2"), r.ID); st != tc.wantStatus {
				t.Fatalf("status %s, want %s", st, tc.wantStatus)
			}
			if n := countType(h.Events(r.ID), engine.EvEffectOutcomeUnknown, "0"); n != tc.wantUnknown {
				t.Fatalf("%d effect_outcome_unknown, want %d", n, tc.wantUnknown)
			}
			if tc.wantUnknown == 0 {
				calls := h.Exec.CallsTo("echo")
				if len(calls) < 2 || calls[0].Path != calls[1].Path || calls[0].IdempotencyKey != calls[1].IdempotencyKey {
					t.Fatalf("keyed re-execution: %+v", calls)
				}
			}
		})
	}
}

// TestRegress_G_SVC_09_Validate pins ToolSpec.Validate.
func TestRegress_G_SVC_09_Validate(t *testing.T) {
	ok := enginetest.FlakySpec
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*engine.ToolSpec){
		"too_many_attempts":  func(s *engine.ToolSpec) { s.Retry.MaxAttempts = engine.MaxToolAttempts + 1 },
		"negative_attempts":  func(s *engine.ToolSpec) { s.Retry.MaxAttempts = -1 },
		"empty_code":         func(s *engine.ToolSpec) { s.Retry.RetryOn = []string{""} },
		"max_below_initial":  func(s *engine.ToolSpec) { s.Retry.Backoff.MaxMs = 10 },
		"shrinking":          func(s *engine.ToolSpec) { s.Retry.Backoff.Multiplier = 0.5 },
		"jitter":             func(s *engine.ToolSpec) { s.Retry.Backoff.Jitter = "equal" },
		"keyed_not_flagged":  func(s *engine.ToolSpec) { s.Idempotency = engine.IdempotencyKeyed },
		"none_but_flagged":   func(s *engine.ToolSpec) { s.Idempotent, s.Idempotency = true, engine.IdempotencyNone },
		"unknown_class_name": func(s *engine.ToolSpec) { s.Idempotency = "mostly" },
	} {
		s := ok
		mut(&s)
		if err := s.Validate(); !errors.Is(err, engine.ErrInvalidToolSpec) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// edgeDenied answers the first n calls with a rate-limit denial from the
// network edge (rate_limited with a retry delay), then echoes.
func edgeDenied(n int32, wait time.Duration, calls *atomic.Int32) enginetest.ToolHandler {
	return func(_ context.Context, call engine.ToolCall) (engine.ToolResult, error) {
		if calls.Add(1) <= n {
			return engine.ToolResult{}, &engine.ToolError{Code: engine.CodeRateLimited, Message: "429 at the proxy", Retryable: true, RetryAfter: wait}
		}
		return engine.ToolResult{Output: call.Input}, nil
	}
}

// TestRegress_G_ENG_09_EdgeRateLimitUncounted: a rate-limit denial at the
// network edge is not a failure (spec 9.5): the effect waits exactly the
// edge's delay and the attempt is not counted, so more denials than
// max_attempts still end in success; Config.MaxEdgeWaits bounds the
// uncounted waits, after which denials count as ordinary retryable
// failures.
func TestRegress_G_ENG_09_EdgeRateLimitUncounted(t *testing.T) {
	const wait = 1500 * time.Millisecond
	t.Run("not_counted", func(t *testing.T) {
		h := enginetest.New(t)
		var calls atomic.Int32
		h.Exec.Handle("flaky", edgeDenied(6, wait, &calls)) // FlakySpec allows 4 attempts
		c := enginetest.Analyst(enginetest.TenantA)
		h.Register(c, "flaky", enginetest.Flaky, core.Nil)
		r := h.Submit(c, "flaky", core.Nil, map[string]any{}, "k")
		if st := h.RunToEnd(h.Engine.NewWorker("w"), r.ID); st != engine.StatusSucceeded || calls.Load() != 7 {
			t.Fatalf("status %s after %d calls", st, calls.Load())
		}
		evs := h.Events(r.ID)
		fs := failedPayloads(t, evs)
		if len(fs) != 6 {
			t.Fatalf("%d failures", len(fs))
		}
		for _, f := range fs {
			if !f.Uncounted || f.Final || f.Error.Code != engine.CodeRateLimited {
				t.Fatalf("edge denial recorded as %+v", f)
			}
		}
		_, rs := retries(t, evs)
		for _, r := range rs {
			if d := r.retryAt.Sub(r.failedAt); d < wait-time.Millisecond || d > wait+time.Millisecond {
				t.Fatalf("waited %v, want the edge's %v", d, wait)
			}
		}
	})
	t.Run("bounded", func(t *testing.T) {
		h := enginetest.New(t, func(h *enginetest.Harness) { h.Config.MaxEdgeWaits = 2 })
		var calls atomic.Int32
		h.Exec.Handle("flaky", edgeDenied(1000, wait, &calls))
		c := enginetest.Analyst(enginetest.TenantA)
		h.Register(c, "flaky", enginetest.Flaky, core.Nil)
		r := h.Submit(c, "flaky", core.Nil, map[string]any{}, "k")
		// 2 uncounted waits, then the 4 attempts of the policy.
		if st := h.RunToEnd(h.Engine.NewWorker("w"), r.ID); st != engine.StatusFailed || calls.Load() != 6 {
			t.Fatalf("status %s after %d calls", st, calls.Load())
		}
		uncounted := 0
		for _, f := range failedPayloads(t, h.Events(r.ID)) {
			if f.Uncounted {
				uncounted++
			}
		}
		if uncounted != 2 {
			t.Fatalf("%d uncounted waits, want 2", uncounted)
		}
	})
}

// TestRegress_G_ENG_09_KillSwitchRace: a kill switch engaged after the
// engine's re-check is enforced by the network edge, which refuses the
// call (kill_switch_active). The engine does not record the refusal as
// the call's result (the call stays in flight until the cancellation
// closes it as cancelled) and cancels the run with the reason kill_switch_race:<level>, so the race is
// reported in the log.
func TestRegress_G_ENG_09_KillSwitchRace(t *testing.T) {
	h := enginetest.New(t)
	var calls atomic.Int32
	h.Exec.Handle("flaky", func(context.Context, engine.ToolCall) (engine.ToolResult, error) {
		calls.Add(1)
		// The switch flips while the call is on its way to the edge.
		h.Scope.SetKillSwitch(engine.KillTenant, enginetest.TenantA, true)
		return engine.ToolResult{}, &engine.ToolError{Code: engine.CodeKillSwitch, Message: "egress proxy: kill switch"}
	})
	c := enginetest.Analyst(enginetest.TenantA)
	h.Register(c, "flaky", enginetest.Flaky, core.Nil)
	r := h.Submit(c, "flaky", core.Nil, map[string]any{}, "k")
	if st := h.RunToEnd(h.Engine.NewWorker("w"), r.ID); st != engine.StatusCancelled || calls.Load() != 1 {
		t.Fatalf("status %s after %d calls", st, calls.Load())
	}
	evs := h.Events(r.ID)
	// The refusal is not the effect's result: the only close is the one
	// cancellation writes for every in-flight effect.
	fs := failedPayloads(t, evs)
	if countType(evs, engine.EvEffectCompleted, "0") != 0 || len(fs) != 1 || fs[0].Error.Code != "cancelled" {
		t.Fatalf("outcomes recorded for the refused call: %+v", fs)
	}
	var reason string
	for _, e := range evs {
		if e.Type == engine.EvRunCancelled {
			var p engine.CancelPayload
			_ = json.Unmarshal(e.Payload, &p)
			reason = p.Reason
		}
	}
	if reason != "kill_switch_race:"+engine.KillTenant {
		t.Fatalf("cancel reason %q", reason)
	}
}
