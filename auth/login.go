package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// UserStatus is identity.users.status.
type UserStatus string

// User statuses.
const (
	UserPending  UserStatus = "pending"
	UserActive   UserStatus = "active"
	UserDisabled UserStatus = "disabled"
)

// User is the part of identity.users that login needs.
type User struct {
	ID             core.UUID
	Email          string
	PasswordHash   string // argon2id PHC string
	Status         UserStatus
	SessionVersion int64
}

// UserStore is the user lookup port of LoginService.
type UserStore interface {
	// FindByEmail returns the user with the (normalized) email, or
	// ErrUserNotFound.
	FindByEmail(ctx context.Context, email string) (User, error)
	// SetPasswordHash replaces the stored hash (re-hash on login).
	SetPasswordHash(ctx context.Context, id core.UUID, hash string) error
}

// Limit is a throttle budget: Count attempts per Per.
type Limit struct {
	Count int
	Per   time.Duration
}

// Throttle is the rate-limiting port (implemented over the limiter package
// by the service). Allow consumes one attempt from key's budget and
// reports whether it was admitted and, if not, how long to wait.
type Throttle interface {
	Allow(ctx context.Context, key string, limit Limit) (ok bool, retryAfter time.Duration, err error)
}

// Lockout is the failure state of one account key.
type Lockout struct {
	Failures    int       // failures since the last success or cool-down
	LockedUntil time.Time // end of the active cool-down, zero if none
}

// LockoutStore tracks login failures per account key (spec 13.1: after 10
// failures for an email, a 15-minute cool-down).
type LockoutStore interface {
	// Get returns the state of key (zero value when unknown).
	Get(ctx context.Context, key string) (Lockout, error)
	// RecordFailure atomically counts a failure at now. When the count
	// reaches threshold it starts a cool-down until now+cooldown and resets
	// the count. It returns the new state.
	RecordFailure(ctx context.Context, key string, now time.Time, threshold int, cooldown time.Duration) (Lockout, error)
	// Reset clears key after a successful login.
	Reset(ctx context.Context, key string) error
}

// PasswordHasher is the hashing dependency of LoginService; *Hasher
// implements it.
type PasswordHasher interface {
	Hash(password string) (string, error)
	Verify(password, encoded string) (bool, error)
	NeedsRehash(encoded string) bool
}

var _ PasswordHasher = (*Hasher)(nil)

// DefaultLoginIPLimit is the login budget per IP (spec 13.1): 20 per 10
// minutes.
func DefaultLoginIPLimit() Limit { return Limit{Count: 20, Per: 10 * time.Minute} }

// DefaultLoginEmailLimit is the login budget per email (spec 13.1): 10 per
// 10 minutes.
func DefaultLoginEmailLimit() Limit { return Limit{Count: 10, Per: 10 * time.Minute} }

// Lockout defaults (spec 13.1).
const (
	DefaultMaxFailures = 10
	DefaultCooldown    = 15 * time.Minute
)

// LoginConfig configures a LoginService. Zero limits take the defaults.
type LoginConfig struct {
	Users       UserStore
	Throttle    Throttle
	Lockouts    LockoutStore
	Passwords   PasswordHasher
	Clock       core.Clock
	IPLimit     Limit
	EmailLimit  Limit
	MaxFailures int
	Cooldown    time.Duration
}

// LoginService checks credentials (spec 13.1). It answers an unknown email
// exactly like a wrong password, with the same error and the same Argon2
// work, applies the per-IP and per-email throttles and the failure
// cool-down to known and unknown emails alike, and refuses pending and
// disabled users only after the password has been verified, so neither
// state leaks to someone without the password. Token issuance is the
// caller's job.
type LoginService struct {
	cfg   LoginConfig
	dummy string // PHC hash verified for unknown emails
}

// NewLoginService checks cfg and returns a LoginService.
func NewLoginService(cfg LoginConfig) (*LoginService, error) {
	if cfg.Users == nil || cfg.Throttle == nil || cfg.Lockouts == nil || cfg.Passwords == nil || cfg.Clock == nil {
		return nil, fmt.Errorf("%w: login service needs users, throttle, lockouts, passwords and clock", ErrConfig)
	}
	if cfg.IPLimit.Count <= 0 || cfg.IPLimit.Per <= 0 {
		cfg.IPLimit = DefaultLoginIPLimit()
	}
	if cfg.EmailLimit.Count <= 0 || cfg.EmailLimit.Per <= 0 {
		cfg.EmailLimit = DefaultLoginEmailLimit()
	}
	if cfg.MaxFailures <= 0 {
		cfg.MaxFailures = DefaultMaxFailures
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = DefaultCooldown
	}
	dummy, err := cfg.Passwords.Hash("flowforge dummy password for unknown accounts")
	if err != nil {
		return nil, fmt.Errorf("auth: precompute dummy hash: %w", err)
	}
	return &LoginService{cfg: cfg, dummy: dummy}, nil
}

