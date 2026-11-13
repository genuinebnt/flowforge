package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/eventstore"
	"github.com/genuinebnt/flowforge/vm"
)

// SubmitRunRequest is proto SubmitRunRequest plus the idempotency key the
// gateway takes from the Idempotency-Key header.
type SubmitRunRequest struct {
	WorkflowName    string
	WorkflowVersion int32 // 0: latest published
	ProgramID       core.UUID
	Input           json.RawMessage // a JSON object; nil means {}
	Priority        int16
	Deadline        time.Duration // 0: none
	IdempotencyKey  string
}

// requestHash is the SHA-256 of the canonical request (spec 9.10).
func requestHash(req SubmitRunRequest, input json.RawMessage) ([]byte, error) {
	h, err := core.HashJSON(map[string]any{
		"workflow": req.WorkflowName, "version": req.WorkflowVersion, "program_id": req.ProgramID.String(),
		"input": input, "priority": req.Priority, "deadline_ms": req.Deadline.Milliseconds(),
	})
	if err != nil {
		return nil, err
	}
	return h[:], nil
}

// SubmitRun creates a run (spec 3.3, 9.10): it requires an idempotency
// key, returns the original run for a repeated request with the same
// hash and fails with ALREADY_EXISTS/IDEMPOTENCY_KEY_REUSED for a
// different one, rejects deprecated versions and versions whose class or
// tools the program's current scope document forbids (max_class,
// activity.prohibited), pins that scope version and records
// run_submitted.
func (e *Engine) SubmitRun(ctx context.Context, c Caller, req SubmitRunRequest) (Run, error) {
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" || len(key) > 255 {
		return Run{}, errInvalid("an idempotency key (1-255 characters) is required")
	}
	input := req.Input
	if len(bytes.TrimSpace(input)) == 0 {
		input = json.RawMessage("{}")
	}
	canon, err := core.CanonicalJSON(input)
	if err != nil {
		return Run{}, errInvalid("input is not valid JSON: %v", err)
	}
	input = canon
	hash, err := requestHash(req, input)
	if err != nil {
		return Run{}, errInternal("hash request", err)
	}
	if r, found, err := e.existingRun(ctx, c, key, hash); err != nil || found {
		return r, err
	}
	if req.Deadline < 0 {
		return Run{}, errInvalid("deadline must not be negative")
	}

	v, err := e.d.Repo.GetWorkflowVersion(ctx, c.Tenant, req.WorkflowName, req.WorkflowVersion)
	if err != nil {
		return Run{}, notFoundOr("workflow version", "get workflow version", err)
	}
	if v.Status == VersionDeprecated {
		return Run{}, errPrecondition(core.ReasonWorkflowDeprecated, "%s version %d is deprecated", v.Name, v.Version)
	}
	lm, err := e.loadModule(ctx, v.Module)
	if err != nil {
		return Run{}, errPrecondition(core.ReasonValidationFailed, "workflow version cannot be loaded: %v", err)
	}
	if _, err := vm.DecodeInputs(lm.prog, input); err != nil {
		return Run{}, errInvalid("input: %v", err)
	}

	var scopeVersion int32
	if !req.ProgramID.IsZero() || v.TargetFacing() {
		if req.ProgramID.IsZero() {
			return Run{}, errInvalid("workflow %s acts on targets and needs a program", v.Name)
		}
		pol, err := e.programPolicy(ctx, c.Tenant, req.ProgramID)
		if err != nil {
			return Run{}, err
		}
		if v.InferredClass > pol.MaxClass {
			return Run{}, errDenied(core.ReasonScopeDenied, "workflow class %s exceeds the program's max_class %s", v.InferredClass, pol.MaxClass)
		}
		// The version may have been registered against an older scope
		// document; check the tools it pins against the current one.
		match := func(a ...string) (string, bool) { return core.ProhibitedActivity(pol.Prohibited, a...) }
		if tool, entry, bad := prohibitedTool(match, lm.tools); bad {
			return Run{}, errDenied(core.ReasonScopeDenied, "workflow calls tool %s, which the program prohibits (activity.prohibited %q)", tool, entry)
		}
		scopeVersion = pol.ScopeVersion
	}
	ks, err := e.d.Scope.KillSwitch(ctx, c.Tenant, req.ProgramID)
	if err != nil {
		return Run{}, apiErr(core.KindUnavailable, core.ReasonUnavailable, "kill switch state unavailable: %v", err)
	}
	if ks.Engaged && v.TargetFacing() {
		return Run{}, errDenied(core.ReasonKillSwitchActive, "%s kill switch is engaged", ks.Level)
	}

	now := msTime(e.now())
	var deadline time.Time
	if req.Deadline > 0 {
		deadline = now.Add(req.Deadline)
	}
	if t := v.Attributes.Timeout; t > 0 && (deadline.IsZero() || now.Add(t).Before(deadline)) {
		deadline = now.Add(t)
	}
	id := e.ids.New()
	rec := RunRecord{
		ID: id, Tenant: c.Tenant, ProgramID: req.ProgramID, WorkflowVersionID: v.ID,
		WorkflowName: v.Name, WorkflowVersion: v.Version, ScopeVersion: scopeVersion,
		Status: StatusPending, CompensationOutcome: CompensationNone, Input: input,
		CreatedBy: c.Actor, IdempotencyKey: key, RequestHash: hash, Priority: req.Priority,
		DeadlineAt: deadline, CreatedAt: now, LastSeq: 1,
	}
	p := RunSubmittedPayload{
		WorkflowVersionID: v.ID.String(), WorkflowName: v.Name, WorkflowVersion: v.Version,
		ScopeVersion: scopeVersion, Input: input, IdempotencyKey: key, RequestHash: hexHash([32]byte(hash)),
		SubmittedBy: c.Actor, Priority: req.Priority, DeadlineAt: fmtTime(deadline),
	}
	if !req.ProgramID.IsZero() {
		p.ProgramID = req.ProgramID.String()
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return Run{}, errInternal("encode run_submitted", err)
	}
	spec := eventstore.RunSpec{
		ID: id, Tenant: c.Tenant, Program: req.ProgramID, WorkflowVersion: v.ID, ScopeVersion: scopeVersion,
		CreatedBy: c.Actor, IdempotencyKey: key, RequestHash: hash, Priority: req.Priority,
		Input: input, Deadline: deadline,
	}
	ev := eventstore.Event{Type: EvRunSubmitted, At: now, Actor: c.Actor, Payload: payload}
	if err := e.d.Store.CreateRun(ctx, spec, []eventstore.Event{ev}); err != nil {
		if errors.Is(err, eventstore.ErrExists) {
			if r, found, err := e.existingRun(ctx, c, key, hash); err != nil || found {
				return r, err
			}
		}
		return Run{}, errInternal("create run", err)
	}
	if err := e.d.Repo.InsertRun(ctx, rec); err != nil && !errors.Is(err, ErrConflict) {
		return Run{}, errInternal("store run", err)
	}
	e.hub.notifyWorkers()
	return Run{RunRecord: rec}, nil
}

// existingRun implements the idempotent repeat of spec 9.10.
func (e *Engine) existingRun(ctx context.Context, c Caller, key string, hash []byte) (Run, bool, error) {
	r, err := e.d.Repo.FindRunByIdempotencyKey(ctx, c.Tenant, key)
	if errors.Is(err, ErrNotFound) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, errInternal("find run by idempotency key", err)
	}
	if !bytes.Equal(r.RequestHash, hash) {
		return Run{}, true, apiErr(core.KindAlreadyExists, core.ReasonIdempotencyKeyReused,
			"idempotency key %q was used for a different request", key)
	}
	run, err := e.GetRun(ctx, c, r.ID)
	return run, true, err
}
