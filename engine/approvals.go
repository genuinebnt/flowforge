package engine

import (
	"context"
	"strings"

	"github.com/genuinebnt/flowforge/core"
)

// Approval decisions (proto ApprovalDecision).
const (
	DecisionApprove = "approve"
	DecisionDeny    = "deny"
)

type approvalToken struct {
	ID string `json:"id"`
}

// ListApprovals lists the tenant's approvals in creation order.
func (e *Engine) ListApprovals(ctx context.Context, c Caller, f ApprovalFilter, p Page) ([]Approval, string, error) {
	var tok approvalToken
	if err := decodeToken(p.Token, &tok); err != nil {
		return nil, "", err
	}
	var after core.UUID
	if tok.ID != "" {
		id, err := core.ParseUUID(tok.ID)
		if err != nil {
			return nil, "", errInvalid("invalid page token")
		}
		after = id
	}
	size := pageSize(p.Size)
	as, err := e.d.Repo.ListApprovals(ctx, c.Tenant, f, after, size+1)
	if err != nil {
		return nil, "", errInternal("list approvals", err)
	}
	next := ""
	if len(as) > size {
		as = as[:size]
		next = encodeToken(approvalToken{ID: as[size-1].ID.String()})
	}
	return as, next, nil
}

// GetApproval returns an approval.
func (e *Engine) GetApproval(ctx context.Context, c Caller, id core.UUID) (Approval, error) {
	a, err := e.d.Repo.GetApproval(ctx, c.Tenant, id)
	if err != nil {
		return Approval{}, notFoundOr("approval", "get approval", err)
	}
	return a, nil
}

// DecideApproval approves or denies a pending approval (spec 9.7). The
// caller must hold the approval's role (the approvals:decide permission is
// checked by the gateway). A user may not decide an approval of a run they
// submitted (SELF_APPROVAL_FORBIDDEN); runs submitted by schedules or API
// keys have no such constraint. Decisions are idempotent per (approval,
// actor): repeating one's own decision returns the approval unchanged; any
// other attempt on a decided approval fails with ABORTED
// APPROVAL_ALREADY_DECIDED. The run is woken and its worker records
// approval_decided.
func (e *Engine) DecideApproval(ctx context.Context, c Caller, id core.UUID, decision, comment string) (Approval, error) {
	var status string
	switch decision {
	case DecisionApprove:
		status = ApprovalApproved
	case DecisionDeny:
		status = ApprovalDenied
	default:
		return Approval{}, errInvalid("decision must be approve or deny")
	}
	if strings.TrimSpace(c.Actor) == "" {
		return Approval{}, errInvalid("the deciding actor is required")
	}
	a, err := e.d.Repo.GetApproval(ctx, c.Tenant, id)
	if err != nil {
		return Approval{}, notFoundOr("approval", "get approval", err)
	}
	if !c.HasRole(a.Role) {
		return Approval{}, errDenied(core.ReasonForbiddenRole, "deciding this approval requires role %q", a.Role)
	}
	run, err := e.d.Repo.GetRun(ctx, c.Tenant, a.RunID)
	if err != nil {
		return Approval{}, notFoundOr("run", "get run", err)
	}
	if c.IsUser() && run.CreatedBy == c.Actor {
		return Approval{}, errDenied(core.ReasonSelfApprovalForbidden, "the requester of a run cannot approve it")
	}
	if a.Status != ApprovalPending {
		return decidedAlready(a, c.Actor, status)
	}
	now := e.now()
	if !now.Before(a.ExpiresAt) {
		return Approval{}, apiErr(core.KindAborted, core.ReasonApprovalAlreadyDecided, "approval expired at %s", core.FormatTime(a.ExpiresAt))
	}
	got, changed, err := e.d.Repo.DecideApproval(ctx, c.Tenant, id, status, c.Actor, comment, now)
	if err != nil {
		return Approval{}, notFoundOr("approval", "decide approval", err)
	}
	if !changed {
		return decidedAlready(got, c.Actor, status)
	}
	if err := e.wake(ctx, a.RunID); err != nil {
		return Approval{}, errInternal("wake run", err)
	}
	return got, nil
}

func decidedAlready(a Approval, actor, status string) (Approval, error) {
	if a.DecidedBy == actor && a.Status == status {
		return a, nil
	}
	return Approval{}, apiErr(core.KindAborted, core.ReasonApprovalAlreadyDecided, "approval is already %s", a.Status)
}
