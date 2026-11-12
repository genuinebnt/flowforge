package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/vm"
)

// defaultApprovalTimeout applies when an approval effect's timeout is not
// positive.
const defaultApprovalTimeout = 24 * time.Hour

// doApproval performs approve(role, reason, timeout) (spec 9.5, 9.7): it
// creates the approval row, records approval_requested and waits until a
// decision or the expiry. A decision completes the effect with
// Ok(Approval) or Err(Denied); an expiry escalates to the configured role
// or completes with Err(Denied) (reason "expired"), which `require`
// turns into approval_denied.
func (r *runner) doApproval(s vm.Suspended, e *effectState) (*stepResult, error) {
	if len(s.Args) != 3 {
		return nil, r.failEffect(e, ActorEngine, EffectError{Code: "invalid_arguments", Message: "approval takes role, reason, timeout"}, bytecode.FailTypeMismatch)
	}
	role, reason, timeout := s.Args[0].Str(), s.Args[1].Str(), time.Duration(s.Args[2].AsInt())
	if timeout <= 0 {
		timeout = defaultApprovalTimeout
	}
	a := r.log.openApproval(e)
	if a == nil {
		return r.requestApproval(e, role, reason, timeout, 0)
	}
	row, err := r.approvalRow(e, a)
	if err != nil {
		return nil, err
	}
	switch row.Status {
	case ApprovalPending:
		if r.now().Before(a.expiresAt) {
			return waitUntil(a.expiresAt, "approval"), nil
		}
		got, changed, err := r.e.d.Repo.DecideApproval(r.ctx, core.Nil, row.ID, ApprovalExpired, ActorEngine, "", r.now())
		if err != nil {
			return nil, err
		}
		if !changed && got.Status != ApprovalExpired {
			return r.approvalDecided(e, a, got) // decided just before the expiry was recorded
		}
		return r.approvalExpired(e, a, reason, timeout)
	case ApprovalApproved, ApprovalDenied:
		return r.approvalDecided(e, a, row)
	default: // expired or cancelled
		return r.approvalExpired(e, a, reason, timeout)
	}
}

func (r *runner) approvalID(path string, round int) core.UUID {
	return derivedID("approval", r.run.String(), path, fmt.Sprint(round))
}

func approvalPath(path string, round int) string {
	if round == 0 {
		return path
	}
	return fmt.Sprintf("%s#%d", path, round)
}

// approvalRow returns the approval's row, re-creating it if an earlier
// owner crashed between the log and the row (never in practice: the row
// is written first).
func (r *runner) approvalRow(e *effectState, a *approvalState) (Approval, error) {
	id, err := core.ParseUUID(a.id)
	if err != nil {
		return Approval{}, fmt.Errorf("engine: approval id %q: %w", a.id, err)
	}
	row, err := r.e.d.Repo.GetApproval(r.ctx, core.Nil, id)
	if errors.Is(err, ErrNotFound) {
		return r.e.d.Repo.UpsertApproval(r.ctx, Approval{
			ID: id, Tenant: r.rec.Tenant, RunID: r.run, EffectPath: approvalPath(e.path, a.round), Role: a.role,
			Reason: a.reason, Proposed: json.RawMessage("{}"), Status: ApprovalPending, ExpiresAt: a.expiresAt,
			EscalateRole: r.e.cfg.EscalationRoles[a.role], CreatedAt: r.now(),
		})
	}
	return row, err
}

func (r *runner) requestApproval(e *effectState, role, reason string, timeout time.Duration, round int) (*stepResult, error) {
	id := r.approvalID(e.path, round)
	row, err := r.e.d.Repo.UpsertApproval(r.ctx, Approval{
		ID: id, Tenant: r.rec.Tenant, RunID: r.run, EffectPath: approvalPath(e.path, round), Role: role,
		Reason: reason, Proposed: json.RawMessage("{}"), Status: ApprovalPending,
		ExpiresAt: msTime(r.now().Add(timeout)), EscalateRole: r.e.cfg.EscalationRoles[role], CreatedAt: r.now(),
	})
	if err != nil {
		return nil, err
	}
	expires := msTime(row.ExpiresAt)
	if err := r.append(ev(EvApprovalRequested, ActorEngine, ApprovalRequestedPayload{
		Path: e.path, ApprovalID: id.String(), Role: row.Role, Reason: row.Reason,
		Proposed: json.RawMessage("{}"), ExpiresAt: fmtTime(expires), Round: round,
	})); err != nil {
		return nil, err
	}
	if row.Status != ApprovalPending {
		// Decided while an earlier owner was down: apply it on the next pass.
		return waitUntil(r.now(), "approval"), nil
	}
	return waitUntil(expires, "approval"), nil
}

func (r *runner) approvalDecided(e *effectState, a *approvalState, row Approval) (*stepResult, error) {
	decision := ApprovalApproved
	var res vm.Value
	if row.Status == ApprovalApproved {
		res = vm.Ok(builtinRecord(bytecode.TypeApproval, map[string]vm.Value{
			"id": vm.Str(a.id), "approval_id": vm.Str(a.id), "role": vm.Str(a.role),
			"decided_by": vm.Str(row.DecidedBy), "actor": vm.Str(row.DecidedBy),
			"decided_at": vm.Time(msTime(row.DecidedAt).UnixNano()), "comment": vm.Str(row.Comment),
		}))
	} else {
		decision = ApprovalDenied
		why := row.Comment
		if why == "" {
			why = "denied by " + row.DecidedBy
		}
		res = vm.Err(builtinRecord(bytecode.TypeDenied, map[string]vm.Value{
			"id": vm.Str(a.id), "approval_id": vm.Str(a.id), "role": vm.Str(a.role),
			"actor": vm.Str(row.DecidedBy), "reason": vm.Str(why),
		}))
	}
	return nil, r.completeEffect(e, ActorEngine, res, ev(EvApprovalDecided, row.DecidedBy, ApprovalDecidedPayload{
		ApprovalID: a.id, Decision: decision, Actor: row.DecidedBy, Comment: row.Comment,
	}))
}

func (r *runner) approvalExpired(e *effectState, a *approvalState, reason string, timeout time.Duration) (*stepResult, error) {
	expired := ev(EvApprovalExpired, ActorEngine, ApprovalExpiredPayload{ApprovalID: a.id})
	if esc := r.e.cfg.EscalationRoles[a.role]; esc != "" && a.round < r.e.cfg.MaxEscalations {
		if err := r.append(expired); err != nil {
			return nil, err
		}
		return r.requestApproval(e, esc, reason, timeout, a.round+1)
	}
	res := vm.Err(builtinRecord(bytecode.TypeDenied, map[string]vm.Value{
		"id": vm.Str(a.id), "approval_id": vm.Str(a.id), "role": vm.Str(a.role),
		"actor": vm.Str(ActorEngine), "reason": vm.Str("expired"),
	}))
	return nil, r.completeEffect(e, ActorEngine, res, expired)
}
