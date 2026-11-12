package engine_test

import (
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/engine/enginetest"
)

func approvalSetup(t *testing.T, configure ...func(*enginetest.Harness)) (*enginetest.Harness, engine.Run, *engine.Worker) {
	t.Helper()
	h := enginetest.New(t, configure...)
	h.Exec.Handle("post", enginetest.Output("id-0"))
	h.Exec.Handle("undo", enginetest.Output(nil))
	h.Exec.Handle("block", enginetest.Echo())
	c := enginetest.Analyst(enginetest.TenantA)
	h.Register(c, "saga", enginetest.Saga(enginetest.SagaOptions{Approval: true, Steps: 1}), core.Nil)
	r := h.Submit(c, "saga", core.Nil, map[string]any{"amount": 5}, "k")
	w := h.Engine.NewWorker("w")
	h.Drain(w)
	return h, r, w
}

func pendingApproval(t *testing.T, h *enginetest.Harness, c engine.Caller, run core.UUID) engine.Approval {
	t.Helper()
	as, _, err := h.Engine.ListApprovals(h.Ctx(), c, engine.ApprovalFilter{RunID: run, Status: engine.ApprovalPending}, engine.Page{})
	if err != nil || len(as) != 1 {
		t.Fatalf("pending approvals %v %v", as, err)
	}
	return as[0]
}

