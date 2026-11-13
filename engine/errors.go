package engine

import (
	"errors"
	"fmt"

	"github.com/genuinebnt/flowforge/core"
)

// Sentinel errors returned by ports and by the worker. API methods wrap
// every failure in a *core.Error with a stable reason (spec 11.8); these
// sentinels are for ports and internal control flow.
var (
	// ErrNotFound is returned by Repo, Scope and ToolRegistry
	// implementations for a missing (or another tenant's) resource.
	ErrNotFound = errors.New("engine: not found")
	// ErrConflict is returned by Repo implementations when a uniqueness
	// constraint is violated (for example a second run with the same
	// tenant and idempotency key).
	ErrConflict = errors.New("engine: conflict")
	// ErrLeaseLost reports that the worker lost the lease of the run it
	// was advancing (an append failed with eventstore.ErrFenced). The
	// worker stops advancing that run immediately (spec 9.4).
	ErrLeaseLost = errors.New("engine: lease lost")
	// ErrInvalidToolSpec is returned by ToolSpec.Validate for a retry
	// policy or idempotency class the engine cannot honour.
	ErrInvalidToolSpec = errors.New("engine: invalid tool spec")
)

func apiErr(kind core.Kind, reason, format string, args ...any) *core.Error {
	return core.Errorf(kind, reason, format, args...)
}

func errInvalid(format string, args ...any) *core.Error {
	return apiErr(core.KindInvalidArgument, core.ReasonValidationFailed, format, args...)
}

func errNotFound(what string) *core.Error {
	return apiErr(core.KindNotFound, core.ReasonNotFound, "%s not found", what)
}

func errPrecondition(reason, format string, args ...any) *core.Error {
	return apiErr(core.KindFailedPrecondition, reason, format, args...)
}

func errDenied(reason, format string, args ...any) *core.Error {
	return apiErr(core.KindPermissionDenied, reason, format, args...)
}

// errInternal wraps an unexpected failure. Port errors that already are
// *core.Error pass through unchanged so their kind is not lost.
func errInternal(op string, err error) error {
	var ce *core.Error
	if errors.As(err, &ce) {
		return err
	}
	if errors.Is(err, ErrNotFound) {
		return apiErr(core.KindNotFound, core.ReasonNotFound, "%s: not found", op)
	}
	return &core.Error{Kind: core.KindInternal, Reason: core.ReasonInternal, Message: fmt.Sprintf("%s: %v", op, err), Err: err}
}

// notFoundOr maps ErrNotFound to a NOT_FOUND API error and anything else
// to an internal error.
func notFoundOr(what, op string, err error) error {
	if errors.Is(err, ErrNotFound) {
		return errNotFound(what)
	}
	return errInternal(op, err)
}
