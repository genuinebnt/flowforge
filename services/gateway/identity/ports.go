package identity

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/limiter"
)

// ---- Revocations -------------------------------------------------------

// Revocations is the access-token revocation state of spec 13.3:
// deny:jti:<jti> and sv:<user_id>. It extends auth.RevocationChecker with
// the writes the identity service performs. redisstore.Revocations is the
// shared implementation; MemRevocations serves a single process.
type Revocations interface {
	auth.RevocationChecker
	// DenyJTI revokes one access token until ttl elapses.
	DenyJTI(ctx context.Context, jti string, ttl time.Duration) error
	// SetSessionVersion caches the user's session_version (subject is the
	// usr_... id, so the key is sv:usr_...) for ttl, which must outlive
	// every access token (SessionVersionTTL). It never lowers a cached
	// version: a writer holding a stale value cannot undo a bump that
	// revoked tokens.
	SetSessionVersion(ctx context.Context, subject string, v int64, ttl time.Duration) error
}

// SessionVersionTTL is how long a cached session version is kept: the
// access-token lifetime plus the verifier's clock skew, plus a margin.
const SessionVersionTTL = auth.AccessTokenTTL + auth.DefaultSkew + time.Minute

// MemRevocations is an in-memory Revocations with expiry on an injected
// clock. It is safe for concurrent use.
type MemRevocations struct {
	clock    core.Clock
	mu       sync.Mutex
	denied   map[string]time.Time
	versions map[string]memVersion
}

type memVersion struct {
	v       int64
	expires time.Time
}

var _ Revocations = (*MemRevocations)(nil)

// NewMemRevocations returns an empty revocation state.
func NewMemRevocations(clock core.Clock) *MemRevocations {
	return &MemRevocations{clock: clock, denied: map[string]time.Time{}, versions: map[string]memVersion{}}
}

// IsRevoked implements auth.RevocationChecker.
func (r *MemRevocations) IsRevoked(_ context.Context, jti string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	exp, ok := r.denied[jti]
	if ok && !r.clock.Now().Before(exp) {
		delete(r.denied, jti)
		return false, nil
	}
	return ok, nil
}

// SessionVersion implements auth.RevocationChecker.
func (r *MemRevocations) SessionVersion(_ context.Context, subject string) (int64, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.versions[subject]
	if ok && !r.clock.Now().Before(v.expires) {
		delete(r.versions, subject)
		return 0, false, nil
	}
	return v.v, ok, nil
}

// DenyJTI implements Revocations.
func (r *MemRevocations) DenyJTI(_ context.Context, jti string, ttl time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.denied[jti] = r.clock.Now().Add(ttl)
	return nil
}

// SetSessionVersion implements Revocations.
func (r *MemRevocations) SetSessionVersion(_ context.Context, subject string, v int64, ttl time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.versions[subject]; ok && old.v > v {
		v = old.v
	}
	r.versions[subject] = memVersion{v: v, expires: r.clock.Now().Add(ttl)}
	return nil
}

// ---- E-mail ------------------------------------------------------------

// Email kinds.
const (
	EmailVerify        = "verify_email"
	EmailSignupExists  = "signup_existing"
	EmailPasswordReset = "password_reset"
	EmailInvite        = "invite"
)

// Email is one outgoing message. Token is the secret the message carries
// (verification, reset or invite token), empty for notices.
type Email struct {
	To      string
	Kind    string
	Subject string
	Body    string
	Token   string
}

// EmailSender delivers e-mail. Delivery failures are logged by the caller
// and never change an API response (that would reveal accounts).
type EmailSender interface {
	Send(ctx context.Context, m Email) error
}

// MemEmailSender records messages (tests, local development). It is safe
// for concurrent use.
type MemEmailSender struct {
	mu   sync.Mutex
	sent []Email
}

// Send implements EmailSender.
func (m *MemEmailSender) Send(_ context.Context, e Email) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, e)
	return nil
}

// Sent returns a copy of every message sent.
func (m *MemEmailSender) Sent() []Email {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.sent)
}

// Last returns the newest message to addr of kind (any kind if empty).
func (m *MemEmailSender) Last(addr, kind string) (Email, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.sent) - 1; i >= 0; i-- {
		e := m.sent[i]
		if auth.NormalizeEmail(e.To) == auth.NormalizeEmail(addr) && (kind == "" || e.Kind == kind) {
			return e, true
		}
	}
	return Email{}, false
}

// LogEmailSender writes messages to a structured logger (development). The
// body, which contains the link with the token, is logged at debug level.
type LogEmailSender struct {
	Log *slog.Logger
}

// Send implements EmailSender.
func (l LogEmailSender) Send(ctx context.Context, e Email) error {
	log := l.Log
	if log == nil {
		log = slog.Default()
	}
	log.InfoContext(ctx, "email", "to", e.To, "kind", e.Kind, "subject", e.Subject)
	log.DebugContext(ctx, "email body", "to", e.To, "body", e.Body)
	return nil
}

// ---- Signing-key secrets -----------------------------------------------

// KeyStore holds signing-key private material (Secret Manager or KMS in
// production). The database stores only the returned reference.
type KeyStore interface {
	// PutPrivateKey stores an Ed25519 seed and returns its reference.
	PutPrivateKey(ctx context.Context, kid string, seed []byte) (ref string, err error)
	// GetPrivateKey returns the seed for ref, or ErrNotFound.
	GetPrivateKey(ctx context.Context, ref string) ([]byte, error)
}

// ProvisionedKey is an operator-provisioned signing key: its kid, the
// reference stored as private_key_ref, and the seed.
type ProvisionedKey struct {
	KID  string
	Ref  string
	Seed []byte
}

