package scope

import "fmt"

// RuleEffect says whether a rule grants or removes permission (spec 4.1).
// Deny always wins over allow.
type RuleEffect int

// Rule effects. The zero value is invalid and rejected by Validate.
const (
	EffectUnspecified RuleEffect = iota
	EffectAllow
	EffectDeny
)

var effectNames = []string{"", "allow", "deny"}

// String returns the wire name of e ("allow" or "deny").
func (e RuleEffect) String() string { return enumName(effectNames, int(e), "effect") }

// MarshalText implements encoding.TextMarshaler.
func (e RuleEffect) MarshalText() ([]byte, error) { return marshalEnum(effectNames, int(e), "effect") }

// UnmarshalText implements encoding.TextUnmarshaler.
func (e *RuleEffect) UnmarshalText(b []byte) error {
	return unmarshalEnum(effectNames, b, "effect", (*int)(e))
}

// RuleKind is the kind of value a rule matches (spec 4.1).
type RuleKind int

// Rule kinds. The zero value is invalid and rejected by Validate.
const (
	RuleUnspecified RuleKind = iota
	// RuleDomain matches exactly one normalized name.
	RuleDomain
	// RuleWildcardDomain ("*.x") matches any name strictly below x, at any
	// depth, never x itself.
	RuleWildcardDomain
	// RuleCIDR matches IPv4 or IPv6 addresses inside a prefix.
	RuleCIDR
	// RuleURLPrefix matches URLs with the same scheme, host and port whose
	// path starts with the rule's path on a segment boundary.
	RuleURLPrefix
	// RuleASN is informational input to discovery only; it never matches a
	// target (see OPEN_QUESTIONS.md).
	RuleASN
)

var ruleKindNames = []string{"", "domain", "wildcard_domain", "cidr", "url_prefix", "asn"}

// String returns the wire name of k.
func (k RuleKind) String() string { return enumName(ruleKindNames, int(k), "rule_kind") }

// MarshalText implements encoding.TextMarshaler.
func (k RuleKind) MarshalText() ([]byte, error) {
	return marshalEnum(ruleKindNames, int(k), "rule_kind")
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (k *RuleKind) UnmarshalText(b []byte) error {
	return unmarshalEnum(ruleKindNames, b, "rule_kind", (*int)(k))
}

// TargetKind says how a raw target string is to be parsed (spec 11, proto
// TargetKind).
type TargetKind int

// Target kinds. TargetUnspecified asks Normalize to infer the kind from the
// text: "://" means a URL, "/" a CIDR prefix, anything else a host.
const (
	TargetUnspecified TargetKind = iota
	// TargetHost is a DNS name or an IP literal (IPv6 may be bracketed).
	TargetHost
	// TargetIP is a strict IP address literal.
	TargetIP
	// TargetURL is an http or https URL.
	TargetURL
	// TargetCIDR is an IPv4 or IPv6 prefix.
	TargetCIDR
)

var targetKindNames = []string{"", "host", "ip", "url", "cidr"}

// String returns the wire name of k ("" for TargetUnspecified).
func (k TargetKind) String() string { return enumName(targetKindNames, int(k), "target_kind") }

// MarshalText implements encoding.TextMarshaler.
func (k TargetKind) MarshalText() ([]byte, error) {
	if k == TargetUnspecified {
		return []byte{}, nil
	}
	return marshalEnum(targetKindNames, int(k), "target_kind")
}

// UnmarshalText implements encoding.TextUnmarshaler. It also accepts
// "hostname" as an alias of "host" (the spelling used by spec 25.9) and the
// empty string for TargetUnspecified.
func (k *TargetKind) UnmarshalText(b []byte) error {
	switch string(b) {
	case "":
		*k = TargetUnspecified
		return nil
	case "hostname":
		*k = TargetHost
		return nil
	}
	return unmarshalEnum(targetKindNames, b, "target_kind", (*int)(k))
}

// DecisionResult is the outcome of a check.
type DecisionResult int

// Decision results. The zero value is neither allow nor deny; callers must
// treat anything but ResultAllow as a denial.
const (
	ResultUnspecified DecisionResult = iota
	ResultAllow
	ResultDeny
)

var resultNames = []string{"", "allow", "deny"}

// String returns the wire name of r.
func (r DecisionResult) String() string { return enumName(resultNames, int(r), "result") }

// MarshalText implements encoding.TextMarshaler.
func (r DecisionResult) MarshalText() ([]byte, error) {
	return marshalEnum(resultNames, int(r), "result")
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (r *DecisionResult) UnmarshalText(b []byte) error {
	return unmarshalEnum(resultNames, b, "result", (*int)(r))
}

// Status is the passive scope status attached to graph entities (spec 4.4,
// 5.7). The zero value is StatusUnknown, so an unset status is never read
// as in scope.
type Status int

// Scope statuses.
const (
	StatusUnknown Status = iota
	StatusInScope
	StatusOutOfScope
)

var statusNames = []string{"unknown", "in_scope", "out_of_scope"}

// String returns the wire name of s.
func (s Status) String() string { return enumName(statusNames, int(s), "status") }

// MarshalText implements encoding.TextMarshaler.
func (s Status) MarshalText() ([]byte, error) { return marshalEnum(statusNames, int(s), "status") }

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *Status) UnmarshalText(b []byte) error {
	return unmarshalEnum(statusNames, b, "status", (*int)(s))
}

func enumName(names []string, v int, what string) string {
	if v >= 0 && v < len(names) {
		return names[v]
	}
	return fmt.Sprintf("%s(%d)", what, v)
}

func marshalEnum(names []string, v int, what string) ([]byte, error) {
	if v < 0 || v >= len(names) || names[v] == "" {
		return nil, fmt.Errorf("%w: %s(%d)", ErrInvalidEnum, what, v)
	}
	return []byte(names[v]), nil
}

func unmarshalEnum(names []string, b []byte, what string, dst *int) error {
	s := string(b)
	if s != "" {
		for i, n := range names {
			if n == s {
				*dst = i
				return nil
			}
		}
	}
	return fmt.Errorf("%w: unknown %s %q", ErrInvalidEnum, what, s)
}
