package engine_test

import (
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/engine/enginetest"
)

func TestEng007_SubmitIdempotency(t *testing.T) {
	h := enginetest.New(t)
	h.Exec.Handle("echo", enginetest.Echo())
	c := enginetest.Analyst(enginetest.TenantA)
	h.Register(c, "chain", enginetest.Chain(1), core.Nil)
	a := h.Submit(c, "chain", core.Nil, map[string]any{}, "same")
	b := h.Submit(c, "chain", core.Nil, map[string]any{}, "same")
	if a.ID != b.ID {
		t.Fatalf("two runs %s and %s for one key", a.ID, b.ID)
	}
	runs, _, err := h.Engine.ListRuns(h.Ctx(), c, engine.RunFilter{}, engine.Page{})
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs %d %v", len(runs), err)
	}
	_, err = h.SubmitErr(c, "chain", core.Nil, map[string]any{"x": 1}, "same", 0)
	wantReason(t, err, core.KindAlreadyExists, core.ReasonIdempotencyKeyReused)
	_, err = h.SubmitErr(c, "chain", core.Nil, map[string]any{}, "", 0)
	wantReason(t, err, core.KindInvalidArgument, core.ReasonValidationFailed)

	// The key is scoped to the tenant.
	other := enginetest.Analyst(enginetest.TenantB)
	h.Register(other, "chain", enginetest.Chain(1), core.Nil)
	if o := h.Submit(other, "chain", core.Nil, map[string]any{}, "same"); o.ID == a.ID {
		t.Fatal("tenants share idempotency keys")
	}

	t.Run("deprecated", func(t *testing.T) {
		if _, err := h.Engine.DeprecateWorkflowVersion(h.Ctx(), c, "chain", 1); err != nil {
			t.Fatal(err)
		}
		_, err := h.SubmitErr(c, "chain", core.Nil, map[string]any{}, "new", 0)
		wantReason(t, err, core.KindNotFound, core.ReasonNotFound) // no published version left
		_, err = h.Engine.SubmitRun(h.Ctx(), c, engine.SubmitRunRequest{WorkflowName: "chain", WorkflowVersion: 1, IdempotencyKey: "new2"})
		wantReason(t, err, core.KindFailedPrecondition, core.ReasonWorkflowDeprecated)
		// Runs that already exist continue.
		if st := h.RunToEnd(h.Engine.NewWorker("w"), a.ID); st != engine.StatusSucceeded {
			t.Fatalf("existing run %s", st)
		}
	})
	t.Run("target_facing_needs_program", func(t *testing.T) {
		h.Register(c, "recon", enginetest.Recon, enginetest.ProgramA)
		_, err := h.SubmitErr(c, "recon", core.Nil, map[string]any{"roots": []string{"acme.com"}}, "r1", 0)
		wantReason(t, err, core.KindInvalidArgument, core.ReasonValidationFailed)
		_, err = h.SubmitErr(c, "recon", enginetest.ProgramA, map[string]any{"roots": "acme.com"}, "r2", 0)
		wantReason(t, err, core.KindInvalidArgument, core.ReasonValidationFailed)
	})
}
