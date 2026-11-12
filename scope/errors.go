package scope

import "errors"

// Sentinel errors. Validation and compile errors wrap one of these (and,
// where useful, the underlying netnorm or time error) with %w.
var (
	// ErrInvalidDocument is wrapped by every document validation error.
	ErrInvalidDocument = errors.New("scope: invalid scope document")
	// ErrInvalidRule reports a rule whose id, effect, kind or value is bad.
	ErrInvalidRule = errors.New("scope: invalid rule")
	// ErrDuplicateRuleID reports two rules with the same id.
	ErrDuplicateRuleID = errors.New("scope: duplicate rule id")
	// ErrInvalidWindow reports a malformed activity window.
	ErrInvalidWindow = errors.New("scope: invalid activity window")
	// ErrInvalidActivity reports a malformed activity policy.
	ErrInvalidActivity = errors.New("scope: invalid activity policy")
	// ErrInvalidTarget reports a target that fails normalization
	// (spec 4.2). Decisions carry it as reason invalid_target.
	ErrInvalidTarget = errors.New("scope: invalid target")
	// ErrInvalidEnum reports an unknown enum name or value.
	ErrInvalidEnum = errors.New("scope: invalid enum value")
	// ErrNoSuchHost is returned by StaticResolver for unknown names.
	ErrNoSuchHost = errors.New("scope: no such host")
	// ErrResolution reports a failed or empty name resolution. Check turns
	// it into deny("resolution_failed").
	ErrResolution = errors.New("scope: resolution failed")
)
