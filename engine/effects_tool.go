package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/core/netnorm"
	"github.com/genuinebnt/flowforge/limiter"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/vm"
)

// execution is a prepared tool call: effect_started is recorded and the
// call waits for its batch to run.
type execution struct {
	e            *effectState
	spec         ToolSpec
	policy       RetryPolicy
	call         ToolCall
	attempt      int
	uncounted    int // e.uncounted when the call was prepared
	siteDeadline time.Time
}

type execResult struct {
	i   int
	out ToolResult
	err error
	dur time.Duration
}

// doToolCall prepares a tool call (spec 9.5): resolve the spec, verify
// the InScope arguments against recorded allow decisions (inv_06),
// re-check the targets, acquire the rate limits, and record
// effect_started. It also handles the operator resolution of an unknown
// outcome and the re-execution of an ambiguous call after a crash.
func (r *runner) doToolCall(s vm.Suspended, e *effectState, ph phase) (*stepResult, error) {
	name := s.Tool()
	actor := ToolActor(name)
	spec, ok := r.lm.tools[name]
	if !ok {
		return nil, r.failEffect(e, actor, EffectError{Code: "unknown_tool", Message: "tool " + name + " is not registered"}, bytecode.FailEffectFailed)
	}
	site, _ := siteAnnotation(r.mod, s.Site)
	if ph == phaseComp && activityClass(spec.EffectClass) > core.ClassNone {
		ks, err := r.e.d.Scope.KillSwitch(r.ctx, r.rec.Tenant, r.rec.ProgramID)
		if err != nil {
			return waitUntil(r.now().Add(r.e.cfg.RateLimitErrorBackoff), "kill_switch_unknown"), nil
		}
		if ks.Engaged {
			return nil, r.failEffect(e, actor, EffectError{Code: "kill_switch_active", Message: ks.Level + " kill switch is engaged"}, bytecode.FailEffectFailed)
		}
	}

	var resolution *Resolution
	if e.unknown {
		q := r.resolution(e.path)
		if q == nil {
			return waitUntil(time.Time{}, "operator"), nil
		}
		r.applied[q.ID] = true
		resolution = &Resolution{Action: q.Resolution, Actor: q.Actor}
		switch q.Resolution {
		case ResolveAssumeCompleted:
			v, err := decodeToolOutput(r.lm, name, q.Result)
			if err != nil {
				return nil, r.failEffect(e, q.Actor, EffectError{Code: "invalid_result", Message: err.Error()}, bytecode.FailEffectFailed)
			}
			val, disp, err := encodeResult(r.mod, v)
			if err != nil {
				return nil, err
			}
			return nil, r.append(ev(EvEffectCompleted, q.Actor, EffectCompletedPayload{Path: e.path, Result: disp, Value: val, Resolution: resolution}))
		case ResolveAssumeFailed:
			return nil, r.append(ev(EvEffectFailed, q.Actor, EffectFailedPayload{
				Path: e.path, Attempt: e.attempt, Final: true, FailCode: bytecode.FailEffectFailed.String(), Resolution: resolution,
				Error: EffectError{Code: "assumed_failed", Message: "resolved by " + q.Actor},
			}))
		}
	}

	reexec := false
	if e.inFlight {
		ann := r.mod.Annotations
		if spec.ReexecSafe() || site.Idempotent || r.attrs.OnUnknownRetry || (ann != nil && ann.Idempotent) {
			reexec = true // same attempt, same idempotency key, no new effect_started
		} else {
			err := r.append(ev(EvEffectOutcomeUnknown, ActorEngine, EffectOutcomeUnknownPayload{Path: e.path, Attempt: e.attempt}))
			if err != nil {
				return nil, err
			}
			return waitUntil(time.Time{}, "operator"), nil
		}
	}
	now := r.now()
	if e.retryPending && now.Before(e.retryAt) {
		return waitUntil(e.retryAt, "retry"), nil
	}
	var siteDeadline time.Time
	if site.Timeout > 0 {
		siteDeadline = e.scheduledAt.Add(time.Duration(site.Timeout))
		if !now.Before(siteDeadline) {
			return nil, r.failEffect(e, actor, EffectError{Code: "timeout", Message: "the statement's #[timeout] expired"}, bytecode.FailTimeout)
		}
	}

	inputs := s.Args[1:]
	var proofs []vm.Value
	for _, a := range inputs {
		proofs = collectProofs(a, proofs, 0)
	}
	if msg := r.verifyProofs(spec, proofs); msg != "" {
		return nil, r.failEffect(e, ActorEngine, EffectError{Code: "scope_denied", Message: msg}, bytecode.FailScopeDenied)
	}
	if len(proofs) > 0 {
		d, err := r.recheck(spec, e, proofs)
		if err != nil {
			return nil, err
		}
		if d != nil {
			return nil, r.failEffect(e, ActorEngine, EffectError{Code: "scope_denied",
				Message: fmt.Sprintf("%s: %s", d.Target, d.Reason)}, bytecode.FailScopeDenied)
		}
	}
	if st, ee := r.rateLimit(spec, proofs); st != nil || ee != nil {
		if ee != nil {
			return nil, r.failEffect(e, ActorEngine, *ee, bytecode.FailBudgetExceeded)
		}
		return st, nil
	}
	input, err := toolInput(r.mod, inputs)
	if err != nil {
		return nil, r.failEffect(e, ActorEngine, EffectError{Code: "invalid_input", Message: err.Error()}, bytecode.FailTypeMismatch)
	}
	attempt := e.attempt
	if !reexec {
		attempt++
		if err := r.append(ev(EvEffectStarted, ActorEngine, EffectStartedPayload{
			Path: e.path, Attempt: attempt, Executor: r.e.cfg.Executor, Resolution: resolution,
		})); err != nil {
			return nil, err
		}
	}
	ids := make([]string, 0, len(proofs))
	for _, p := range proofs {
		ids = append(ids, p.ProofDecision())
	}
	ids = append(ids, e.decisions...)
	call := ToolCall{
		Tool: spec.Name, Version: spec.Version, Input: input, RunID: r.run, Path: e.path,
		IdempotencyKey: IdempotencyKey(r.run, e.path), Tenant: r.rec.Tenant, ProgramID: r.rec.ProgramID,
		ScopeVersion: r.log.scopeVersion, DecisionIDs: ids, Attempt: attempt,
		Timeout: time.Duration(spec.TimeoutMs) * time.Millisecond,
	}
	return &stepResult{kind: stepExec, exec: &execution{
		e: e, spec: spec, policy: effectivePolicy(spec, site), call: call, attempt: attempt, uncounted: e.uncounted, siteDeadline: siteDeadline,
	}}, nil
}

