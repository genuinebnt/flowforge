package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/genuinebnt/flowforge/core"
	flowforgev1 "github.com/genuinebnt/flowforge/gen/flowforge/v1"
)

// Gateway-local reasons (not produced by internal services).
const (
	// ReasonCSRFFailed: a cookie-authenticated state-changing request
	// without a matching X-CSRF-Token (spec 13.5).
	ReasonCSRFFailed = "CSRF_FAILED"
	// ReasonPreconditionFailed: If-Match does not match the current ETag.
	ReasonPreconditionFailed = "PRECONDITION_FAILED"
)

// apiError is an error in the REST error model (spec 12.2).
type apiError struct {
	Status     int
	Reason     string
	Message    string
	Details    []any
	RetryAfter time.Duration
}

func (e *apiError) Error() string { return e.Reason + ": " + e.Message }

func newError(status int, reason, format string, args ...any) *apiError {
	return &apiError{Status: status, Reason: reason, Message: fmt.Sprintf(format, args...)}
}

func errValidation(format string, args ...any) *apiError {
	return newError(http.StatusBadRequest, core.ReasonValidationFailed, format, args...)
}

func errNotFound() *apiError {
	return newError(http.StatusNotFound, core.ReasonNotFound, "resource not found")
}

func errForbiddenRole() *apiError {
	return newError(http.StatusForbidden, core.ReasonForbiddenRole, "your role does not allow this operation")
}

// statusForReason resolves the HTTP status of spec 11.8 for a gRPC code
// and reason.
func statusForReason(code codes.Code, reason string) int {
	switch reason {
	case core.ReasonDSLDiagnostics:
		return http.StatusUnprocessableEntity
	case ReasonPreconditionFailed:
		return http.StatusPreconditionFailed
	}
	switch code {
	case codes.InvalidArgument, codes.OutOfRange:
		return http.StatusBadRequest
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.NotFound:
		return http.StatusNotFound
	case codes.AlreadyExists, codes.Aborted, codes.FailedPrecondition:
		return http.StatusConflict
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests
	case codes.Unavailable, codes.Canceled:
		return http.StatusServiceUnavailable
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	}
	return http.StatusInternalServerError
}

var defaultReason = map[codes.Code]string{
	codes.InvalidArgument:    core.ReasonValidationFailed,
	codes.OutOfRange:         core.ReasonValidationFailed,
	codes.Unauthenticated:    core.ReasonTokenInvalid,
	codes.PermissionDenied:   core.ReasonForbiddenRole,
	codes.NotFound:           core.ReasonNotFound,
	codes.AlreadyExists:      core.ReasonAlreadyExists,
	codes.Aborted:            core.ReasonVersionConflict,
	codes.FailedPrecondition: core.ReasonVersionConflict,
	codes.ResourceExhausted:  core.ReasonRateLimited,
	codes.Unavailable:        core.ReasonUnavailable,
	codes.Canceled:           core.ReasonUnavailable,
	codes.DeadlineExceeded:   core.ReasonDeadlineExceeded,
}

var kindCode = map[core.Kind]codes.Code{
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

func internalError() *apiError {
	return &apiError{Status: http.StatusInternalServerError, Reason: core.ReasonInternal, Message: "internal error"}
}

// toAPIError maps any error to the REST error model: gateway errors pass
// through, *core.Error (identity) and gRPC statuses (internal services)
// map through the spec 11.8 table; everything else is INTERNAL.
func toAPIError(err error) *apiError {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return newError(http.StatusRequestEntityTooLarge, core.ReasonValidationFailed, "request body exceeds %d bytes", mbe.Limit)
	}
	var ce *core.Error
	if errors.As(err, &ce) {
		code, ok := kindCode[ce.Kind]
		if !ok || code == codes.Internal {
			return internalError()
		}
		e := &apiError{Status: statusForReason(code, ce.Reason), Reason: ce.Reason, Message: ce.Message, RetryAfter: ce.RetryAfter}
		e.Details = []any{metaDetail(ce.Reason, ce.Meta)}
		return e
	}
	if st, ok := status.FromError(err); ok && st.Code() != codes.OK {
		return fromStatus(st)
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return newError(http.StatusGatewayTimeout, core.ReasonDeadlineExceeded, "upstream deadline exceeded")
	case errors.Is(err, context.Canceled):
		return newError(http.StatusServiceUnavailable, core.ReasonUnavailable, "request cancelled")
	}
	return internalError()
}

func fromStatus(st *status.Status) *apiError {
	code := st.Code()
	reason := defaultReason[code]
	var meta map[string]string
	var retry time.Duration
	var diags []any
	var violations []any
	for _, d := range st.Details() {
		switch v := d.(type) {
		case *errdetails.ErrorInfo:
			if v.GetReason() != "" {
				reason = v.GetReason()
			}
			meta = v.GetMetadata()
		case *errdetails.RetryInfo:
			retry = v.GetRetryDelay().AsDuration()
		case *flowforgev1.Diagnostic:
			diags = append(diags, diagnosticJSON(v))
		case *errdetails.BadRequest:
			for _, fv := range v.GetFieldViolations() {
				violations = append(violations, map[string]any{
					"reason": core.ReasonValidationFailed, "field": fv.GetField(), "description": fv.GetDescription(),
				})
			}
		}
	}
	if reason == "" || code == codes.Internal || code == codes.Unknown || code == codes.DataLoss || code == codes.Unimplemented {
		return internalError()
	}
	e := &apiError{Status: statusForReason(code, reason), Reason: reason, Message: st.Message(), RetryAfter: retry}
	switch {
	case reason == core.ReasonDSLDiagnostics:
		e.Details = append([]any{}, diags...)
	default:
		e.Details = append([]any{metaDetail(reason, meta)}, violations...)
	}
	if e.Status == http.StatusTooManyRequests && e.RetryAfter <= 0 {
		e.RetryAfter = time.Second
	}
	return e
}

// numericMeta reports metadata keys rendered as JSON numbers (ErrorInfo
// metadata is strings only; spec 12.2 shows scope_version as a number).
func numericMeta(key string) bool {
	for _, suf := range []string{"version", "_seq", "_ms", "count", "_seconds"} {
		if strings.HasSuffix(key, suf) {
			return true
		}
	}
	return false
}

func metaDetail(reason string, meta map[string]string) map[string]any {
	d := map[string]any{"reason": reason}
	for k, v := range meta {
		if k == "reason" {
			continue
		}
		if numericMeta(k) {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				d[k] = n
				continue
			}
		}
		d[k] = v
	}
	return d
}

// errorBody is the JSON of spec 12.2.
type errorBody struct {
	Error errorObject `json:"error"`
}

type errorObject struct {
	Code      string `json:"code"`
	Status    int    `json:"status"`
	Message   string `json:"message"`
	Details   []any  `json:"details"`
	RequestID string `json:"request_id"`
}

func (e *apiError) body(requestID string) errorBody {
	details := e.Details
	if details == nil {
		details = []any{}
		if e.Status < 500 {
			details = []any{map[string]any{"reason": e.Reason}}
		}
	}
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return errorBody{Error: errorObject{
		Code: strings.ToLower(e.Reason), Status: e.Status, Message: msg, Details: details, RequestID: requestID,
	}}
}
