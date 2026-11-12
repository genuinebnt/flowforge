package scope

import (
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// Document is a program scope document (spec 4.1). It is the published,
// versioned input to Compile; JSON tags follow the wire form.
type Document struct {
	ProgramID     string    `json:"program_id,omitempty"`
	Version       int       `json:"version,omitempty"`
	Name          string    `json:"name,omitempty"`
	EffectiveFrom time.Time `json:"effective_from,omitzero"`
	Rules         []Rule    `json:"rules"`
	Activity      Activity  `json:"activity"`
}

// Rule is one allow or deny rule. Rules are evaluated as a set; when
// several rules of the same effect match, the one that comes first in the
// document is reported.
type Rule struct {
	ID     string     `json:"id"`
	Effect RuleEffect `json:"effect"`
	Kind   RuleKind   `json:"kind"`
	Value  string     `json:"value"`
	Note   string     `json:"note,omitempty"`
}

// Activity is the program's activity policy.
type Activity struct {
	// MaxClass is the highest activity class any run may perform. The zero
	// value (core.ClassNone) permits nothing.
	MaxClass core.ActivityClass `json:"max_class"`
	// Prohibited lists technique names the program forbids outright. It is
	// enforced by the policy check, not by Check.
	Prohibited []string `json:"prohibited,omitempty"`
	// Rate is the program-level ceiling enforced by the limiter.
	Rate Rate `json:"rate,omitzero"`
	// Windows are the testing windows. Empty means always open.
	Windows []Window `json:"windows"`
}

// Rate is a program-level rate ceiling (spec 14).
type Rate struct {
	RequestsPerSecond float64 `json:"requests_per_second"`
	Burst             int     `json:"burst"`
	Concurrency       int     `json:"concurrency"`
}

// Window is a weekly testing window. Days are mon..sun; Start and End are
// HH:MM wall-clock times in TZ (an IANA zone name). End is exclusive and
// may be "24:00". A window with End before Start runs past midnight into
// the next day; the day it starts on is the one listed in Days.
type Window struct {
	Days  []string `json:"days"`
	Start string   `json:"start"`
	End   string   `json:"end"`
	TZ    string   `json:"tz"`
}

// Validate reports whether d can be published: rule ids are unique and
// non-empty, every effect and kind is known, every value normalizes, and
// the activity policy and windows are well formed. The returned error
// joins one wrapped error per problem; each wraps ErrInvalidDocument.
func (d Document) Validate() error {
	_, err := compileDocument(d)
	return err
}
