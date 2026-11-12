package engine

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"slices"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/eventstore"
	"github.com/genuinebnt/flowforge/vm"
)

// GetRun returns a run (proto GetRun plus waiting_on). The status and the
// other log-derived fields come from the event log, which wins over the
// stored projection (spec 9.2).
func (e *Engine) GetRun(ctx context.Context, c Caller, id core.UUID) (Run, error) {
	rec, err := e.d.Repo.GetRun(ctx, c.Tenant, id)
	if err != nil {
		return Run{}, notFoundOr("run", "get run", err)
	}
	l, info, err := e.readLog(ctx, id)
	if err != nil {
		return Run{}, err
	}
	l.project(&rec)
	run := Run{RunRecord: rec, WaitingOn: l.waitingOn()}
	if run.Status == StatusRunning && info.Status == StatusWaiting && info.LeaseOwner == "" {
		run.Status = StatusWaiting // parked without an event: rate-limit delay or child backoff
	}
	if !Terminal(run.Status) && run.Status != StatusCancelling && run.Status != StatusCompensating {
		if reqs, err := e.d.Repo.PendingRunRequests(ctx, id); err == nil &&
			slices.ContainsFunc(reqs, func(r RunRequest) bool { return r.Kind == RequestCancel }) {
			run.Status = StatusCancelling
		}
	}
	return run, nil
}

func (e *Engine) readLog(ctx context.Context, id core.UUID) (*runLog, eventstore.RunInfo, error) {
	info, err := e.d.Store.GetRun(ctx, id)
	if err != nil {
		if errors.Is(err, eventstore.ErrNotFound) {
			return nil, info, errNotFound("run")
		}
		return nil, info, errInternal("read run", err)
	}
	evs, err := e.d.Store.Read(ctx, id, 1, 0)
	if err != nil {
		return nil, info, errInternal("read run events", err)
	}
	l, err := foldLog(evs)
	if err != nil {
		return nil, info, errInternal("fold run events", err)
	}
	return l, info, nil
}

type runToken struct {
	CreatedAt time.Time `json:"c"`
	ID        string    `json:"i"`
}

// ListRuns lists runs newest first (proto ListRuns). Status, compensation
// outcome and output come from the projection.
func (e *Engine) ListRuns(ctx context.Context, c Caller, f RunFilter, p Page) ([]Run, string, error) {
	var tok runToken
	if err := decodeToken(p.Token, &tok); err != nil {
		return nil, "", err
	}
	var cur RunCursor
	if tok.ID != "" {
		id, err := core.ParseUUID(tok.ID)
		if err != nil {
			return nil, "", errInvalid("invalid page token")
		}
		cur = RunCursor{CreatedAt: tok.CreatedAt, ID: id}
	}
	if f.Status != "" && !f.Status.Valid() {
		return nil, "", errInvalid("unknown status %q", f.Status)
	}
	size := pageSize(p.Size)
	recs, err := e.d.Repo.ListRuns(ctx, c.Tenant, f, cur, size+1)
	if err != nil {
		return nil, "", errInternal("list runs", err)
	}
	next := ""
	if len(recs) > size {
		recs = recs[:size]
		last := recs[size-1]
		next = encodeToken(runToken{CreatedAt: last.CreatedAt, ID: last.ID.String()})
	}
	out := make([]Run, len(recs))
	for i, r := range recs {
		out[i] = Run{RunRecord: r}
	}
	return out, next, nil
}

// CancelRun requests cancellation (spec 9.8). The worker advancing the run
// appends run_cancel_requested, cancels in-flight effects, runs the
// compensations and appends run_cancelled.
func (e *Engine) CancelRun(ctx context.Context, c Caller, id core.UUID, reason string) (Run, error) {
	return e.requestCancel(ctx, c, id, c.Actor, reason)
}

func (e *Engine) requestCancel(ctx context.Context, c Caller, id core.UUID, actor, reason string) (Run, error) {
	run, err := e.GetRun(ctx, c, id)
	if err != nil {
		return Run{}, err
	}
	switch {
	case Terminal(run.Status), run.Status == StatusCompensating:
		return Run{}, errPrecondition(core.ReasonRunNotCancellable, "run is %s", run.Status)
	case run.Status == StatusCancelling:
		return run, nil
	}
	if _, err := e.d.Repo.AddRunRequest(ctx, RunRequest{
		RunID: id, Kind: RequestCancel, Actor: actor, Reason: reason, CreatedAt: e.now(),
	}); err != nil {
		return Run{}, errInternal("queue cancel request", err)
	}
	if err := e.reschedule(ctx, id); err != nil {
		return Run{}, err
	}
	return e.GetRun(ctx, c, id)
}

