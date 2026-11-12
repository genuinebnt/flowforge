package core

import (
	"errors"
	"fmt"
	"time"
)

// Kind is the transport-neutral error category. Services map it to gRPC
// codes and HTTP statuses (spec 11.8); modules never do.
type Kind int

// Error kinds, mirroring the gRPC codes used in spec 11.8.
const (
	KindUnknown Kind = iota
	KindInvalidArgument
	KindUnauthenticated
	KindPermissionDenied
	KindNotFound
	KindAlreadyExists
	KindAborted
	KindFailedPrecondition
	KindResourceExhausted
	KindUnavailable
	KindDeadlineExceeded
	KindInternal
)

var kindNames = [...]string{
	"unknown", "invalid_argument", "unauthenticated", "permission_denied",
	"not_found", "already_exists", "aborted", "failed_precondition",
	"resource_exhausted", "unavailable", "deadline_exceeded", "internal",
}

func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return fmt.Sprintf("kind(%d)", int(k))
}

// Stable error reason codes (spec 11.8).
const (
	ReasonValidationFailed       = "VALIDATION_FAILED"
	ReasonInvalidTarget          = "INVALID_TARGET"
	ReasonDSLDiagnostics         = "DSL_DIAGNOSTICS"
	ReasonTokenMissing           = "TOKEN_MISSING"
	ReasonTokenExpired           = "TOKEN_EXPIRED"
	ReasonTokenInvalid           = "TOKEN_INVALID"
	ReasonForbiddenRole          = "FORBIDDEN_ROLE"
	ReasonScopeDenied            = "SCOPE_DENIED"
	ReasonSelfApprovalForbidden  = "SELF_APPROVAL_FORBIDDEN"
	ReasonKillSwitchActive       = "KILL_SWITCH_ACTIVE"
	ReasonNotFound               = "NOT_FOUND"
	ReasonAlreadyExists          = "ALREADY_EXISTS"
	ReasonIdempotencyKeyReused   = "IDEMPOTENCY_KEY_REUSED"
	ReasonVersionConflict        = "VERSION_CONFLICT"
	ReasonFenceLost              = "FENCE_LOST"
	ReasonApprovalAlreadyDecided = "APPROVAL_ALREADY_DECIDED"
	ReasonRunNotCancellable      = "RUN_NOT_CANCELLABLE"
	ReasonWorkflowDeprecated     = "WORKFLOW_DEPRECATED"
	ReasonProgramPaused          = "PROGRAM_PAUSED"
	ReasonBudgetExceeded         = "BUDGET_EXCEEDED"
	ReasonOutsideWindow          = "OUTSIDE_WINDOW"
	ReasonRateLimited            = "RATE_LIMITED"
	ReasonQuotaExceeded          = "QUOTA_EXCEEDED"
	ReasonUnavailable            = "UNAVAILABLE"
	ReasonDeadlineExceeded       = "DEADLINE_EXCEEDED"
	ReasonInternal               = "INTERNAL"
)

// Error is the structured error that crosses service boundaries. Modules
// define their own sentinel errors; services wrap them in an *Error with a
// kind and reason before mapping to the wire.
type Error struct {
	Kind    Kind
	Reason  string            // stable reason code, e.g. SCOPE_DENIED
	Message string            // human readable
	Meta    map[string]string // free-form details (ErrorInfo metadata)
	Details []any             // structured details, e.g. diagnostics
	// RetryAfter is set for RESOURCE_EXHAUSTED errors.
	RetryAfter time.Duration
	// Err is the wrapped cause, if any.
	Err error
}

// Errorf builds an *Error.
func Errorf(kind Kind, reason, format string, args ...any) *Error {
	return &Error{Kind: kind, Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// Wrap builds an *Error around cause.
func Wrap(cause error, kind Kind, reason string) *Error {
	return &Error{Kind: kind, Reason: reason, Message: cause.Error(), Err: cause}
}

func (e *Error) Error() string {
	if e.Message == "" {
		return e.Reason
	}
	return e.Reason + ": " + e.Message
}

// Unwrap returns the wrapped cause.
func (e *Error) Unwrap() error { return e.Err }

// Is reports whether target is an *Error with the same kind and, if the
// target names one, the same reason. This lets callers write
// errors.Is(err, core.ErrNotFound).
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	return t.Kind == e.Kind && (t.Reason == "" || t.Reason == e.Reason)
}

// Sentinel errors for errors.Is checks on the common kinds.
var (
	ErrInvalidArgument  = &Error{Kind: KindInvalidArgument}
	ErrUnauthenticated  = &Error{Kind: KindUnauthenticated}
	ErrPermissionDenied = &Error{Kind: KindPermissionDenied}
	ErrNotFound         = &Error{Kind: KindNotFound}
	ErrAlreadyExists    = &Error{Kind: KindAlreadyExists}
	ErrAborted          = &Error{Kind: KindAborted}
	ErrPrecondition     = &Error{Kind: KindFailedPrecondition}
	ErrExhausted        = &Error{Kind: KindResourceExhausted}
	ErrUnavailable      = &Error{Kind: KindUnavailable}
	ErrInternal         = &Error{Kind: KindInternal}
)

// KindOf returns the kind of err, or KindUnknown when err carries none.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindUnknown
}
