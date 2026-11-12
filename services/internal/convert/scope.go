package convert

import (
	"net/netip"

	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/scope"
)

// ScopeDoc converts a proto scope document to the domain form (spec 4.1).
func ScopeDoc(d *pb.ScopeDocument) (scope.Document, error) {
	doc := scope.Document{Rules: []scope.Rule{}, Activity: scope.Activity{Windows: []scope.Window{}}}
	if d == nil {
		return doc, Invalid("document", "scope document is required")
	}
	for i, r := range d.GetRules() {
		var eff scope.RuleEffect
		switch r.GetEffect() {
		case pb.RuleEffect_RULE_EFFECT_ALLOW:
			eff = scope.EffectAllow
		case pb.RuleEffect_RULE_EFFECT_DENY:
			eff = scope.EffectDeny
		default:
			return doc, Invalid("document.rules", "rule %d: effect is required", i)
		}
		var kind scope.RuleKind
		switch r.GetKind() {
		case pb.RuleKind_RULE_KIND_DOMAIN:
			kind = scope.RuleDomain
		case pb.RuleKind_RULE_KIND_WILDCARD_DOMAIN:
			kind = scope.RuleWildcardDomain
		case pb.RuleKind_RULE_KIND_CIDR:
			kind = scope.RuleCIDR
		case pb.RuleKind_RULE_KIND_URL_PREFIX:
			kind = scope.RuleURLPrefix
		case pb.RuleKind_RULE_KIND_ASN:
			kind = scope.RuleASN
		default:
			return doc, Invalid("document.rules", "rule %d: kind is required", i)
		}
		doc.Rules = append(doc.Rules, scope.Rule{ID: r.GetId(), Effect: eff, Kind: kind, Value: r.GetValue(), Note: r.GetNote()})
	}
	a := d.GetActivity()
	doc.Activity.MaxClass = ClassFrom(a.GetMaxClass())
	doc.Activity.Prohibited = a.GetProhibited()
	if r := a.GetRate(); r != nil {
		doc.Activity.Rate = scope.Rate{RequestsPerSecond: r.GetRequestsPerSecond(), Burst: int(r.GetBurst()), Concurrency: int(r.GetConcurrency())}
	}
	for _, w := range a.GetWindows() {
		doc.Activity.Windows = append(doc.Activity.Windows, scope.Window{Days: w.GetDays(), Start: w.GetStart(), End: w.GetEnd(), TZ: w.GetTz()})
	}
	if err := doc.Validate(); err != nil {
		return doc, &core.Error{Kind: core.KindInvalidArgument, Reason: core.ReasonValidationFailed, Message: err.Error(), Err: err}
	}
	return doc, nil
}

// ScopeDocProto converts a domain scope document.
func ScopeDocProto(d scope.Document) *pb.ScopeDocument {
	out := &pb.ScopeDocument{Activity: &pb.ActivityPolicy{
		MaxClass: Class(d.Activity.MaxClass), Prohibited: d.Activity.Prohibited,
	}}
	if r := d.Activity.Rate; r != (scope.Rate{}) {
		out.Activity.Rate = &pb.RateLimit{RequestsPerSecond: r.RequestsPerSecond, Burst: int32(r.Burst), Concurrency: int32(r.Concurrency)}
	}
	for _, w := range d.Activity.Windows {
		out.Activity.Windows = append(out.Activity.Windows, &pb.TimeWindow{Days: w.Days, Start: w.Start, End: w.End, Tz: w.TZ})
	}
	for _, r := range d.Rules {
		pr := &pb.ScopeRule{Id: r.ID, Value: r.Value, Note: r.Note}
		switch r.Effect {
		case scope.EffectAllow:
			pr.Effect = pb.RuleEffect_RULE_EFFECT_ALLOW
		case scope.EffectDeny:
			pr.Effect = pb.RuleEffect_RULE_EFFECT_DENY
		}
		switch r.Kind {
		case scope.RuleDomain:
			pr.Kind = pb.RuleKind_RULE_KIND_DOMAIN
		case scope.RuleWildcardDomain:
			pr.Kind = pb.RuleKind_RULE_KIND_WILDCARD_DOMAIN
		case scope.RuleCIDR:
			pr.Kind = pb.RuleKind_RULE_KIND_CIDR
		case scope.RuleURLPrefix:
			pr.Kind = pb.RuleKind_RULE_KIND_URL_PREFIX
		case scope.RuleASN:
			pr.Kind = pb.RuleKind_RULE_KIND_ASN
		}
		out.Rules = append(out.Rules, pr)
	}
	return out
}

// TargetKind converts a proto target kind.
func TargetKind(k pb.TargetKind) scope.TargetKind {
	switch k {
	case pb.TargetKind_TARGET_KIND_HOST:
		return scope.TargetHost
	case pb.TargetKind_TARGET_KIND_IP:
		return scope.TargetIP
	case pb.TargetKind_TARGET_KIND_URL:
		return scope.TargetURL
	case pb.TargetKind_TARGET_KIND_CIDR:
		return scope.TargetCIDR
	}
	return scope.TargetUnspecified
}

// TargetKindProto converts a domain target kind.
func TargetKindProto(k scope.TargetKind) pb.TargetKind {
	switch k {
	case scope.TargetHost:
		return pb.TargetKind_TARGET_KIND_HOST
	case scope.TargetIP:
		return pb.TargetKind_TARGET_KIND_IP
	case scope.TargetURL:
		return pb.TargetKind_TARGET_KIND_URL
	case scope.TargetCIDR:
		return pb.TargetKind_TARGET_KIND_CIDR
	}
	return pb.TargetKind_TARGET_KIND_UNSPECIFIED
}

// DecisionProto converts a scope decision.
func DecisionProto(d scope.Decision) *pb.Decision {
	out := &pb.Decision{DecisionId: d.ID, Reason: d.Reason, RuleId: d.RuleID, ScopeVersion: int32(d.ScopeVersion),
		NormalizedTarget: d.Target, DecidedAt: TS(d.DecidedAt), Result: pb.DecisionResult_DECISION_RESULT_DENY}
	if d.Allowed() {
		out.Result = pb.DecisionResult_DECISION_RESULT_ALLOW
	}
	for _, ip := range d.PinnedIPs {
		out.PinnedIps = append(out.PinnedIps, ip.String())
	}
	return out
}

// Decision converts a proto decision. Anything but an explicit allow is a
// deny, and unparsable pinned addresses turn an allow into a deny (fail
// closed).
func Decision(d *pb.Decision) scope.Decision {
	out := scope.Decision{ID: d.GetDecisionId(), Result: scope.ResultDeny, Reason: d.GetReason(), RuleID: d.GetRuleId(),
		ScopeVersion: int(d.GetScopeVersion()), Target: d.GetNormalizedTarget(), DecidedAt: Time(d.GetDecidedAt()), PinnedIPs: []netip.Addr{}}
	if d.GetResult() == pb.DecisionResult_DECISION_RESULT_ALLOW {
		out.Result = scope.ResultAllow
	}
	for _, s := range d.GetPinnedIps() {
		a, err := netip.ParseAddr(s)
		if err != nil {
			out.Result, out.Reason = scope.ResultDeny, "invalid_pinned_ip"
			continue
		}
		out.PinnedIPs = append(out.PinnedIPs, a)
	}
	return out
}
