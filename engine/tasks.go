package engine

import (
	"fmt"
	"time"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/vm"
)

// task is one VM the runner drives: the workflow body, a par_map child or
// a compensation.
type task struct {
	vm       *vm.VM
	out      vm.Outcome
	finished bool
}

func (t *task) failed() bool {
	_, ok := t.out.(vm.Failed)
	return ok
}

type stepKind int

const (
	stepFinished stepKind = iota // the VM completed or failed
	stepExec                     // a tool execution is prepared
	stepWait                     // the VM waits for time or for someone
	stepHalt                     // the run must stop the body (cancel, timeout, pause, nondeterminism)
)

type haltReason int

const (
	haltCancel haltReason = iota + 1
	haltTimeout
	haltPause
	haltNondeterminism
)

type stepResult struct {
	kind  stepKind
	until time.Time // stepWait; zero: until someone acts (operator, approval without expiry)
	why   string    // stepWait: approval, timer, retry, rate_limit, operator
	halt  haltReason
	msg   string
	exec  *execution
}

func waitUntil(t time.Time, why string) *stepResult {
	return &stepResult{kind: stepWait, until: t, why: why}
}

func halt(h haltReason, msg string) *stepResult {
	return &stepResult{kind: stepHalt, halt: h, msg: msg}
}

type phase int

const (
	phaseBody phase = iota // the workflow body: cancellation, deadline and scope changes apply
	phaseComp              // compensations: they always run to the end
)

// runTasks drives tasks (the workflow body alone, or the children of a
// par_map) until all finish, the run must wait, or it must halt. At most
// limit tasks are active at once; they are admitted in input order. In
// every round each active task is driven to its next tool execution or
// wait; the executions of a round run concurrently and their results are
// appended in task order in one atomic batch, so the log does not depend
// on timing and a crash never splits a round (inv_05, eng_008). Without
// collect, the first failed task ends the round and the caller fails the
// par_map.
func (r *runner) runTasks(ts []*task, limit int, collect bool, ph phase) (stepResult, error) {
	limit = max(limit, 1)
	admitted := min(limit, len(ts))
	for {
		var execs []*execution
		var wake time.Time
		indefinite := false
		progressed := false
		for i := 0; i < admitted; i++ {
			t := ts[i]
			if t.finished {
				continue
			}
			st, err := r.step(t, ph)
			if err != nil {
				return stepResult{}, err
			}
			switch st.kind {
			case stepFinished:
				t.finished, progressed = true, true
				if !collect && t.failed() {
					return stepResult{kind: stepFinished}, nil
				}
			case stepExec:
				execs = append(execs, st.exec)
			case stepWait:
				if st.until.IsZero() {
					indefinite = true
				} else if wake.IsZero() || st.until.Before(wake) {
					wake = st.until
				}
			case stepHalt:
				return st, nil
			}
		}
		for admitted < len(ts) && unfinished(ts[:admitted]) < limit {
			admitted++
			progressed = true
		}
		if len(execs) > 0 {
			st, err := r.execBatch(execs, ph)
			if err != nil {
				return stepResult{}, err
			}
			if st != nil {
				return *st, nil
			}
			continue
		}
		if unfinished(ts) == 0 {
			return stepResult{kind: stepFinished}, nil
		}
		if !progressed {
			if wake.IsZero() && !indefinite {
				return stepResult{}, fmt.Errorf("engine: tasks stalled without a wake time")
			}
			return stepResult{kind: stepWait, until: wake}, nil
		}
	}
}

func unfinished(ts []*task) int {
	n := 0
	for _, t := range ts {
		if !t.finished {
			n++
		}
	}
	return n
}

// step drives one task until it finishes or needs the outside world.
func (r *runner) step(t *task, ph phase) (stepResult, error) {
	for {
		if err := r.ctx.Err(); err != nil {
			if r.fenced.Load() {
				return stepResult{}, ErrLeaseLost
			}
			return stepResult{}, err
		}
		var err error
		switch o := t.out.(type) {
		case vm.Completed, vm.Failed:
			return stepResult{kind: stepFinished}, nil
		case vm.OutOfFuel:
			t.out, err = t.vm.Run(r.e.cfg.FuelSlice)
		case vm.Suspended:
			res, st, derr := r.decide(t, o, ph)
			if derr != nil {
				return stepResult{}, derr
			}
			if st != nil {
				return *st, nil
			}
			t.out, err = t.vm.Resume(res, r.e.cfg.FuelSlice)
		default:
			return stepResult{}, fmt.Errorf("engine: unexpected VM outcome %T", t.out)
		}
		if err != nil {
			return stepResult{}, fmt.Errorf("engine: vm: %w", err)
		}
	}
}

