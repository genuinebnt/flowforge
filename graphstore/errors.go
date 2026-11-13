package graphstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/oam"
)

// Reason codes specific to the graph store. Everything else uses the
// shared reasons of core (VALIDATION_FAILED, NOT_FOUND, INTERNAL, ...).
const (
	// ReasonTraversalLimit reports a path search that settled more than
	// MaxPathNodes entities without reaching its target.
	ReasonTraversalLimit = "TRAVERSAL_LIMIT_EXCEEDED"
	// ReasonCancelled reports an operation stopped by its context.
	ReasonCancelled = "CANCELLED"
)

// InvalidArgument returns an INVALID_ARGUMENT error with reason
// VALIDATION_FAILED; field, when set, is reported in Meta["field"].
func InvalidArgument(field, format string, args ...any) error {
	e := core.Errorf(core.KindInvalidArgument, core.ReasonValidationFailed, format, args...)
	if field != "" {
		e.Meta = map[string]string{"field": field}
	}
	return e
}

// NotFound returns a NOT_FOUND error for an entity, edge, snapshot or
// other object. Ids of another tenant are reported exactly like ids that
// do not exist.
func NotFound(what, id string) error {
	return core.Errorf(core.KindNotFound, core.ReasonNotFound, "%s %s not found", what, id)
}

// Classify maps an error from a lower layer to the *core.Error the store
// returns at its boundary: oam validation failures become INVALID_ARGUMENT
// (VALIDATION_FAILED, the failing path in Meta["field"]), a full buffer of
// pending references is RESOURCE_EXHAUSTED (QUOTA_EXCEEDED), context errors
// become DEADLINE_EXCEEDED or ABORTED, *core.Error values pass through and
// everything else is INTERNAL. The original error stays reachable with
// errors.Is / errors.As.
func Classify(err error) error {
	if err == nil {
		return nil
	}
	var ce *core.Error
	if errors.As(err, &ce) {
		return err
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return core.Wrap(err, core.KindDeadlineExceeded, core.ReasonDeadlineExceeded)
	case errors.Is(err, context.Canceled):
		return core.Wrap(err, core.KindAborted, ReasonCancelled)
	case errors.Is(err, oam.ErrPendingLimit):
		return core.Wrap(err, core.KindResourceExhausted, core.ReasonQuotaExceeded)
	case isOAMInput(err):
		e := core.Wrap(err, core.KindInvalidArgument, core.ReasonValidationFailed)
		var ve *oam.ValidationError
		if errors.As(err, &ve) {
			e.Meta = map[string]string{"field": ve.Path}
		}
		return e
	}
	return core.Wrap(err, core.KindInternal, core.ReasonInternal)
}

// Internal wraps err as INTERNAL with a short description of the step.
func Internal(step string, err error) error {
	if err == nil {
		return nil
	}
	var ce *core.Error
	if errors.As(err, &ce) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return Classify(err)
	}
	return core.Wrap(fmt.Errorf("graphstore: %s: %w", step, err), core.KindInternal, core.ReasonInternal)
}

func isOAMInput(err error) bool {
	for _, s := range []error{
		oam.ErrInvalidObservation, oam.ErrMalformed, oam.ErrUnknownField, oam.ErrDuplicateField,
		oam.ErrInvalidAsset, oam.ErrInvalidRelation, oam.ErrUnknownRelationType, oam.ErrInvalidProperty,
		oam.ErrUnknownPropertyType, oam.ErrLabelEndpoints, oam.ErrBatchTooLarge, oam.ErrUnresolvedRef,
		oam.ErrUnknownEntity, oam.ErrInvalidRun,
	} {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}