// reschedule makes a run due now; a paused run (unschedulable) is moved
// back to the ready set first so a worker applies the queued request.
func (e *Engine) reschedule(ctx context.Context, id core.UUID) error {
	info, err := e.d.Store.GetRun(ctx, id)
	if err != nil {
		return errInternal("read run", err)
	}
	if info.Status == StatusPaused {
		lease, err := e.d.Store.AcquireLease(ctx, id, "engine-control", e.cfg.LeaseTTL)
		if err == nil {
			err = e.d.Store.ReleaseLease(ctx, lease, eventstore.Next{Status: StatusRunning})
		}
		if err != nil && !errors.Is(err, eventstore.ErrLeaseHeld) {
			return errInternal("unpause run", err)
		}
	}
	if err := e.wake(ctx, id); err != nil {
		return errInternal("wake run", err)
	}
	return nil
}

type seqToken struct {
	Seq uint64 `json:"seq"`
}

// ListRunEvents returns a page of a run's events after afterSeq,
// optionally filtered by type (proto ListRunEvents).
func (e *Engine) ListRunEvents(ctx context.Context, c Caller, id core.UUID, afterSeq uint64, types []string, p Page) ([]eventstore.Event, string, error) {
	if _, err := e.d.Repo.GetRun(ctx, c.Tenant, id); err != nil {
		return nil, "", notFoundOr("run", "get run", err)
	}
	var tok seqToken
	if err := decodeToken(p.Token, &tok); err != nil {
		return nil, "", err
	}
	from := max(afterSeq, tok.Seq) + 1
	size := pageSize(p.Size)
	var out []eventstore.Event
	for len(out) <= size {
		evs, err := e.d.Store.Read(ctx, id, from, 4*size)
		if err != nil {
			return nil, "", errInternal("read run events", err)
		}
		for _, ev := range evs {
			if len(types) == 0 || slices.Contains(types, ev.Type) {
				out = append(out, ev)
			}
		}
		if len(evs) < 4*size {
			break
		}
		from = evs[len(evs)-1].Seq + 1
	}
	next := ""
	if len(out) > size {
		out = out[:size]
		next = encodeToken(seqToken{Seq: out[size-1].Seq})
	}
	return out, next, nil
}

// WatchRun streams a run's events after afterSeq and then follows new
// ones; it is resumable by passing the last seen seq (proto WatchRun).
func (e *Engine) WatchRun(ctx context.Context, c Caller, id core.UUID, afterSeq uint64) (iter.Seq2[eventstore.Event, error], error) {
	if _, err := e.d.Repo.GetRun(ctx, c.Tenant, id); err != nil {
		return nil, notFoundOr("run", "get run", err)
	}
	return e.d.Store.Subscribe(ctx, id, afterSeq+1), nil
}

// ResolveUnknownEffect is the operator action for an effect whose outcome
// is unknown (spec 9.5): retry it, or record it as completed (with result,
// a JSON value of the tool's output type) or as failed.
func (e *Engine) ResolveUnknownEffect(ctx context.Context, c Caller, id core.UUID, path, resolution string, result json.RawMessage) (Run, error) {
	rec, err := e.d.Repo.GetRun(ctx, c.Tenant, id)
	if err != nil {
		return Run{}, notFoundOr("run", "get run", err)
	}
	l, _, err := e.readLog(ctx, id)
	if err != nil {
		return Run{}, err
	}
	eff := l.effect(path)
	if eff == nil || eff.done || !eff.unknown {
		return Run{}, errPrecondition(core.ReasonValidationFailed, "effect %q has no unknown outcome", path)
	}
	switch resolution {
	case ResolveRetry, ResolveAssumeFailed:
	case ResolveAssumeCompleted:
		v, err := e.d.Repo.GetWorkflowVersionByID(ctx, rec.Tenant, rec.WorkflowVersionID)
		if err != nil {
			return Run{}, errInternal("get workflow version", err)
		}
		lm, err := e.loadModule(ctx, v.Module)
		if err != nil {
			return Run{}, errInternal("load module", err)
		}
		if _, err := decodeToolOutput(lm, eff.tool, result); err != nil {
			return Run{}, errInvalid("result: %v", err)
		}
	default:
		return Run{}, errInvalid("resolution must be retry, assume_completed or assume_failed")
	}
	if _, err := e.d.Repo.AddRunRequest(ctx, RunRequest{
		RunID: id, Kind: RequestResolve, Actor: c.Actor, Path: path, Resolution: resolution,
		Result: result, CreatedAt: e.now(),
	}); err != nil {
		return Run{}, errInternal("queue resolution", err)
	}
	if err := e.reschedule(ctx, id); err != nil {
		return Run{}, err
	}
	return e.GetRun(ctx, c, id)
}

// decodeToolOutput converts a tool's JSON output to a VM value of the
// tool's declared output type.
func decodeToolOutput(lm *loadedModule, tool string, out json.RawMessage) (vm.Value, error) {
	spec, ok := lm.tools[tool]
	if !ok {
		return vm.Value{}, errors.New("tool " + tool + " is not in the module")
	}
	m := lm.prog.Module()
	t, err := vm.ParseType(m, spec.OutputType)
	if err != nil {
		return vm.Value{}, err
	}
	if len(out) == 0 {
		out = json.RawMessage("null")
	}
	return vm.FromJSON(m, t, out)
}
