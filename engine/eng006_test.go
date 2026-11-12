package engine_test

import (
	"context"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/engine/enginetest"
)

func TestEng006_KillSwitch(t *testing.T) {
	c := enginetest.Analyst(enginetest.TenantA)

	t.Run("mid_run", func(t *testing.T) {
		h := enginetest.New(t)
		reconHandlers(h)
		h.Exec.Handle("subfinder", enginetest.Output([]string{"a.acme.com", "b.acme.com", "c.acme.com"}))
		inner := enginetest.Output(map[string]any{"url": "https://a.acme.com/", "status": 200})
		h.Exec.Handle("httpx", func(ctx context.Context, call engine.ToolCall) (engine.ToolResult, error) {
			h.Scope.SetKillSwitch(engine.KillProgram, enginetest.ProgramA, true)
			return inner(ctx, call)
		})
		h.Register(c, "recon", enginetest.Recon, enginetest.ProgramA)
		r := h.Submit(c, "recon", enginetest.ProgramA, map[string]any{"roots": []string{"acme.com"}}, "k")
		if st := h.RunToEnd(h.Engine.NewWorker("w"), r.ID); st != engine.StatusCancelled {
			t.Fatalf("status %s", st)
		}
		if n := len(h.Exec.CallsTo("httpx")); n != 1 {
			t.Fatalf("httpx called %d times after the kill switch", n)
		}
		got, _ := h.Engine.GetRun(h.Ctx(), c, r.ID)
		if got.FailureMessage != "kill_switch:program" {
			t.Fatalf("reason %q", got.FailureMessage)
		}
		// New target-facing runs are refused while the switch is engaged.
		_, err := h.SubmitErr(c, "recon", enginetest.ProgramA, map[string]any{"roots": []string{"acme.com"}}, "k2", 0)
		wantReason(t, err, core.KindPermissionDenied, core.ReasonKillSwitchActive)
	})

	t.Run("handle_kill_switch_cancels_waiting_runs", func(t *testing.T) {
		h := enginetest.New(t)
		h.Exec.Handle("post", enginetest.Output("id"))
		h.Register(c, "saga", enginetest.Saga(enginetest.SagaOptions{Approval: true, Steps: 1}), core.Nil)
		r := h.Submit(c, "saga", core.Nil, map[string]any{"amount": 1}, "k")
		w := h.Engine.NewWorker("w")
		h.Drain(w)
		if err := h.Engine.HandleKillSwitch(h.Ctx(), engine.KillTenant, enginetest.TenantA); err != nil {
			t.Fatal(err)
		}
		if st := h.RunToEnd(w, r.ID); st != engine.StatusCancelled {
			t.Fatalf("status %s", st)
		}
		as, _, _ := h.Engine.ListApprovals(h.Ctx(), c, engine.ApprovalFilter{RunID: r.ID}, engine.Page{})
		if len(as) != 1 || as[0].Status != engine.ApprovalCancelled {
			t.Fatalf("approvals %+v", as)
		}
	})
}