// NormalizeEmail lower-cases and trims an email address, matching the
// citext comparison of identity.users.email.
//
// PORT: strings.TrimSpace trims Unicode White_Space and strings.ToLower
// maps each rune with Unicode simple case mapping (U+0130 becomes "i");
// Rust's str::to_lowercase uses full mappings (U+0130 becomes "i\u0307")
// and context-sensitive final sigma, so map per char with the simple
// mapping to get the same account keys.
func NormalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// IPThrottleKey is the limiter key rl:auth:ip:<ip>:<endpoint> (spec 14.3).
// Unlike limiter.AuthIPKey (which auth may not import, both being layer
// 1) it does not escape ':' in the components, so for an IPv6 client the
// login key and the signup key built by limiter.AuthIPKey spell the
// address differently. Each key is self-consistent; a port must reproduce
// both spellings exactly if it shares the limiter store.
func IPThrottleKey(ip, endpoint string) string { return "rl:auth:ip:" + ip + ":" + endpoint }

// AccountThrottleKey is the limiter key rl:auth:acct:<sha256 email> (spec
// 14.3); the email is normalized and only its lowercase hex SHA-256
// appears in the key. It equals limiter.AuthAccountKey for every input.
func AccountThrottleKey(email string) string {
	sum := sha256.Sum256([]byte(NormalizeEmail(email)))
	return "rl:auth:acct:" + hex.EncodeToString(sum[:])
}

// LoginRequest is a login attempt.
type LoginRequest struct {
	Email    string
	Password string
	IP       string
}

// LoginResult is a successful login.
type LoginResult struct {
	User User
	// Rehashed reports that the stored hash was upgraded to the current
	// parameters. A failed upgrade does not fail the login; it is retried
	// on the next one.
	Rehashed bool
}

// Login checks req. It returns ErrInvalidCredentials for an unknown email
// or a wrong password, a *ThrottledError (matching ErrThrottled) when a
// throttle or the cool-down applies, ErrPendingUser or ErrUserDisabled for
// a correct password on an account that may not log in, or a wrapped
// store error.
func (s *LoginService) Login(ctx context.Context, req LoginRequest) (LoginResult, error) {
	email := NormalizeEmail(req.Email)
	acct := AccountThrottleKey(email)
	if err := s.allow(ctx, IPThrottleKey(req.IP, "login"), s.cfg.IPLimit); err != nil {
		return LoginResult{}, err
	}
	if err := s.allow(ctx, acct, s.cfg.EmailLimit); err != nil {
		return LoginResult{}, err
	}
	now := s.cfg.Clock.Now()
	lock, err := s.cfg.Lockouts.Get(ctx, acct)
	if err != nil {
		return LoginResult{}, fmt.Errorf("auth: read lockout: %w", err)
	}
	if now.Before(lock.LockedUntil) {
		return LoginResult{}, &ThrottledError{RetryAfter: lock.LockedUntil.Sub(now)}
	}

	user, err := s.cfg.Users.FindByEmail(ctx, email)
	known := err == nil
	if err != nil && !errors.Is(err, ErrUserNotFound) {
		return LoginResult{}, fmt.Errorf("auth: find user: %w", err)
	}
	hash := s.dummy
	if known {
		hash = user.PasswordHash
	}
	ok, verr := s.cfg.Passwords.Verify(req.Password, hash)
	if !known || !ok || verr != nil {
		if _, err := s.cfg.Lockouts.RecordFailure(ctx, acct, now, s.cfg.MaxFailures, s.cfg.Cooldown); err != nil {
			return LoginResult{}, fmt.Errorf("auth: record failure: %w", err)
		}
		return LoginResult{}, ErrInvalidCredentials
	}

	switch user.Status {
	case UserActive:
	case UserPending:
		return LoginResult{}, ErrPendingUser
	default:
		return LoginResult{}, ErrUserDisabled
	}
	if err := s.cfg.Lockouts.Reset(ctx, acct); err != nil {
		return LoginResult{}, fmt.Errorf("auth: reset lockout: %w", err)
	}
	res := LoginResult{User: user}
	if s.cfg.Passwords.NeedsRehash(user.PasswordHash) {
		if h, err := s.cfg.Passwords.Hash(req.Password); err == nil {
			if s.cfg.Users.SetPasswordHash(ctx, user.ID, h) == nil {
				res.User.PasswordHash, res.Rehashed = h, true
			}
		}
	}
	return res, nil
}

func (s *LoginService) allow(ctx context.Context, key string, l Limit) error {
	ok, retry, err := s.cfg.Throttle.Allow(ctx, key, l)
	if err != nil {
		return fmt.Errorf("auth: throttle: %w", err)
	}
	if !ok {
		return &ThrottledError{RetryAfter: retry}
	}
	return nil
}
