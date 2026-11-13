package platform

import (
	"context"
	"errors"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/protoadapt"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/genuinebnt/flowforge/core"
)

// DefaultRetryAfter is the RetryInfo delay attached to RESOURCE_EXHAUSTED
// errors that carry none (spec 11.8: RetryInfo is always set).
const DefaultRetryAfter = time.Second

var kindCodes = map[core.Kind]codes.Code{
	core.KindInvalidArgument:    codes.InvalidArgument,
	core.KindUnauthenticated:    codes.Unauthenticated,
	core.KindPermissionDenied:   codes.PermissionDenied,
	core.KindNotFound:           codes.NotFound,
	core.KindAlreadyExists:      codes.AlreadyExists,
	core.KindAborted:            codes.Aborted,
	core.KindFailedPrecondition: codes.FailedPrecondition,
	core.KindResourceExhausted:  codes.ResourceExhausted,
	core.KindUnavailable:        codes.Unavailable,
	core.KindDeadlineExceeded:   codes.DeadlineExceeded,
	core.KindInternal:           codes.Internal,
}

var defaultReasons = map[codes.Code]string{
	codes.InvalidArgument:    core.ReasonValidationFailed,
	codes.Unauthenticated:    core.ReasonTokenInvalid,
	codes.PermissionDenied:   core.ReasonForbiddenRole,
	codes.NotFound:           core.ReasonNotFound,
	codes.AlreadyExists:      core.ReasonAlreadyExists,
	codes.Aborted:            core.ReasonVersionConflict,
	codes.FailedPrecondition: core.ReasonValidationFailed,
	codes.ResourceExhausted:  core.ReasonRateLimited,
	codes.Unavailable:        core.ReasonUnavailable,
	codes.DeadlineExceeded:   core.ReasonDeadlineExceeded,
	codes.Internal:           core.ReasonInternal,
	codes.Canceled:           core.ReasonUnavailable,
}

// ToStatus maps an error returned by a handler to a gRPC status error with
// a google.rpc.ErrorInfo detail (spec 11.8). *core.Error values keep their
// kind, reason, message and metadata; proto.Message values in Details are
// attached as further details (diagnostics); RESOURCE_EXHAUSTED always
// carries a google.rpc.RetryInfo. Context errors map to DEADLINE_EXCEEDED
// and CANCELLED. Anything else is INTERNAL and reveals only the request id.
// Errors that already are gRPC statuses pass through unchanged.
//
// PORT: the ErrorInfo reason and domain, the request_id metadata key and
// RetryInfo on every RESOURCE_EXHAUSTED are what clients of either
// implementation match on (docs/PORTING_RUST.md 5.17); map tonic::Status
// the same way.
func ToStatus(err error, domain, requestID string) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	var ce *core.Error
	switch {
	case errors.As(err, &ce):
	case errors.Is(err, context.DeadlineExceeded):
		ce = &core.Error{Kind: core.KindDeadlineExceeded, Reason: core.ReasonDeadlineExceeded, Message: "deadline exceeded"}
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "cancelled")
	default:
		ce = &core.Error{Kind: core.KindInternal, Reason: core.ReasonInternal, Message: "internal error"}
	}
	code, ok := kindCodes[ce.Kind]
	if !ok {
		code = codes.Internal
	}
	reason := ce.Reason
	if reason == "" {
		reason = defaultReasons[code]
	}
	msg := ce.Message
	if msg == "" {
		msg = reason
	}
	meta := map[string]string{}
	if code != codes.Internal {
		for k, v := range ce.Meta {
			meta[k] = v
		}
	} else {
		msg = "internal error"
	}
	if requestID != "" {
		meta["request_id"] = requestID
	}
	st := status.New(code, msg)
	details := []proto.Message{&errdetails.ErrorInfo{Reason: reason, Domain: domain, Metadata: meta}}
	if code == codes.ResourceExhausted {
		d := ce.RetryAfter
		if d <= 0 {
			d = DefaultRetryAfter
		}
		details = append(details, &errdetails.RetryInfo{RetryDelay: durationpb.New(d)})
	}
	if code != codes.Internal {
		for _, d := range ce.Details {
			if m, ok := d.(proto.Message); ok {
				details = append(details, m)
			}
		}
	}
	protoDetails := make([]protoadapt.MessageV1, 0, len(details))
	for _, d := range details {
		protoDetails = append(protoDetails, protoadapt.MessageV1Of(d))
	}
	if withDetails, derr := st.WithDetails(protoDetails...); derr == nil {
		st = withDetails
	}
	return st.Err()
}

var codeKinds = map[codes.Code]core.Kind{
	codes.InvalidArgument:    core.KindInvalidArgument,
	codes.Unauthenticated:    core.KindUnauthenticated,
	codes.PermissionDenied:   core.KindPermissionDenied,
	codes.NotFound:           core.KindNotFound,
	codes.AlreadyExists:      core.KindAlreadyExists,
	codes.Aborted:            core.KindAborted,
	codes.FailedPrecondition: core.KindFailedPrecondition,
	codes.ResourceExhausted:  core.KindResourceExhausted,
	codes.Unavailable:        core.KindUnavailable,
	codes.DeadlineExceeded:   core.KindDeadlineExceeded,
	codes.Internal:           core.KindInternal,
	codes.Canceled:           core.KindUnavailable,
	codes.Unknown:            core.KindInternal,
	codes.Unimplemented:      core.KindInternal,
	codes.DataLoss:           core.KindInternal,
	codes.OutOfRange:         core.KindInvalidArgument,
}

// FromStatus is the client-side reverse mapping: a gRPC status error
// becomes a *core.Error with the kind of its code, the ErrorInfo reason and
// metadata, the RetryInfo delay and any other details (as proto messages).
// Non-status errors are returned unchanged.
func FromStatus(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	kind, ok := codeKinds[st.Code()]
	if !ok {
		kind = core.KindInternal
	}
	ce := &core.Error{Kind: kind, Message: st.Message(), Err: err}
	for _, d := range st.Details() {
		switch v := d.(type) {
		case *errdetails.ErrorInfo:
			ce.Reason = v.GetReason()
			if len(v.GetMetadata()) > 0 {
				ce.Meta = v.GetMetadata()
			}
		case *errdetails.RetryInfo:
			ce.RetryAfter = v.GetRetryDelay().AsDuration()
		case proto.Message:
			ce.Details = append(ce.Details, v)
		}
	}
	if ce.Reason == "" {
		ce.Reason = defaultReasons[st.Code()]
	}
	return ce
}

// Code returns the gRPC code of err (OK for nil).
func Code(err error) codes.Code { return status.Code(err) }
