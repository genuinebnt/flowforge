package auth

import (
	"errors"
	"fmt"
	"time"
)

// rejection is a sentinel with a stable reason code that also matches a
// base error, so callers can test either errors.Is(err, ErrExpired) or
// errors.Is(err, ErrInvalidToken).
type rejection struct {
	code string
	msg  string
	base error
}

func (e *rejection) Error() string { return "auth: " + e.msg }

// Is reports whether target is the rejection's base error.
func (e *rejection) Is(target error) bool { return target == e.base }

// RejectionReason returns the stable reason code of a token or refresh
// rejection in err's chain ("expired", "alg_none", "refresh_reused", ...),
// or "" when err carries none.
func RejectionReason(err error) string {
	var r *rejection
	if errors.As(err, &r) {
		return r.code
	}
	return ""
}

// ErrInvalidToken is matched by every access/internal token rejection.
var ErrInvalidToken = errors.New("auth: invalid token")

func tokenRejection(code, msg string) error {
	return &rejection{code: code, msg: msg, base: ErrInvalidToken}
}

// Token rejections, one per reason (auth_002). Each matches ErrInvalidToken.
var (
	ErrMalformed       = tokenRejection("malformed", "malformed token")
	ErrAlgNone         = tokenRejection("alg_none", `token alg "none" is never accepted`)
	ErrAlgNotAllowed   = tokenRejection("alg_not_allowed", "token alg is not EdDSA")
	ErrTypInvalid      = tokenRejection("typ_invalid", `token typ is not "JWT"`)
	ErrCritUnsupported = tokenRejection("crit_unsupported", "token has critical header parameters")
	ErrKidMissing      = tokenRejection("kid_missing", "token header has no kid")
	ErrUnknownKid      = tokenRejection("unknown_kid", "token kid is not in the key set")
	ErrKeyRetired      = tokenRejection("key_retired", "token was signed with a retired key")
	ErrBadSignature    = tokenRejection("bad_signature", "token signature is invalid")
	ErrIssuer          = tokenRejection("wrong_iss", "token issuer is not accepted")
	ErrAudience        = tokenRejection("wrong_aud", "token audience is not accepted")
	ErrExpired         = tokenRejection("expired", "token has expired")
	ErrNotYetValid     = tokenRejection("not_yet_valid", "token is not valid yet (nbf)")
	ErrIssuedInFuture  = tokenRejection("iat_future", "token was issued in the future (iat)")
	ErrLifetimeTooLong = tokenRejection("lifetime_too_long", "token lifetime exceeds the maximum")
	ErrClaimMissing    = tokenRejection("claim_missing", "token lacks a required claim")
	ErrClaimInvalid    = tokenRejection("claim_invalid", "token has an invalid claim")
	ErrRevoked         = tokenRejection("revoked", "token has been revoked (jti)")
	ErrSessionRevoked  = tokenRejection("session_revoked", "token session version has been revoked (sv)")
)

// ErrRevocationUnavailable means the revocation state could not be read;
// the token is rejected (fail closed) but services should report it as
// unavailable, not as a bad token.
var ErrRevocationUnavailable = errors.New("auth: revocation state unavailable")

// ErrInvalidRefresh is matched by every refresh-token rejection.
var ErrInvalidRefresh = errors.New("auth: invalid refresh token")

func refreshRejection(code, msg string) error {
	return &rejection{code: code, msg: msg, base: ErrInvalidRefresh}
}

// Refresh rejections (spec 13.4). Each matches ErrInvalidRefresh.
var (
	ErrRefreshUnknown = refreshRejection("refresh_unknown", "refresh token not found")
	ErrRefreshExpired = refreshRejection("refresh_expired", "refresh token has expired")
	ErrRefreshRevoked = refreshRejection("refresh_revoked", "refresh token has been revoked")
	ErrRefreshReused  = refreshRejection("refresh_reused", "refresh token reuse detected; session revoked")
)

// Password and hashing errors.
var (
	// ErrPasswordPolicy is matched by every policy violation.
	ErrPasswordPolicy   = errors.New("auth: password rejected by policy")
	ErrPasswordTooShort = fmt.Errorf("%w: shorter than %d characters", ErrPasswordPolicy, MinPasswordLength)
	ErrPasswordTooLong  = fmt.Errorf("%w: longer than %d characters", ErrPasswordPolicy, MaxPasswordLength)
	ErrPasswordEncoding = fmt.Errorf("%w: not valid UTF-8", ErrPasswordPolicy)
	ErrPasswordBreached = fmt.Errorf("%w: found in a breached-password list", ErrPasswordPolicy)

	// ErrInvalidHash means a stored password hash is not a supported PHC
	// string.
	ErrInvalidHash = errors.New("auth: invalid password hash")
	// ErrWeakParams means Argon2id parameters are below the spec 13.1 floor.
	ErrWeakParams = errors.New("auth: argon2id parameters below policy")
)

// Login errors.
var (
	// ErrInvalidCredentials is the single answer for an unknown email and a
	// wrong password (no account enumeration).
	ErrInvalidCredentials = errors.New("auth: invalid email or password")
	// ErrThrottled is matched by *ThrottledError.
	ErrThrottled = errors.New("auth: too many attempts")
	// ErrPendingUser refuses login before email verification.
	ErrPendingUser = errors.New("auth: email address not verified")
	// ErrUserDisabled refuses login for a disabled user.
	ErrUserDisabled = errors.New("auth: user disabled")
	// ErrUserNotFound is returned by UserStore.FindByEmail.
	ErrUserNotFound = errors.New("auth: user not found")
)

// ThrottledError reports a throttle or cool-down with the time to wait.
type ThrottledError struct {
	RetryAfter time.Duration
}

func (e *ThrottledError) Error() string {
	return fmt.Sprintf("auth: too many attempts; retry after %s", e.RetryAfter)
}

// Is matches ErrThrottled.
func (e *ThrottledError) Is(target error) bool { return target == ErrThrottled }

// Key, opaque-token and store errors.
var (
	ErrInvalidKey   = errors.New("auth: invalid signing key")
	ErrKeyNotActive = errors.New("auth: only an active key may sign")
	ErrInvalidJWKS  = errors.New("auth: invalid JWKS")

	ErrInvalidAPIKey       = errors.New("auth: invalid API key")
	ErrVerificationInvalid = errors.New("auth: email verification token is invalid, used or expired")
	ErrCSRFMissing         = errors.New("auth: CSRF token missing")
	ErrCSRFMismatch        = errors.New("auth: CSRF token mismatch")

	// ErrNotFound is returned by store ports for a missing row.
	ErrNotFound = errors.New("auth: not found")
	// ErrConfig reports an invalid service configuration.
	ErrConfig = errors.New("auth: invalid configuration")
)
