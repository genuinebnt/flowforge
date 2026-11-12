package scope

import (
	"context"
	"net/netip"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// Request is one scope check.
type Request struct {
	// Target is the raw target; Check normalizes it (spec 4.2).
	Target string `json:"target"`
	// Kind says how to parse Target; TargetUnspecified infers it.
	Kind TargetKind `json:"kind,omitempty"`
	// Class is the activity class of the action about to be taken.
	Class core.ActivityClass `json:"activity_class"`
	// Now is the instant the decision is made for. It is supplied by the
	// caller (the engine's recorded clock), never read here.
	Now time.Time `json:"-"`
	// KillSwitch is true when a global, tenant or program kill switch is
	// engaged. It is checked before anything else.
	KillSwitch bool `json:"-"`
	// SkipResolution turns off resolution and resolved-address checks for
	// an active check of a name (the proto CheckRequest.resolve = false
	// case). The decision then carries no pinned addresses, so the egress
	// proxy, which connects only to pinned addresses, cannot act on it. It
	// exists for advisory checks; the engine never sets it.
	SkipResolution bool `json:"-"`
}

// Check runs the decision algorithm of spec 4.3 for req:
//
//  1. kill switch engaged -> deny(kill_switch)
//  2. target fails normalization -> deny(invalid_target)
//  3. class not passive, active or intrusive -> deny(invalid_class)
//  4. class above activity.max_class -> deny(class_exceeds_program)
//  5. non-passive outside every window -> deny(outside_window)
//  6. a deny rule matches -> deny(explicit_deny, rule)
//  7. non-passive name target (unless SkipResolution): resolve once;
//     resolution fails -> deny(resolution_failed); an address in a deny
//     cidr -> deny(resolved_ip_denied, rule)
//  8. an allow rule matches -> allow(rule, pinned addresses)
//  9. otherwise deny(no_matching_allow)
//
// When several rules match, the first in document order is reported.
// Check never fails: every problem, including an unreachable resolver,
// becomes a deny.
func (p *Policy) Check(ctx context.Context, req Request) Decision {
	d := Decision{ScopeVersion: p.doc.Version, DecidedAt: req.Now, PinnedIPs: []netip.Addr{}}
	t, err := Normalize(req.Target, req.Kind)
	if err == nil {
		d.Target = t.String()
	}
	deny := func(reason string, idx int32) Decision {
		d.Result, d.Reason = ResultDeny, reason
		if idx != noRule {
			d.RuleID = p.rules[idx].id
		}
		return d
	}
	switch {
	case req.KillSwitch:
		return deny(ReasonKillSwitch, noRule)
	case err != nil:
		return deny(ReasonInvalidTarget, noRule)
	case req.Class < core.ClassPassive || req.Class > core.ClassIntrusive:
		return deny(ReasonInvalidClass, noRule)
	case req.Class > p.doc.Activity.MaxClass:
		return deny(ReasonClassExceedsProgram, noRule)
	}
	active := req.Class != core.ClassPassive
	if active && !inAnyWindow(p.windows, req.Now) {
		return deny(ReasonOutsideWindow, noRule)
	}
	if idx := p.first(&t, EffectDeny); idx != noRule {
		return deny(ReasonExplicitDeny, idx)
	}
	var pinned []netip.Addr
	if active && t.IsName() && !req.SkipResolution {
		ips, err := resolveOnce(ctx, p.resolver, t.Name())
		if err != nil {
			return deny(ReasonResolutionFailed, noRule)
		}
		for _, ip := range ips {
			if idx := p.firstDenyCIDR(ip); idx != noRule {
				return deny(ReasonResolvedIPDenied, idx)
			}
		}
		pinned = ips
	}
	idx := p.first(&t, EffectAllow)
	if idx == noRule {
		return deny(ReasonNoMatchingAllow, noRule)
	}
	d.Result, d.Reason, d.RuleID = ResultAllow, ReasonAllow, p.rules[idx].id
	if pinned != nil {
		d.PinnedIPs = pinned
	}
	return d
}

// Status computes the passive scope status of a graph entity (spec 5.7):
// the rule-matching part of Check in passive mode, without resolution,
// windows or class limits. A target that cannot be normalized is
// StatusUnknown; nothing is ever inferred into scope.
func (p *Policy) Status(target string, kind TargetKind) Status {
	t, err := Normalize(target, kind)
	if err != nil {
		return StatusUnknown
	}
	if p.first(&t, EffectDeny) != noRule {
		return StatusOutOfScope
	}
	if p.first(&t, EffectAllow) != noRule {
		return StatusInScope
	}
	return StatusOutOfScope
}

// Allows is the DSL's scope.allows pre-filter (spec 7.6): true only when
// Status is StatusInScope. It is a hint, not a proof; active work still
// needs Check.
func (p *Policy) Allows(target string, kind TargetKind) bool {
	return p.Status(target, kind) == StatusInScope
}
