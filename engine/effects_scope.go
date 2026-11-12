package engine

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/vm"
)

// checkClass is the activity class scope.check effects are decided for:
// the workflow's inferred class (at least passive), so that one proof
// covers every tool call the workflow may make with it.
func (r *runner) checkClass() core.ActivityClass {
	return max(r.ver.InferredClass, core.ClassPassive)
}

// reasonScopeUnavailable is the deny reason recorded when the scope
// service could not be asked.
const reasonScopeUnavailable = "scope_unavailable"

// decideScope asks the scope service about one target and returns the
// decision. Every failure is a deny (spec 2.4: no silent success).
func (r *runner) decideScope(target vm.Value, class core.ActivityClass, version int32, path, purpose string, n int) scope.Decision {
	id := core.PublicID(core.PrefixDecision, derivedID("decision", r.run.String(), path, purpose, fmt.Sprint(n), fmt.Sprint(version)))
	deny := func(reason, tgt string) scope.Decision {
		return scope.Decision{ID: id, Result: scope.ResultDeny, Reason: reason, ScopeVersion: int(version),
			Target: tgt, PinnedIPs: []netip.Addr{}, DecidedAt: msTime(r.now())}
	}
	raw, kind, ok := targetOf(r.mod, target)
	if !ok {
		return deny(scope.ReasonInvalidTarget, "")
	}
	if r.rec.ProgramID.IsZero() {
		return deny("no_program", normalizedTarget(r.mod, target))
	}
	d, err := r.e.d.Scope.Check(r.ctx, ScopeCheck{
		Tenant: r.rec.Tenant, Program: r.rec.ProgramID, ScopeVersion: version, Target: raw, Kind: kind,
		Class: class, RunID: r.run, Path: path, Now: msTime(r.now()), DecisionID: id,
	})
	if err != nil {
		return deny(reasonScopeUnavailable, normalizedTarget(r.mod, target))
	}
	if d.ID == "" {
		d.ID = id
	}
	if d.DecidedAt.IsZero() {
		d.DecidedAt = msTime(r.now())
	}
	d.DecidedAt = msTime(d.DecidedAt)
	if d.ScopeVersion == 0 {
		d.ScopeVersion = int(version)
	}
	if d.PinnedIPs == nil {
		d.PinnedIPs = []netip.Addr{}
	}
	if d.Allowed() && int32(d.ScopeVersion) != version {
		return deny("scope_version_mismatch", d.Target)
	}
	return d
}

// doScopeCheck performs scope.check(x): it records the decision and
// completes with Ok(InScope proof) or Err(ScopeDenied) (spec 9.5).
func (r *runner) doScopeCheck(s vm.Suspended, e *effectState) error {
	if len(s.Args) != 1 {
		return r.failEffect(e, ActorEngine, EffectError{Code: "invalid_arguments", Message: "scope_check takes one target"}, bytecode.FailTypeMismatch)
	}
	target := s.Args[0]
	d := r.decideScope(target, r.checkClass(), r.log.scopeVersion, e.path, PurposeScopeCheck, 0)
	var res vm.Value
	if d.Allowed() {
		res = vm.Ok(vm.ScopeProof(target, d.ID))
	} else {
		shown := d.Target
		if shown == "" {
			raw, _, _ := targetOf(r.mod, target)
			shown = raw
		}
		res = vm.Err(builtinRecord(bytecode.TypeScopeDenied, map[string]vm.Value{
			"target": vm.Str(shown), "reason": vm.Str(d.Reason), "rule_id": optStr(d.RuleID), "decision_id": vm.Str(d.ID),
		}))
	}
	return r.completeEffect(e, ActorEngine, res,
		ev(EvScopeDecision, ActorEngine, ScopeDecisionPayload{Path: e.path, Purpose: PurposeScopeCheck, Decision: d}))
}

// verifyProofs checks invariant inv_06 for a tool call: every ScopeProof
// argument must name an allow decision recorded in this run for exactly
// that (normalized) target, and the run must hold an allow decision for
// the target under its current scope version. A target-facing tool
// (active or intrusive) without any proof is refused.
func (r *runner) verifyProofs(spec ToolSpec, proofs []vm.Value) string {
	if len(proofs) == 0 {
		if activityClass(spec.EffectClass) >= core.ClassActive {
			return fmt.Sprintf("%s is %s and takes no scope proof", spec.Name, spec.EffectClass)
		}
		return ""
	}
	for _, p := range proofs {
		tgt := normalizedTarget(r.mod, p.ProofTarget())
		d, ok := r.log.decisions[p.ProofDecision()]
		switch {
		case !ok:
			return fmt.Sprintf("proof for %q names decision %q, which this run did not record", tgt, p.ProofDecision())
		case !d.Allowed():
			return fmt.Sprintf("decision %s denied %q", d.ID, d.Target)
		case tgt == "" || d.Target != tgt:
			return fmt.Sprintf("decision %s is for %q, not %q", d.ID, d.Target, tgt)
		}
		if _, ok := r.log.allowed[targetVersion{tgt, r.log.scopeVersion}]; !ok {
			return fmt.Sprintf("%q has no allow decision under scope version %d", tgt, r.log.scopeVersion)
		}
	}
	return ""
}

