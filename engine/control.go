package engine

import (
	"context"
	"errors"

	"github.com/genuinebnt/flowforge/core"
)

// HandleScopeChanged reacts to a new scope version of a program
// (scope.events): every non-terminal run of the program is paused at its
// next effect boundary and re-validated against newVersion. A run whose
// allowed targets and activity class still pass continues pinned to the
// new version (scope_changed action continue); otherwise it stays paused
// (scope_changed action pause, run_paused) until a later version passes
// or it is cancelled. Paused runs are re-validated too.
func (e *Engine) HandleScopeChanged(ctx context.Context, tenant, program core.UUID, newVersion int32) error {
	if program.IsZero() || newVersion <= 0 {
		return errInvalid("program and new version are required")
	}
	runs, err := e.d.Repo.ListActiveRuns(ctx, tenant, program)
	if err != nil {
		return errInternal("list active runs", err)
	}
	var errs []error
	for _, r := range runs {
		if r.ScopeVersion >= newVersion {
			continue
		}
		if _, err := e.d.Repo.AddRunRequest(ctx, RunRequest{
			RunID: r.ID, Kind: RequestScopeChanged, Actor: ActorEngine, NewVersion: newVersion, CreatedAt: e.now(),
		}); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := e.reschedule(ctx, r.ID); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return errInternal("scope change", err)
	}
	return nil
}

// Kill switch levels (spec 11.3 KillSwitch.level).
const (
	KillGlobal  = "global"
	KillTenant  = "tenant"
	KillProgram = "program"
)

// HandleKillSwitch reacts to an engaged kill switch: every non-terminal run
// in its reach (all runs, a tenant's or a program's) is cancelled. Workers
// also check the kill switch before every tool call, so no new tool call is
// issued even before the cancellation is applied (eng_006).
func (e *Engine) HandleKillSwitch(ctx context.Context, level string, target core.UUID) error {
	var tenant, program core.UUID
	switch level {
	case KillGlobal:
	case KillTenant:
		tenant = target
	case KillProgram:
		program = target
	default:
		return errInvalid("unknown kill switch level %q", level)
	}
	runs, err := e.d.Repo.ListActiveRuns(ctx, tenant, program)
	if err != nil {
		return errInternal("list active runs", err)
	}
	var errs []error
	for _, r := range runs {
		if !program.IsZero() && r.ProgramID != program {
			continue
		}
		sys := Caller{Tenant: r.Tenant, Actor: ActorEngine}
		if _, err := e.requestCancel(ctx, sys, r.ID, ActorEngine, "kill_switch:"+level); err != nil {
			var ce *core.Error
			if errors.As(err, &ce) && ce.Reason == core.ReasonRunNotCancellable {
				continue
			}
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return errInternal("kill switch", err)
	}
	return nil
}
