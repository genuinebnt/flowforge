package engine

import (
	"context"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/eventstore"
	"github.com/genuinebnt/flowforge/vm"
)

// failureFlow ends a failed run (spec 9.6): with registered compensations
// it records compensation_started, runs each closure newest first as an
// ordinary recorded effect sequence, records compensation_step per
// closure and compensation_finished, then run_failed. A compensation that
// fails permanently is recorded and the remaining ones still run.
func (r *runner) failureFlow(code, msg string, extra []pending) (eventstore.Next, error) {
	comps := r.root.vm.Compensations()
	if len(comps) == 0 && r.log.comp == nil {
		if err := r.append(append(extra, ev(EvRunFailed, ActorEngine, RunFailedPayload{Code: code, Message: msg}))...); err != nil {
			return eventstore.Next{}, err
		}
		return eventstore.Next{Status: StatusFailed}, nil
	}
	st, err := r.compensate(comps, CauseFailure, code, msg, extra)
	if err != nil || st != nil {
		return r.parkOr(st, err)
	}
	if c := r.log.comp; c != nil && c.started.Code != "" {
		code, msg = c.started.Code, c.started.Message
	}
	if err := r.append(ev(EvRunFailed, ActorEngine, RunFailedPayload{Code: code, Message: msg})); err != nil {
		return eventstore.Next{}, err
	}
	return eventstore.Next{Status: StatusFailed}, nil
}

// timeoutFlow ends a run whose deadline passed (spec 9.8): open effects
// fail with timeout, then the failure flow runs with code timeout.
func (r *runner) timeoutFlow() (eventstore.Next, error) {
	var evs []pending
	if r.log.status == StatusPaused {
		evs = append(evs, ev(EvRunResumed, ActorEngine, ReasonPayload{Reason: "deadline"}))
	}
	evs = append(evs, r.closeOpenUnder("", "timeout", "run deadline exceeded", bytecode.FailTimeout)...)
	if err := r.append(evs...); err != nil {
		return eventstore.Next{}, err
	}
	r.cancelApprovals()
	return r.failureFlow(bytecode.FailTimeout.String(), "run deadline exceeded", nil)
}

// cancelFlow cancels a run (spec 9.8): run_cancel_requested, open effects
// fail with cancelled (in-flight executions were already cancelled
// through their context), compensations run, then run_cancelled. root is
// nil for a run that never started.
func (r *runner) cancelFlow(root *task) (eventstore.Next, error) {
	actor, reason := r.cancelSource()
	var evs []pending
	if r.log.cancel == nil {
		evs = append(evs, ev(EvRunCancelRequested, actor, CancelPayload{Actor: actor, Reason: reason}))
	}
	if err := r.append(evs...); err != nil {
		return eventstore.Next{}, err
	}
	r.markApplied(RequestCancel)
	if err := r.append(r.closeOpenUnder("", "cancelled", "run cancelled", bytecode.FailCancelled)...); err != nil {
		return eventstore.Next{}, err
	}
	r.cancelApprovals()
	if root != nil {
		if comps := root.vm.Compensations(); len(comps) > 0 || r.log.comp != nil {
			st, err := r.compensate(comps, CauseCancel, bytecode.FailCancelled.String(), reason, nil)
			if err != nil || st != nil {
				return r.parkOr(st, err)
			}
		}
	}
	c := r.log.cancel
	if err := r.append(ev(EvRunCancelled, c.Actor, CancelPayload{Actor: c.Actor, Reason: c.Reason})); err != nil {
		return eventstore.Next{}, err
	}
	return eventstore.Next{Status: StatusCancelled}, nil
}

// cancelSource names who cancels: the log, a queued request or a kill
// switch.
func (r *runner) cancelSource() (actor, reason string) {
	switch {
	case r.log.cancel != nil:
		return r.log.cancel.Actor, r.log.cancel.Reason
	case r.cancelRequest() != nil:
		q := r.cancelRequest()
		return q.Actor, q.Reason
	case r.killReason != "":
		return ActorEngine, r.killReason
	}
	return ActorEngine, "cancelled"
}

// cancelApprovals closes the run's pending approvals.
func (r *runner) cancelApprovals() {
	for _, a := range r.log.approvals {
		if a.decided || a.expired {
			continue
		}
		if id, err := core.ParseUUID(a.id); err == nil {
			_, _, _ = r.e.d.Repo.DecideApproval(context.WithoutCancel(r.ctx), core.Nil, id, ApprovalCancelled, ActorEngine, "run ended", r.now())
		}
	}
}

func (r *runner) parkOr(st *stepResult, err error) (eventstore.Next, error) {
	if err != nil {
		return eventstore.Next{}, err
	}
	return r.park(*st), nil
}

// compensate runs the compensation stack (newest first). It returns a
// wait when a compensation effect must wait (its retry backoff, say); the
// next advance continues with the first unrecorded step.
func (r *runner) compensate(comps []vm.Value, cause, code, msg string, extra []pending) (*stepResult, error) {
	r.compensating.Store(true)
	r.interrupted.Store("")
	if r.log.comp == nil {
		evs := append(extra, ev(EvCompensationStarted, ActorEngine, CompensationStartedPayload{
			Count: len(comps), Cause: cause, Code: code, Message: msg, PathPrefix: uint32(r.root.vm.NextSeq()),
		}))
		if err := r.append(evs...); err != nil {
			return nil, err
		}
	}
	c := r.log.comp
	count := min(c.started.Count, len(comps))
	for k := len(c.steps); k < count; k++ {
		step := CompensationStepPayload{Index: k, Name: r.mod.FuncName(int(comps[k].FuncIndex())), Result: "ok"}
		child, err := vm.NewChild(r.lm.prog, comps[k], nil, vm.Path{c.started.PathPrefix, uint32(k)}, r.vmcfg)
		if err != nil {
			step.Result, step.Error = "failed", &EffectError{Code: "invalid_compensation", Message: err.Error()}
		} else {
			t := &task{vm: child, out: child.Outcome()}
			st, err := r.runTasks([]*task{t}, 1, false, phaseComp)
			if err != nil {
				return nil, err
			}
			switch st.kind {
			case stepWait:
				return &st, nil
			case stepHalt:
				step.Result, step.Error = "failed", &EffectError{Code: "nondeterminism_detected", Message: st.msg}
			default:
				if f, ok := t.out.(vm.Failed); ok {
					step.Result, step.Error = "failed", &EffectError{Code: f.Code.String(), Message: f.Message}
				}
			}
		}
		if err := r.append(ev(EvCompensationStep, ActorEngine, step)); err != nil {
			return nil, err
		}
	}
	if !c.finished {
		outcome := CompensationCompleted
		for _, s := range c.steps {
			if s.Result != "ok" {
				outcome = CompensationFailed
			}
		}
		if count < c.started.Count {
			outcome = CompensationFailed
		}
		if err := r.append(ev(EvCompensationFinished, ActorEngine, CompensationFinishedPayload{Outcome: outcome})); err != nil {
			return nil, err
		}
	}
	return nil, nil
}