// recheck re-decides every proof target of a tool call right before it
// runs, with the tool's own class (spec 3.3), and records the decisions.
// It returns the first denial.
func (r *runner) recheck(spec ToolSpec, e *effectState, proofs []vm.Value) (*scope.Decision, error) {
	if len(e.decisions) > 0 {
		for _, id := range e.decisions {
			if d := r.log.decisions[id]; !d.Allowed() {
				return &d, nil
			}
		}
		return nil, nil
	}
	class := max(activityClass(spec.EffectClass), core.ClassPassive)
	var evs []pending
	var denied *scope.Decision
	seen := map[string]bool{}
	for i, p := range proofs {
		tgt := normalizedTarget(r.mod, p.ProofTarget())
		if seen[tgt] {
			continue
		}
		seen[tgt] = true
		d := r.decideScope(p.ProofTarget(), class, r.log.scopeVersion, e.path, PurposeRecheck, i)
		evs = append(evs, ev(EvScopeDecision, ActorEngine, ScopeDecisionPayload{Path: e.path, Purpose: PurposeRecheck, Decision: d}))
		if !d.Allowed() && denied == nil {
			denied = &d
		}
	}
	if err := r.append(evs...); err != nil {
		return nil, err
	}
	return denied, nil
}

// revalidate applies a scope version change at an effect boundary: the
// run's class must fit the new max_class and every target the run holds
// an allow decision for must still be allowed. The decisions are
// recorded; the run then continues pinned to the new version or pauses.
//
// If the scope service cannot be reached (the policy cannot be loaded for
// any reason but not-found, or a re-decision is scope_unavailable),
// nothing is recorded and retry is true: the run waits at the boundary
// (fail closed, no effect runs) and re-validates later. Recording a pause
// for an outage would leave the run paused until the next scope version,
// although the new version may well allow it (regress_scope_outage_pause).
func (r *runner) revalidate(version int32, path string) (paused, retry bool, err error) {
	old := r.log.scopeVersion
	ok := true
	pol, perr := r.e.d.Scope.Policy(r.ctx, r.rec.Tenant, r.rec.ProgramID, version)
	switch {
	case perr != nil && !errors.Is(perr, ErrNotFound):
		return false, true, nil
	case perr != nil || pol.MaxClass() < r.ver.InferredClass:
		ok = false
	}
	var evs []pending
	if ok {
		var targets []string
		for tv := range r.log.allowed {
			if tv.version == old {
				targets = append(targets, tv.target)
			}
		}
		slices.Sort(targets)
		targets = slices.Compact(targets)
		for n, tgt := range targets {
			d := r.decideScope(vm.Str(tgt), r.checkClass(), version, path, PurposeRevalidate, n)
			if d.Reason == reasonScopeUnavailable {
				return false, true, nil
			}
			evs = append(evs, ev(EvScopeDecision, ActorEngine, ScopeDecisionPayload{Path: path, Purpose: PurposeRevalidate, Decision: d}))
			if !d.Allowed() {
				ok = false
			}
		}
	}
	action := ScopeActionContinue
	if !ok {
		action = ScopeActionPause
	}
	evs = append(evs, ev(EvScopeChanged, ActorEngine, ScopeChangedPayload{OldVersion: old, NewVersion: version, Action: action}))
	switch {
	case !ok && !r.log.paused:
		evs = append(evs, ev(EvRunPaused, ActorEngine, ReasonPayload{Reason: "scope_changed"}))
	case ok && r.log.paused:
		evs = append(evs, ev(EvRunResumed, ActorEngine, ReasonPayload{Reason: "scope_changed"}))
	}
	if err := r.append(evs...); err != nil {
		return false, false, err
	}
	r.markApplied(RequestScopeChanged)
	return !ok, false, nil
}

// scopeSnapshot backs scope.allows (spec 7.6) with the scope version
// pinned at submission. It stays fixed for the whole run, even after a
// scope change, so replay sees the same answers (vm OPEN_QUESTIONS 10);
// scope.check, the authoritative gate, uses the current version.
func (r *runner) scopeSnapshot() vm.ScopeSnapshot {
	if r.rec.ProgramID.IsZero() {
		return nil
	}
	pol, err := r.e.d.Scope.Policy(r.ctx, r.rec.Tenant, r.rec.ProgramID, r.log.submitted.ScopeVersion)
	if err != nil {
		return denyAll{}
	}
	return policySnapshot{pol: pol, mod: r.mod}
}

type policySnapshot struct {
	pol *scope.Policy
	mod *bytecode.Module
}

func (p policySnapshot) Allows(target vm.Value) bool {
	raw, kind, ok := targetOf(p.mod, target)
	return ok && p.pol.Allows(raw, kind)
}

type denyAll struct{}

func (denyAll) Allows(vm.Value) bool { return false }
