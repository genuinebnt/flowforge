package core

import (
	"errors"
	"fmt"
	"time"
)

// Kind is the transport-neutral error category. Services map it to gRPC
// codes and HTTP statuses (spec 11.8); modules never do.
type Kind int

// Error kinds, mirroring the gRPC codes used in spec 11.8. KindUnknown is
// the zero value and also what KindOf reports for errors that are not an
// *Error; services map it to INTERNAL.
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

// String returns the lower snake case kind name ("not_found", ...), or
// "kind(N)" for a value outside the enum.
func (k Kind) String() string {
	if k >= 0 && int(k) < len(kindNames) {
		return kindNames[k]
	}
	return fmt.Sprintf("kind(%d)", int(k))
}

// Stable error reason codes (spec 11.8, 12.2). They appear verbatim in
// google.rpc.ErrorInfo.reason and, lower-cased, as the REST error "code";
// clients match on them, so values never change once published.
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

// Error is the structured error that crosses service boundaries (spec
// 11.8). Modules define their own sentinel errors; services wrap them in an
// *Error with a kind and reason before mapping to the wire.
type Error struct {
	Kind    Kind
	Reason  string            // stable reason code, e.g. SCOPE_DENIED
	Message string            // human readable
	Meta    map[string]string // free-form details (ErrorInfo metadata)
	Details []any             // structured details, e.g. diagnostics
	// RetryAfter is the retry hint for RESOURCE_EXHAUSTED errors, sent as
	// google.rpc.RetryInfo; zero means "use the service default". Other
	// kinds ignore it on the gRPC path.
	RetryAfter time.Duration
	// Err is the wrapped cause, if any.
	Err error
}

// Errorf builds an *Error whose Message is fmt.Sprintf(format, args...).
// It wraps nothing, even when args contain an error (use Wrap for that).
func Errorf(kind Kind, reason, format string, args ...any) *Error {
	return &Error{Kind: kind, Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// Wrap builds an *Error around cause, using cause's text as the Message
// and keeping cause reachable through errors.Is/As. cause must be non-nil.
func Wrap(cause error, kind Kind, reason string) *Error {
	return &Error{Kind: kind, Reason: reason, Message: cause.Error(), Err: cause}
}

// Error returns "REASON: message", or just the reason when Message is
// empty.
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
// errors.Is(err, core.ErrNotFound) for any NOT_FOUND error whatever its
// reason; Meta, Message and the wrapped cause are not compared.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	return t.Kind == e.Kind && (t.Reason == "" || t.Reason == e.Reason)
}

// Sentinel errors for errors.Is checks on the common kinds. They carry no
// reason, so each matches every *Error of its kind (see Error.Is). They
// are compared by kind, never by pointer, and must not be mutated.
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

// KindOf returns the kind of the first *Error in err's chain, or
// KindUnknown when there is none (including err == nil).
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindUnknown
}
