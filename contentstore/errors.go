package contentstore

import (
	"errors"
	"fmt"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// Stable error reasons of this package (README "Errors"). Each error the
// store returns is a *core.Error carrying one of them; services map the
// kind to gRPC and HTTP (spec 11.8).
const (
	// ReasonQuotaExceeded: a per-tenant quota (captures per day, stored
	// bytes, index bytes) refused the request. Kind ResourceExhausted;
	// Meta["quota"] names the quota; RetryAfter is set for the daily one.
	ReasonQuotaExceeded = core.ReasonQuotaExceeded
	// ReasonOutOfOrder: a capture older than its key's open version that
	// is not a re-send of a stored one. Kind FailedPrecondition.
	ReasonOutOfOrder = "CAPTURE_OUT_OF_ORDER"
	// ReasonIntegrity: stored bytes failed verification (a restored body
	// whose hash differs, an undecodable record). Kind Internal.
	ReasonIntegrity = "CONTENT_INTEGRITY"
)

// Quota names (Meta["quota"] of a QUOTA_EXCEEDED error).
const (
	QuotaCapturesPerDay = "captures_per_day"
	QuotaStoredBytes    = "stored_bytes"
	QuotaIndexBytes     = "index_bytes"
)

// Sentinels for errors.Is.
var (
	ErrNotFound      = &core.Error{Kind: core.KindNotFound, Reason: core.ReasonNotFound}
	ErrInvalid       = &core.Error{Kind: core.KindInvalidArgument, Reason: core.ReasonValidationFailed}
	ErrQuotaExceeded = &core.Error{Kind: core.KindResourceExhausted, Reason: ReasonQuotaExceeded}
	ErrOutOfOrder    = &core.Error{Kind: core.KindFailedPrecondition, Reason: ReasonOutOfOrder}
	ErrIntegrity     = &core.Error{Kind: core.KindInternal, Reason: ReasonIntegrity}
	// ErrConflict is a concurrent write to the same key; Store retries it
	// internally and returns it only after MaxRetries attempts.
	ErrConflict = &core.Error{Kind: core.KindAborted, Reason: core.ReasonVersionConflict}
)

func notFound(what string, args ...any) error {
	return &core.Error{Kind: core.KindNotFound, Reason: core.ReasonNotFound, Message: fmt.Sprintf(what, args...)}
}

func invalid(format string, args ...any) error {
	return &core.Error{Kind: core.KindInvalidArgument, Reason: core.ReasonValidationFailed, Message: fmt.Sprintf(format, args...)}
}

func integrity(format string, args ...any) error {
	return &core.Error{Kind: core.KindInternal, Reason: ReasonIntegrity, Message: fmt.Sprintf(format, args...)}
}

func conflict(format string, args ...any) error {
	return &core.Error{Kind: core.KindAborted, Reason: core.ReasonVersionConflict, Message: fmt.Sprintf(format, args...)}
}

// quotaExceeded builds the QUOTA_EXCEEDED error of quota; retry is the
// time until the quota can admit the request again (0: unknown).
func quotaExceeded(quota string, used, limit int64, retry time.Duration) error {
	return &core.Error{Kind: core.KindResourceExhausted, Reason: ReasonQuotaExceeded,
		Message:    fmt.Sprintf("quota %s exceeded (%d of %d)", quota, used, limit),
		Meta:       map[string]string{"quota": quota, "used": fmt.Sprint(used), "limit": fmt.Sprint(limit)},
		RetryAfter: retry}
}

// internal wraps an adapter or chunk-store failure.
func internal(op string, err error) error {
	var ce *core.Error
	if errors.As(err, &ce) {
		return err
	}
	return &core.Error{Kind: core.KindInternal, Reason: core.ReasonInternal, Message: "contentstore: " + op + ": " + err.Error(), Err: err}
}