func TestEng004_Approvals(t *testing.T) {
	requester := enginetest.Analyst(enginetest.TenantA)
	approver := enginetest.Approver(enginetest.TenantA)

	t.Run("approve", func(t *testing.T) {
		h, r, w := approvalSetup(t)
		got, _ := h.Engine.GetRun(h.Ctx(), requester, r.ID)
		if got.Status != engine.StatusWaiting || got.WaitingOn == nil || got.WaitingOn.Kind != "approval" {
			t.Fatalf("run %s waiting on %+v", got.Status, got.WaitingOn)
		}
		a := pendingApproval(t, h, approver, r.ID)
		if a.Role != "finance_ap" || !a.ExpiresAt.Equal(enginetest.Start.Add(time.Hour)) || a.ID.String() != got.WaitingOn.ApprovalID {
			t.Fatalf("approval %+v", a)
		}
		if _, err := h.Engine.DecideApproval(h.Ctx(), requester, a.ID, engine.DecisionApprove, ""); err == nil {
			t.Fatal("caller without the role decided")
		} else {
			wantReason(t, err, core.KindPermissionDenied, core.ReasonForbiddenRole)
		}
		self := requester
		self.Roles = append(self.Roles, "finance_ap")
		_, err := h.Engine.DecideApproval(h.Ctx(), self, a.ID, engine.DecisionApprove, "")
		wantReason(t, err, core.KindPermissionDenied, core.ReasonSelfApprovalForbidden)

		d, err := h.Engine.DecideApproval(h.Ctx(), approver, a.ID, engine.DecisionApprove, "ok to post")
		if err != nil || d.Status != engine.ApprovalApproved || d.DecidedBy != approver.Actor {
			t.Fatalf("decide %+v %v", d, err)
		}
		again, err := h.Engine.DecideApproval(h.Ctx(), approver, a.ID, engine.DecisionApprove, "ok to post")
		if err != nil || again.Status != engine.ApprovalApproved {
			t.Fatalf("idempotent repeat: %v", err)
		}
		_, err = h.Engine.DecideApproval(h.Ctx(), approver, a.ID, engine.DecisionDeny, "")
		wantReason(t, err, core.KindAborted, core.ReasonApprovalAlreadyDecided)
		other := engine.Caller{Tenant: enginetest.TenantA, Actor: "user:other", Roles: []string{"finance_ap"}}
		_, err = h.Engine.DecideApproval(h.Ctx(), other, a.ID, engine.DecisionApprove, "")
		wantReason(t, err, core.KindAborted, core.ReasonApprovalAlreadyDecided)

		if st := h.RunToEnd(w, r.ID); st != engine.StatusSucceeded {
			t.Fatalf("status %s", st)
		}
		evs := h.Events(r.ID)
		if countType(evs, engine.EvApprovalDecided, "") != 1 || len(h.Exec.CallsTo("post")) != 1 {
			t.Fatal("approval not recorded once or post not called once")
		}
		if len(h.Pub.Envelopes(engine.TopicApprovalEvents)) != 2 {
			t.Fatalf("approval events %+v", h.Pub.Envelopes(engine.TopicApprovalEvents))
		}
	})

	t.Run("reject", func(t *testing.T) {
		h, r, w := approvalSetup(t)
		a := pendingApproval(t, h, approver, r.ID)
		if _, err := h.Engine.DecideApproval(h.Ctx(), approver, a.ID, engine.DecisionDeny, "not now"); err != nil {
			t.Fatal(err)
		}
		h.RunToEnd(w, r.ID)
		got, _ := h.Engine.GetRun(h.Ctx(), requester, r.ID)
		if got.Status != engine.StatusFailed || got.FailureCode != "approval_denied" || len(h.Exec.CallsTo("post")) != 0 {
			t.Fatalf("run %s %s", got.Status, got.FailureCode)
		}
	})

	t.Run("service_account_requester_may_be_approved_by_anyone_with_the_role", func(t *testing.T) {
		h := enginetest.New(t)
		h.Exec.Handle("post", enginetest.Output("id-0"))
		h.Exec.Handle("block", enginetest.Echo())
		key := engine.Caller{Tenant: enginetest.TenantA, Actor: "apikey:k1", Roles: []string{"analyst", "finance_ap"}}
		h.Register(key, "saga", enginetest.Saga(enginetest.SagaOptions{Approval: true, Steps: 1}), core.Nil)
		r := h.Submit(key, "saga", core.Nil, map[string]any{"amount": 5}, "k")
		w := h.Engine.NewWorker("w")
		h.Drain(w)
		a := pendingApproval(t, h, key, r.ID)
		if _, err := h.Engine.DecideApproval(h.Ctx(), key, a.ID, engine.DecisionApprove, ""); err != nil {
			t.Fatal(err)
		}
		if st := h.RunToEnd(w, r.ID); st != engine.StatusSucceeded {
			t.Fatalf("status %s", st)
		}
	})

	t.Run("expiry", func(t *testing.T) {
		h, r, w := approvalSetup(t)
		a := pendingApproval(t, h, approver, r.ID)
		h.Clock.Advance(time.Hour)
		_, err := h.Engine.DecideApproval(h.Ctx(), approver, a.ID, engine.DecisionApprove, "")
		wantReason(t, err, core.KindAborted, core.ReasonApprovalAlreadyDecided)
		h.RunToEnd(w, r.ID)
		got, _ := h.Engine.GetRun(h.Ctx(), requester, r.ID)
		if got.Status != engine.StatusFailed || got.FailureCode != "approval_denied" {
			t.Fatalf("run %s %s", got.Status, got.FailureCode)
		}
		ea, _ := h.Engine.GetApproval(h.Ctx(), approver, a.ID)
		if ea.Status != engine.ApprovalExpired || countType(h.Events(r.ID), engine.EvApprovalExpired, "") != 1 {
			t.Fatalf("approval %s", ea.Status)
		}
	})

	t.Run("escalation", func(t *testing.T) {
		h, r, w := approvalSetup(t, func(h *enginetest.Harness) {
			h.Config.EscalationRoles = map[string]string{"finance_ap": "cfo"}
			h.Config.MaxEscalations = 1
		})
		first := pendingApproval(t, h, approver, r.ID)
		if first.EscalateRole != "cfo" {
			t.Fatalf("escalate role %q", first.EscalateRole)
		}
		h.Clock.Advance(time.Hour)
		h.Drain(w)
		cfo := engine.Caller{Tenant: enginetest.TenantA, Actor: "user:cfo", Roles: []string{"cfo"}}
		second := pendingApproval(t, h, cfo, r.ID)
		if second.Role != "cfo" || second.ID == first.ID || second.EffectPath != "0#1" {
			t.Fatalf("escalated approval %+v", second)
		}
		if _, err := h.Engine.DecideApproval(h.Ctx(), cfo, second.ID, engine.DecisionApprove, ""); err != nil {
			t.Fatal(err)
		}
		if st := h.RunToEnd(w, r.ID); st != engine.StatusSucceeded {
			t.Fatalf("status %s", st)
		}
	})
}