// decide handles one suspension: a recorded result is replayed; otherwise
// the run's boundary conditions are checked and the effect is performed
// (or continued) according to its kind. It returns either the result to
// resume the VM with, or a step result.
func (r *runner) decide(t *task, s vm.Suspended, ph phase) (vm.EffectResult, *stepResult, error) {
	path := s.Path.String()
	hash, err := vm.ArgsHash(s.Args)
	if err != nil {
		return vm.FailResult(bytecode.FailTypeMismatch, "effect arguments cannot be encoded: "+err.Error()), nil, nil
	}
	hashHex := hexHash(hash)
	e := r.log.effect(path)
	if e != nil {
		if e.kind != s.Kind || e.argsHash != hashHex {
			return vm.EffectResult{}, halt(haltNondeterminism, fmt.Sprintf(
				"effect %s: the log has %s with args_hash %s, replay produced %s with %s",
				path, e.kind, short(e.argsHash), s.Kind, short(hashHex))), nil
		}
		if e.done {
			return r.recorded(e), nil, nil
		}
	}
	if ph == phaseBody {
		if st, err := r.boundary(s, path); st != nil || err != nil {
			return vm.EffectResult{}, st, err
		}
		if t == r.root && e == nil {
			if err := r.maybeSnapshot(); err != nil {
				return vm.EffectResult{}, nil, err
			}
		}
	}
	if e == nil {
		if err := r.schedule(s, path, hashHex); err != nil {
			return vm.EffectResult{}, nil, err
		}
		e = r.log.effect(path)
	}
	var st *stepResult
	switch s.Kind {
	case bytecode.EffectScopeCheck:
		err = r.doScopeCheck(s, e)
	case bytecode.EffectToolCall:
		st, err = r.doToolCall(s, e, ph)
	case bytecode.EffectApproval:
		st, err = r.doApproval(s, e)
	case bytecode.EffectSleep:
		st, err = r.doSleep(s, e)
	case bytecode.EffectNow:
		err = r.doNow(e)
	case bytecode.EffectLog:
		err = r.doLog(s, e)
	case bytecode.EffectEmitAlert:
		err = r.doAlert(s, e)
	case bytecode.EffectParMap:
		st, err = r.doParMap(s, e, ph)
	case bytecode.EffectEmbed, bytecode.EffectSimilarityQuery:
		err = r.doIndexEffect(s, e)
	default: // agent_call, execute_plan: registration rejects them (D1)
		err = r.failEffect(e, ActorEngine, EffectError{Code: "agent_disabled", Message: s.Kind.String() + ": agent effects not enabled"}, bytecode.FailEffectFailed)
	}
	if err != nil || st != nil {
		return vm.EffectResult{}, st, err
	}
	if !e.done {
		return vm.EffectResult{}, nil, fmt.Errorf("engine: effect %s (%s) neither finished nor waits", path, s.Kind)
	}
	return r.recorded(e), nil, nil
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// recorded returns the recorded result of a finished effect.
func (r *runner) recorded(e *effectState) vm.EffectResult {
	if e.failed {
		msg := e.failErr.Message
		if e.failErr.Code != "" {
			msg = e.failErr.Code + ": " + msg
		}
		return vm.FailResult(e.failCode, msg)
	}
	v, err := vm.DecodeValue(r.lm.prog, e.value)
	if err != nil {
		return vm.FailResult(bytecode.FailNondeterminismDetected, "recorded result of "+e.path+" cannot be decoded: "+err.Error())
	}
	return vm.OkResult(v)
}

// schedule appends effect_scheduled for a new effect.
func (r *runner) schedule(s vm.Suspended, path, hash string) error {
	p := EffectScheduledPayload{Path: path, Kind: s.Kind.String(), ArgsHash: hash, Args: displayArgs(r.mod, s.Args)}
	if s.Kind == bytecode.EffectToolCall {
		p.Tool = s.Tool()
		p.IdempotencyKey = IdempotencyKey(r.run, path)
	}
	return r.append(ev(EvEffectScheduled, ActorVM, p))
}

// boundary checks, before the body performs or continues an effect,
// whether the run must stop instead: cancellation, the deadline, a scope
// version change, a pause, or a kill switch (eng_006: once a kill switch
// is engaged no further effect starts and the run is cancelled).
func (r *runner) boundary(s vm.Suspended, path string) (*stepResult, error) {
	if r.cancelRequested() {
		return halt(haltCancel, ""), nil
	}
	if r.deadlinePassed() {
		return halt(haltTimeout, ""), nil
	}
	if v := r.pendingScopeChange(); v > 0 {
		paused, retry, err := r.revalidate(v, path)
		if err != nil {
			return nil, err
		}
		if retry {
			return waitUntil(r.now().Add(r.e.cfg.RateLimitErrorBackoff), "scope_unavailable"), nil
		}
		if paused {
			return halt(haltPause, ""), nil
		}
	}
	if r.log.paused {
		return halt(haltPause, ""), nil
	}
	{
		ks, err := r.e.d.Scope.KillSwitch(r.ctx, r.rec.Tenant, r.rec.ProgramID)
		if err != nil {
			// Fail closed: no tool call while the kill switch state is unknown.
			return waitUntil(r.now().Add(r.e.cfg.RateLimitErrorBackoff), "kill_switch_unknown"), nil
		}
		if ks.Engaged {
			r.killReason = "kill_switch:" + ks.Level
			return halt(haltCancel, ""), nil
		}
	}
	return nil, nil
}

// completeEffect records a successful result.
func (r *runner) completeEffect(e *effectState, actor string, v vm.Value, extra ...pending) error {
	val, disp, err := encodeResult(r.mod, v)
	if err != nil {
		return r.failEffect(e, actor, EffectError{Code: "invalid_result", Message: err.Error()}, bytecode.FailEffectFailed, extra...)
	}
	evs := append(extra, ev(EvEffectCompleted, actor, EffectCompletedPayload{Path: e.path, Result: disp, Value: val}))
	return r.append(evs...)
}

// failEffect records the terminal failure of an effect.
func (r *runner) failEffect(e *effectState, actor string, ee EffectError, code bytecode.FailCode, extra ...pending) error {
	evs := append(extra, ev(EvEffectFailed, actor, EffectFailedPayload{
		Path: e.path, Attempt: e.attempt, Error: ee, Final: true, FailCode: code.String(),
	}))
	return r.append(evs...)
}