// toolInput renders the tool inputs as JSON: one input is sent as is,
// several as an array. ScopeProofs are unwrapped to their targets.
func toolInput(m *bytecode.Module, inputs []vm.Value) (json.RawMessage, error) {
	parts := make([]json.RawMessage, len(inputs))
	for i, a := range inputs {
		b, err := vm.ToJSON(m, plainTargets(a, 0), vm.JSONOptions{UnwrapProofs: true})
		if err != nil {
			return nil, err
		}
		parts[i] = b
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return json.Marshal(parts)
}

// rateLimit acquires the keys of spec 14.3. A denial is not a failure:
// the call waits retry_after without counting an attempt. A limiter error
// waits a fixed backoff (fail closed) unless the request can never pass.
func (r *runner) rateLimit(spec ToolSpec, proofs []vm.Value) (*stepResult, *EffectError) {
	if r.e.d.Limiter == nil {
		return nil, nil
	}
	program := "none"
	if !r.rec.ProgramID.IsZero() {
		program = r.rec.ProgramID.String()
	}
	tc := limiter.ToolCall{Class: spec.EffectClass, TenantID: r.rec.Tenant.String(), ProgramID: program, Tool: spec.Name}
	if len(proofs) > 0 {
		tc.Target = limitTarget(r.mod, proofs[0].ProofTarget())
	}
	if tc.Class == "" {
		tc.Class = "none"
	}
	reqs, err := tc.Requests(1)
	if err != nil {
		return nil, &EffectError{Code: "rate_limited", Message: err.Error()}
	}
	var res limiter.Result
	if wl, ok := r.e.d.Limiter.(WorkLimiter); ok {
		res, err = wl.AcquireAllWork(reqs, WorkOf(spec.EffectClass), r.e.d.Clock.Monotonic())
	} else {
		res, err = r.e.d.Limiter.AcquireAll(reqs, r.e.d.Clock.Monotonic())
	}
	switch {
	case err != nil && res.RetryAfter == limiter.Never:
		return nil, &EffectError{Code: "rate_limited", Message: "the request can never pass the rate limit: " + err.Error()}
	case err != nil:
		return waitUntil(r.now().Add(r.e.cfg.RateLimitErrorBackoff), "rate_limit"), nil
	case !res.Allowed:
		return waitUntil(r.now().Add(max(res.RetryAfter, time.Millisecond)), "rate_limit"), nil
	}
	return nil, nil
}

// limitTarget is the registrable domain or IP address of a target
// (rl:h key, spec 14.3).
func limitTarget(m *bytecode.Module, v vm.Value) string {
	raw, kind, ok := targetOf(m, v)
	if !ok {
		return ""
	}
	t, err := scope.Normalize(raw, kind)
	if err != nil {
		return ""
	}
	if n := t.Name(); n != "" {
		return netnorm.RegistrableDomain(n)
	}
	if a, ok := t.Addr(); ok {
		return a.String()
	}
	return t.String()
}

// execBatch runs prepared tool calls concurrently and records their
// outcomes in task order in one atomic append. If the run is cancelled,
// times out or hits a kill switch meanwhile, the calls are cancelled and
// nothing is recorded; the caller halts the body.
func (r *runner) execBatch(execs []*execution, ph phase) (*stepResult, error) {
	ctx, end := r.beginExec()
	results := make(chan execResult, len(execs))
	now := r.now()
	run := func(i int, call ToolCall, timeout time.Duration) {
		cctx, cancel := ctx, context.CancelFunc(func() {})
		if timeout > 0 {
			cctx, cancel = context.WithTimeout(ctx, timeout)
		}
		defer cancel()
		start := r.e.d.Clock.Monotonic()
		out, err := r.e.d.Executor.Execute(cctx, call)
		if err == nil && cctx.Err() != nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		results <- execResult{i: i, out: out, err: err, dur: r.e.d.Clock.Monotonic() - start}
	}
	timeouts := make([]time.Duration, len(execs))
	for i, x := range execs {
		timeout := x.call.Timeout
		bound := func(t time.Time) {
			if !t.IsZero() {
				if d := t.Sub(now); timeout <= 0 || d < timeout {
					timeout = max(d, time.Millisecond)
				}
			}
		}
		bound(x.siteDeadline)
		if ph == phaseBody {
			bound(r.rec.DeadlineAt)
		}
		timeouts[i] = timeout
	}
	if se, ok := r.e.d.Executor.(SerialExecutor); ok && se.SerialBatches() {
		go func() {
			for i, x := range execs {
				run(i, x.call, timeouts[i])
			}
		}()
	} else {
		for i, x := range execs {
			go run(i, x.call, timeouts[i])
		}
	}
	got := make([]*execResult, len(execs))
	done := ctx.Done()
	var grace <-chan time.Time
	for n := 0; n < len(execs); {
		select {
		case res := <-results:
			got[res.i] = &res
			n++
		case <-done:
			done = nil
			t := time.NewTimer(r.e.cfg.CancelGrace)
			defer t.Stop()
			grace = t.C
		case <-grace:
			n = len(execs) // abandon executors that ignore cancellation
		}
	}
	end()
	if r.fenced.Load() {
		return nil, ErrLeaseLost
	}
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	if why, _ := r.interrupted.Load().(string); why != "" {
		r.interrupted.Store("")
		if ph == phaseComp {
			return nil, fmt.Errorf("engine: compensation executions were interrupted (%s)", why)
		}
		r.reqsDirty.Store(true)
		switch why {
		case "timeout":
			return halt(haltTimeout, ""), nil
		case "kill_switch":
			if ks, err := r.e.d.Scope.KillSwitch(r.ctx, r.rec.Tenant, r.rec.ProgramID); err == nil && ks.Engaged {
				r.killReason = "kill_switch:" + ks.Level
			} else {
				r.killReason = "kill_switch"
			}
		}
		return halt(haltCancel, ""), nil
	}
	if ph == phaseBody && slices.ContainsFunc(got, func(res *execResult) bool { return res != nil && isKillSwitchRefusal(res.err) }) {
		// A kill-switch race: the switch was engaged after this advance's
		// re-check, and the network edge (the egress proxy, the enforcement
		// point) refused a call before it reached its target. The run halts
		// as the watchdog's kill-switch interrupt would, recording no
		// outcome for the batch, and the cancellation reason reports the
		// race ("kill_switch_race[:level]", G-ENG-09).
		r.reqsDirty.Store(true)
		r.killReason = "kill_switch_race"
		if ks, err := r.e.d.Scope.KillSwitch(r.ctx, r.rec.Tenant, r.rec.ProgramID); err == nil && ks.Engaged {
			r.killReason += ":" + ks.Level
		}
		return halt(haltCancel, ""), nil
	}
	var evs []pending
	for i, x := range execs {
		res := got[i]
		if res == nil {
			return nil, fmt.Errorf("engine: execution of %s did not return", x.e.path)
		}
		out, err := r.outcome(x, res)
		if err != nil {
			return nil, err
		}
		evs = append(evs, out...)
	}
	return nil, r.append(evs...)
}

// outcome turns one execution result into its events: observation_ingested
// and effect_completed, or effect_failed (plus effect_retry_scheduled when
// the error is retryable, attempts remain and the deadlines allow it).
func (r *runner) outcome(x *execution, res *execResult) ([]pending, error) {
	actor := ToolActor(x.spec.Name)
	if res.err == nil {
		v, err := decodeToolOutput(r.lm, x.spec.Name, res.out.Output)
		if err != nil {
			return r.failureEvents(x, EffectError{Code: "invalid_output", Message: err.Error()}, 0), nil
		}
		obs, err := r.ingest(x, res.out.Observations)
		if err != nil {
			return r.failureEvents(x, EffectError{Code: "ingest_unavailable", Message: err.Error(), Retryable: true}, 0), nil
		}
		val, disp, err := encodeResult(r.mod, v)
		if err != nil {
			return r.failureEvents(x, EffectError{Code: "invalid_output", Message: err.Error()}, 0), nil
		}
		return append(obs, ev(EvEffectCompleted, actor, EffectCompletedPayload{
			Path: x.e.path, Result: disp, Value: val, DurationMs: res.dur.Milliseconds(),
		})), nil
	}
	te, edgeWait := classify(res.err)
	return r.failureEvents(x, te, edgeWait), nil
}

// effectError is classify without the edge wait (effects that are not
// tool calls).
func effectError(err error) EffectError {
	ee, _ := classify(err)
	return ee
}

// isKillSwitchRefusal reports a ToolError with CodeKillSwitch.
func isKillSwitchRefusal(err error) bool {
	var te *ToolError
	return errors.As(err, &te) && te.Code == CodeKillSwitch
}

// classify maps an executor error to an effect error (spec 9.5). edgeWait
// is the retry delay of a rate-limit denial at the network edge (a
// ToolError with CodeRateLimited and RetryAfter), else 0.
func classify(err error) (ee EffectError, edgeWait time.Duration) {
	var te *ToolError
	switch {
	case errors.As(err, &te):
		if te.Code == CodeRateLimited && te.RetryAfter > 0 {
			edgeWait = te.RetryAfter
		}
		return EffectError{Code: te.Code, Message: te.Message, Retryable: te.Retryable}, edgeWait
	case errors.Is(err, context.DeadlineExceeded):
		return EffectError{Code: "deadline_exceeded", Message: err.Error(), Retryable: true}, 0
	case errors.Is(err, context.Canceled):
		return EffectError{Code: "cancelled", Message: err.Error()}, 0
	}
	return EffectError{Code: "unavailable", Message: err.Error(), Retryable: true}, 0
}

// failureEvents records a failed attempt and, when the policy allows,
// schedules the next one (spec 9.5). A rate-limit denial at the network
// edge (edgeWait > 0) is not a failure: the effect waits edgeWait (no
// jitter: the edge computed the delay) and the attempt is marked
// Uncounted, up to Config.MaxEdgeWaits times per effect. The policy's
// attempt budget and backoff use the counted attempts only.
func (r *runner) failureEvents(x *execution, te EffectError, edgeWait time.Duration) []pending {
	actor := ToolActor(x.spec.Name)
	now := r.now()
	inDeadlines := func(at time.Time) bool {
		return (r.rec.DeadlineAt.IsZero() || at.Before(r.rec.DeadlineAt)) && (x.siteDeadline.IsZero() || at.Before(x.siteDeadline))
	}
	if edgeWait > 0 && x.uncounted < r.e.cfg.MaxEdgeWaits {
		at := msTime(now.Add(edgeWait))
		if inDeadlines(at) {
			return []pending{
				ev(EvEffectFailed, actor, EffectFailedPayload{Path: x.e.path, Attempt: x.attempt, Error: te, Uncounted: true}),
				ev(EvEffectRetryScheduled, ActorEngine, EffectRetryScheduledPayload{Path: x.e.path, Attempt: x.attempt, RetryAt: fmtTime(at)}),
			}
		}
	}
	counted := x.attempt - x.uncounted
	code := bytecode.FailEffectFailed
	switch te.Code {
	case "scope_denied":
		code = bytecode.FailScopeDenied
	case "budget_exceeded":
		code = bytecode.FailBudgetExceeded
	}
	if te.Code == "deadline_exceeded" && !x.siteDeadline.IsZero() && !now.Before(x.siteDeadline) {
		code = bytecode.FailTimeout
	} else if retryable(x.policy, te) && counted < x.policy.MaxAttempts {
		wait := FullJitter(r.e.d.Rng, BackoffDelay(x.policy.Backoff, counted))
		at := msTime(now.Add(wait))
		if inDeadlines(at) {
			return []pending{
				ev(EvEffectFailed, actor, EffectFailedPayload{Path: x.e.path, Attempt: x.attempt, Error: te}),
				ev(EvEffectRetryScheduled, ActorEngine, EffectRetryScheduledPayload{Path: x.e.path, Attempt: x.attempt, RetryAt: fmtTime(at)}),
			}
		}
	}
	return []pending{ev(EvEffectFailed, actor, EffectFailedPayload{
		Path: x.e.path, Attempt: x.attempt, Error: te, Final: true, FailCode: code.String(),
	})}
}

// ingest forwards observations to the graph and returns their
// observation_ingested events. The engine fills in the envelope fields it
// owns (spec 6.1); observation ids are derived from the run, the path and
// the position, so re-ingesting after a crash is idempotent.
func (r *runner) ingest(x *execution, obs []json.RawMessage) ([]pending, error) {
	if len(obs) == 0 || r.e.d.Ingester == nil {
		return nil, nil
	}
	envs := make([]json.RawMessage, 0, len(obs))
	for i, raw := range obs {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("observation %d: %w", i, err)
		}
		if id, _ := m["observation_id"].(string); id == "" {
			m["observation_id"] = core.PublicID(core.PrefixObservation, derivedID("observation", r.run.String(), x.e.path, fmt.Sprint(i)))
		}
		m["tenant_id"] = r.rec.Tenant.String()
		m["run_id"] = r.run.String()
		m["effect_path"] = x.e.path
		if !r.rec.ProgramID.IsZero() {
			m["program_id"] = r.rec.ProgramID.String()
		}
		m["tool"] = map[string]any{"name": x.spec.Name, "version": x.spec.Version}
		b, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		envs = append(envs, b)
	}
	res, err := r.e.d.Ingester.Ingest(r.ctx, IngestRequest{
		Tenant: r.rec.Tenant, Program: r.rec.ProgramID, RunID: r.run, Path: x.e.path,
		Tool: x.spec.Name, ToolVersion: x.spec.Version, Observations: envs,
	})
	if err != nil {
		return nil, err
	}
	out := make([]pending, 0, len(res))
	for _, ir := range res {
		out = append(out, ev(EvObservationIngested, ActorEngine, ObservationIngestedPayload{
			Path: x.e.path, ObservationID: ir.ObservationID, EntityCount: ir.EntityCount, EdgeCount: ir.EdgeCount,
		}))
	}
	return out, nil
}
