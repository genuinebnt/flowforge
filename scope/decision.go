package scope

import (
	"encoding/json"
	"net/netip"
	"slices"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// Decision reasons (spec 4.3 and the Decision message of section 11).
const (
	ReasonAllow               = "allow"
	ReasonKillSwitch          = "kill_switch"
	ReasonInvalidTarget       = "invalid_target"
	ReasonClassExceedsProgram = "class_exceeds_program"
	ReasonOutsideWindow       = "outside_window"
	ReasonExplicitDeny        = "explicit_deny"
	ReasonResolvedIPDenied    = "resolved_ip_denied"
	ReasonNoMatchingAllow     = "no_matching_allow"
	// ReasonResolutionFailed is an extension: an active check of a name
	// whose resolution failed, returned no usable address, or had no
	// resolver. The spec requires failing closed (invariant 2.4) but names
	// no reason; see OPEN_QUESTIONS.md.
	ReasonResolutionFailed = "resolution_failed"
	// ReasonInvalidClass is an extension: the request's activity class is
	// not passive, active or intrusive. See OPEN_QUESTIONS.md.
	ReasonInvalidClass = "invalid_class"
)

// Decision is the outcome of one scope check (spec 4.3). The same value is
// stored in scope_decisions and in the run's event log.
type Decision struct {
	// ID is assigned by the scope service when it records the decision;
	// Check leaves it empty.
	ID           string
	Result       DecisionResult
	Reason       string
	RuleID       string // the deciding rule, empty for reasons without one
	ScopeVersion int
	Target       string       // normalized target, empty for invalid_target
	PinnedIPs    []netip.Addr // addresses the following connection may use; never nil
	DecidedAt    time.Time
}

// Allowed reports whether d permits the action. Anything but an explicit
// allow, including the zero Decision, is a denial.
func (d Decision) Allowed() bool { return d.Result == ResultAllow }

// Equal reports whether d and o are the same decision: every field equal,
// PinnedIPs in the same order, DecidedAt the same instant (time.Time.Equal,
// so the location is ignored).
func (d Decision) Equal(o Decision) bool {
	return d.ID == o.ID && d.Result == o.Result && d.Reason == o.Reason && d.RuleID == o.RuleID &&
		d.ScopeVersion == o.ScopeVersion && d.Target == o.Target &&
		slices.Equal(d.PinnedIPs, o.PinnedIPs) && d.DecidedAt.Equal(o.DecidedAt)
}

type decisionJSON struct {
	ID           string         `json:"decision_id,omitempty"`
	Result       DecisionResult `json:"result"`
	Reason       string         `json:"reason"`
	RuleID       string         `json:"rule_id,omitempty"`
	ScopeVersion int            `json:"scope_version"`
	Target       string         `json:"normalized_target"`
	PinnedIPs    []netip.Addr   `json:"pinned_ips"`
	DecidedAt    string         `json:"decided_at,omitempty"`
}

// MarshalJSON renders the REST shape of spec 12.5, with decided_at in the
// canonical millisecond UTC form (omitted when zero) and pinned_ips always
// an array, never null.
//
// PORT: netip.Addr marshals as its String form: dotted quad for IPv4 and
// RFC 5952 compressed lowercase for IPv6 (Rust's Ipv6Addr Display matches).
func (d Decision) MarshalJSON() ([]byte, error) {
	j := decisionJSON{
		ID: d.ID, Result: d.Result, Reason: d.Reason, RuleID: d.RuleID,
		ScopeVersion: d.ScopeVersion, Target: d.Target, PinnedIPs: d.PinnedIPs,
	}
	if j.PinnedIPs == nil {
		j.PinnedIPs = []netip.Addr{}
	}
	if !d.DecidedAt.IsZero() {
		j.DecidedAt = core.FormatTime(d.DecidedAt)
	}
	return json.Marshal(j)
}

// UnmarshalJSON implements json.Unmarshaler for the MarshalJSON shape. It
// is tolerant (unknown fields ignored, spec 1.2 responses) and turns a
// missing or null pinned_ips into an empty slice.
func (d *Decision) UnmarshalJSON(b []byte) error {
	var j decisionJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	out := Decision{
		ID: j.ID, Result: j.Result, Reason: j.Reason, RuleID: j.RuleID,
		ScopeVersion: j.ScopeVersion, Target: j.Target, PinnedIPs: j.PinnedIPs,
	}
	if out.PinnedIPs == nil {
		out.PinnedIPs = []netip.Addr{}
	}
	if j.DecidedAt != "" {
		t, err := core.ParseTime(j.DecidedAt)
		if err != nil {
			return err
		}
		out.DecidedAt = t
	}
	*d = out
	return nil
}