// ProvisionedKeys is a KeyStore whose current key an operator provisions
// (Terraform mounts FF_SIGNING_KEY_<KID>; keystore.Provisioned). The
// KeyManager signs with CurrentKey, registers it in identity.signing_keys
// if it is new (the previous active key becomes retiring), and never
// generates keys: Rotate fails with ErrOperatorRotation and Maintain does
// not rotate. ok false means no key is designated and the KeyManager
// manages keys itself.
type ProvisionedKeys interface {
	KeyStore
	CurrentKey(ctx context.Context) (key ProvisionedKey, ok bool, err error)
}

// MemKeyStore keeps seeds in process memory (development): keys vanish on
// restart, and the KeyManager then rotates to a fresh key while the old
// public keys keep verifying until they retire.
type MemKeyStore struct {
	mu    sync.Mutex
	seeds map[string][]byte
}

// NewMemKeyStore returns an empty key store.
func NewMemKeyStore() *MemKeyStore { return &MemKeyStore{seeds: map[string][]byte{}} }

// PutPrivateKey implements KeyStore.
func (m *MemKeyStore) PutPrivateKey(_ context.Context, kid string, seed []byte) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ref := "mem://signing-keys/" + kid
	m.seeds[ref] = slices.Clone(seed)
	return ref, nil
}

// GetPrivateKey implements KeyStore.
func (m *MemKeyStore) GetPrivateKey(_ context.Context, ref string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.seeds[ref]
	if !ok {
		return nil, notFound("private key " + ref)
	}
	return slices.Clone(s), nil
}

// ---- Rate limiting -----------------------------------------------------

// LimiterFactory builds one limiter whose every key uses p.
type LimiterFactory func(p limiter.Params) (limiter.ContextLimiter, error)

// MemLimiterFactory builds in-process GCRA limiters.
func MemLimiterFactory(p limiter.Params) (limiter.ContextLimiter, error) {
	g, err := limiter.NewAtomicGCRA(limiter.Rule{Pattern: "*", Params: p})
	if err != nil {
		return nil, err
	}
	return ctxLimiter{g}, nil
}

type ctxLimiter struct{ l limiter.Limiter }

func (c ctxLimiter) Acquire(_ context.Context, key string, cost int, now time.Duration) (limiter.Result, error) {
	return c.l.Acquire(key, cost, now)
}

func (c ctxLimiter) AcquireAll(_ context.Context, reqs []limiter.Request, now time.Duration) (limiter.Result, error) {
	return c.l.AcquireAll(reqs, now)
}

// RateLimiter applies GCRA limits whose parameters are chosen per call (the
// key builders of spec 14.3 put the endpoint after variable parts, so
// prefix rules cannot tell rl:auth:ip:<ip>:login from ...:signup). It keeps
// one limiter per distinct Params. It implements auth.Throttle.
type RateLimiter struct {
	factory LimiterFactory
	clock   core.Clock
	mu      sync.Mutex
	byP     map[limiter.Params]limiter.ContextLimiter
}

var _ auth.Throttle = (*RateLimiter)(nil)

// NewRateLimiter returns a RateLimiter over factory (MemLimiterFactory when
// nil); clock supplies the monotonic reading passed to the limiter.
func NewRateLimiter(factory LimiterFactory, clock core.Clock) *RateLimiter {
	if factory == nil {
		factory = MemLimiterFactory
	}
	return &RateLimiter{factory: factory, clock: clock, byP: map[limiter.Params]limiter.ContextLimiter{}}
}

// Take charges cost against key under p.
func (r *RateLimiter) Take(ctx context.Context, key string, p limiter.Params, cost int) (limiter.Result, error) {
	r.mu.Lock()
	l, ok := r.byP[p]
	if !ok {
		var err error
		if l, err = r.factory(p); err != nil {
			r.mu.Unlock()
			return limiter.Result{}, fmt.Errorf("identity: limiter for %+v: %w", p, err)
		}
		r.byP[p] = l
	}
	r.mu.Unlock()
	return l.Acquire(ctx, key, cost, r.clock.Monotonic())
}

// ParamsFor converts "count per period" into GCRA parameters (burst count,
// refilling at count/period).
func ParamsFor(l auth.Limit) limiter.Params {
	return limiter.Params{Rate: float64(l.Count) / l.Per.Seconds(), Burst: l.Count}
}

// Allow implements auth.Throttle.
func (r *RateLimiter) Allow(ctx context.Context, key string, l auth.Limit) (bool, time.Duration, error) {
	if l.Count <= 0 || l.Per <= 0 {
		return false, 0, fmt.Errorf("identity: invalid limit %+v", l)
	}
	res, err := r.Take(ctx, key, ParamsFor(l), 1)
	if err != nil {
		return false, 0, err
	}
	if !res.Allowed {
		retry := res.RetryAfter
		// A denial with no wait (redislimiter reports whole milliseconds,
		// so a shorter wait reads as 0) or one over a day (limiter.Never)
		// still yields a positive, bounded Retry-After: the period, capped
		// at a day.
		if retry <= 0 || retry > 24*time.Hour {
			retry = min(l.Per, 24*time.Hour)
		}
		return false, retry, nil
	}
	return true, 0, nil
}

// RetrySeconds rounds a retry delay up to whole seconds, clamped to
// [1, 86400], for Retry-After headers.
func RetrySeconds(d time.Duration) int {
	if d <= 0 {
		return 1
	}
	s := math.Ceil(d.Seconds())
	if s > 86400 {
		s = 86400
	}
	return int(s)
}

// randomReader reports an error for a config without a random source,
// so a missing reader fails construction rather than the first token.
func randomReader(r io.Reader) error {
	if r == nil {
		return fmt.Errorf("identity: no random reader")
	}
	return nil
}
