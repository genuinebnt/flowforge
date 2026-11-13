package identity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
)

// Topic is the outbox topic of every identity event (spec 11.7).
const Topic = "auth.events"

// Audit/outbox event names.
const (
	EventSignup          = "signup"
	EventSignupExisting  = "signup_existing_email"
	EventEmailVerified   = "email_verified"
	EventLogin           = "login"
	EventLoginFailed     = "login_failed"
	EventLoginThrottled  = "login_throttled"
	EventSignupThrottled = "signup_throttled"
	EventRefreshReuse    = auth.AuditRefreshReuse
	EventLogout          = "logout"
	EventPasswordForgot  = "password_forgot"
	EventPasswordReset   = "password_reset"
	EventRoleChanged     = "member_role_changed"
	EventInviteCreated   = "invite_created"
	EventInviteAccepted  = "invite_accepted"
	EventAPIKeyCreated   = "api_key_created"
	EventAPIKeyRevoked   = "api_key_revoked"
	EventVerifyResent    = "verification_resent"
	EventSessionsRevoked = "sessions_revoked"
)

// Limits are the auth throttles of spec 13.1 (limiter keys of 14.3).
type Limits struct {
	Signup        auth.Limit // per IP: rl:auth:ip:<ip>:signup
	LoginIP       auth.Limit // per IP: rl:auth:ip:<ip>:login
	LoginEmail    auth.Limit // per account: rl:auth:acct:<sha256 email>
	Resend        auth.Limit // per account: rl:auth:acct:<sha256 email>:resend
	Forgot        auth.Limit // per IP: rl:auth:ip:<ip>:password_forgot
	ForgotAccount auth.Limit // per account: rl:auth:acct:<sha256 email>:forgot
}

// DefaultLimits returns the spec 13.1 values (forgot-password limits are
// this implementation's choice).
func DefaultLimits() Limits {
	return Limits{
		Signup:        auth.Limit{Count: 5, Per: time.Hour},
		LoginIP:       auth.DefaultLoginIPLimit(),
		LoginEmail:    auth.DefaultLoginEmailLimit(),
		Resend:        auth.Limit{Count: 3, Per: time.Hour},
		Forgot:        auth.Limit{Count: 5, Per: time.Hour},
		ForgotAccount: auth.Limit{Count: 3, Per: time.Hour},
	}
}

// Config configures a Service.
type Config struct {
	Store       Store
	Keys        *KeyManager
	Revocations Revocations
	Limiter     *RateLimiter
	Lockouts    auth.LockoutStore
	Passwords   auth.PasswordHasher
	Policy      auth.PasswordPolicy
	Email       EmailSender
	Clock       core.Clock
	// IDs draws row ids under the service's own lock; an Rng shared with
	// another user (KeyManagerConfig.IDs) must itself be safe for
	// concurrent use, such as core.SystemRng.
	IDs    core.Rng
	Random io.Reader // secrets: refresh tokens, API keys, e-mail tokens
	// PublicURL is the console base URL used in e-mail links.
	PublicURL string
	Limits    Limits
	InviteTTL time.Duration // default 7 days
	ResetTTL  time.Duration // default 1 hour
	Log       *slog.Logger
}

// Service implements the identity flows.
type Service struct {
	cfg       Config
	login     *auth.LoginService
	refresher *auth.Refresher
	ids       lockedRng
}

// ClientInfo describes the caller of a public endpoint.
type ClientInfo struct {
	IP        string
	UserAgent string
}

// Actor is an authenticated caller.
type Actor struct {
	TenantID core.UUID
	UserID   core.UUID // for API keys: the key's creator
	KeyID    core.UUID // zero for users
	Role     auth.Role
	Client   ClientInfo
}

// New returns a Service.
func New(cfg Config) (*Service, error) {
	switch {
	case cfg.Store == nil || cfg.Keys == nil || cfg.Revocations == nil || cfg.Limiter == nil || cfg.Lockouts == nil:
		return nil, fmt.Errorf("identity: config needs store, keys, revocations, limiter and lockouts")
	case cfg.Passwords == nil || cfg.Policy.Breached == nil || cfg.Email == nil || cfg.Clock == nil || cfg.IDs == nil:
		return nil, fmt.Errorf("identity: config needs passwords, breached checker, email, clock and ids")
	}
	if err := randomReader(cfg.Random); err != nil {
		return nil, err
	}
	def := DefaultLimits()
	fill := func(l *auth.Limit, d auth.Limit) {
		if l.Count <= 0 || l.Per <= 0 {
			*l = d
		}
	}
	fill(&cfg.Limits.Signup, def.Signup)
	fill(&cfg.Limits.LoginIP, def.LoginIP)
	fill(&cfg.Limits.LoginEmail, def.LoginEmail)
	fill(&cfg.Limits.Resend, def.Resend)
	fill(&cfg.Limits.Forgot, def.Forgot)
	fill(&cfg.Limits.ForgotAccount, def.ForgotAccount)
	if cfg.InviteTTL <= 0 {
		cfg.InviteTTL = 7 * 24 * time.Hour
	}
	if cfg.ResetTTL <= 0 {
		cfg.ResetTTL = time.Hour
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	s := &Service{cfg: cfg, ids: lockedRng{mu: &sync.Mutex{}, r: cfg.IDs}}
	var err error
	s.login, err = auth.NewLoginService(auth.LoginConfig{
		Users: userStore{cfg.Store}, Throttle: cfg.Limiter, Lockouts: cfg.Lockouts, Passwords: cfg.Passwords,
		Clock: cfg.Clock, IPLimit: cfg.Limits.LoginIP, EmailLimit: cfg.Limits.LoginEmail,
	})
	if err != nil {
		return nil, err
	}
	s.refresher, err = auth.NewRefresher(auth.RefresherConfig{
		Store: refreshStore{s: s, st: cfg.Store}, Clock: cfg.Clock, Random: cfg.Random, IDs: s.ids,
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Keys returns the key manager.
func (s *Service) Keys() *KeyManager { return s.cfg.Keys }

// Store returns the repository.
func (s *Service) Store() Store { return s.cfg.Store }

func (s *Service) newID() core.UUID { return core.NewUUIDv7(s.cfg.Clock, s.ids) }

func (s *Service) now() time.Time { return s.cfg.Clock.Now().UTC() }

// ---- errors ------------------------------------------------------------

func errValidation(format string, args ...any) error {
	return core.Errorf(core.KindInvalidArgument, core.ReasonValidationFailed, format, args...)
}

func errUnauthenticated(reason, msg string) error {
	return core.Errorf(core.KindUnauthenticated, reason, "%s", msg)
}

func errNotFound() error {
	return core.Errorf(core.KindNotFound, core.ReasonNotFound, "resource not found")
}

func errForbidden(msg string) error {
	return core.Errorf(core.KindPermissionDenied, core.ReasonForbiddenRole, "%s", msg)
}

func errRateLimited(retry time.Duration) error {
	return &core.Error{
		Kind: core.KindResourceExhausted, Reason: core.ReasonRateLimited,
		Message: "too many attempts; retry later", RetryAfter: retry,
	}
}

func errInternal(err error) error { return core.Wrap(err, core.KindInternal, core.ReasonInternal) }

// errInvalidCredentials is the single response for unknown emails, wrong
// passwords and accounts without a usable membership (spec 13.1).
func errInvalidCredentials() error {
	return errUnauthenticated(core.ReasonTokenInvalid, "invalid email or password")
}

func throttleErr(err error) error {
	var te *auth.ThrottledError
	if errors.As(err, &te) {
		return errRateLimited(te.RetryAfter)
	}
	return errInternal(err)
}

// ---- events ------------------------------------------------------------

// record writes one auth_audit row and the matching auth.events outbox
// row through st (pass a transaction to make them atomic with a change).
func (s *Service) record(ctx context.Context, st Store, event string, tenant, user core.UUID, ci ClientInfo, detail map[string]string) error {
	now := s.now()
	if err := st.AppendAudit(ctx, AuditRecord{
		At: now, TenantID: tenant, UserID: user, Event: event, IP: ci.IP, UserAgent: ci.UserAgent, Detail: detail,
	}); err != nil {
		return fmt.Errorf("identity: audit %s: %w", event, err)
	}
	generic := map[string]string{}
	for k, v := range detail {
		generic[k] = v
	}
	tid, uid := "", ""
	if !tenant.IsZero() {
		tid = core.PublicID(core.PrefixTenant, tenant)
	}
	if !user.IsZero() {
		uid = core.PublicID(core.PrefixUser, user)
		generic["user_id"] = uid
	}
	id := s.newID()
	payload, err := json.Marshal(map[string]any{
		"event_id": id.String(), "type": "auth." + event, "schema_version": 1,
		"tenant_id": tid, "occurred_at": core.FormatTime(now), "generic": generic,
	})
	if err != nil {
		return err
	}
	if err := st.AppendOutbox(ctx, OutboxEvent{EventID: id, Topic: Topic, OrderingKey: uid, Payload: payload, CreatedAt: now}); err != nil {
		return fmt.Errorf("identity: outbox %s: %w", event, err)
	}
	return nil
}

// recordBestEffort records an event outside a state change; a failure is
// logged, never returned (it must not change a uniform response).
func (s *Service) recordBestEffort(ctx context.Context, event string, tenant, user core.UUID, ci ClientInfo, detail map[string]string) {
	if err := s.record(ctx, s.cfg.Store, event, tenant, user, ci, detail); err != nil {
		s.cfg.Log.WarnContext(ctx, "identity: record event", "event", event, "err", err)
	}
}

func (s *Service) send(ctx context.Context, m Email) {
	if err := s.cfg.Email.Send(ctx, m); err != nil {
		s.cfg.Log.WarnContext(ctx, "identity: send email", "kind", m.Kind, "err", err)
	}
}

// publishSessionVersion caches a bumped session version so access tokens
// with an older sv are rejected (spec 13.3).
func (s *Service) publishSessionVersion(ctx context.Context, user core.UUID, v int64) error {
	return s.cfg.Revocations.SetSessionVersion(ctx, core.PublicID(core.PrefixUser, user), v, SessionVersionTTL)
}

// ---- validation helpers ------------------------------------------------

// NormalizeEmail validates and normalizes an e-mail address.
func NormalizeEmail(raw string) (string, error) {
	e := auth.NormalizeEmail(raw)
	local, domain, ok := strings.Cut(e, "@")
	switch {
	case !ok || local == "" || domain == "" || strings.Contains(domain, "@"):
		return "", errValidation("email: not a valid address")
	case len(e) > 254 || !utf8.ValidString(e):
		return "", errValidation("email: not a valid address")
	case strings.ContainsFunc(e, func(r rune) bool { return r <= ' ' || r == 0x7f }):
		return "", errValidation("email: not a valid address")
	case !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, "."):
		return "", errValidation("email: not a valid address")
	}
	return e, nil
}

func (s *Service) checkPassword(ctx context.Context, pw string) error {
	if err := s.cfg.Policy.Check(ctx, pw); err != nil {
		if errors.Is(err, auth.ErrPasswordPolicy) {
			return errValidation("password: %s", strings.TrimPrefix(err.Error(), "auth: password rejected by policy: "))
		}
		return errInternal(err)
	}
	return nil
}

// newSecret returns a 32-byte base64url secret (same format as the auth
// package's opaque tokens).
func (s *Service) newSecret() (string, error) {
	b := make([]byte, auth.SecretBytes)
	if _, err := io.ReadFull(s.cfg.Random, b); err != nil {
		return "", fmt.Errorf("identity: read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func isSecret(tok string) bool {
	if len(tok) != 43 {
		return false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(tok)
	return err == nil && len(b) == auth.SecretBytes
}

// resetHash domain-separates password-reset tokens from e-mail
// verification tokens: both live in identity.email_verifications, and a
// token of one kind can never be redeemed as the other.
func resetHash(tok string) [32]byte { return auth.HashToken("password_reset\x00" + tok) }

// EncodePageToken encodes a keyset cursor.
func EncodePageToken(last core.UUID) string {
	return base64.RawURLEncoding.EncodeToString(last[:])
}

// DecodePageToken decodes a cursor from EncodePageToken ("" is the start).
func DecodePageToken(tok string) (core.UUID, error) {
	if tok == "" {
		return core.Nil, nil
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(tok)
	if err != nil || len(b) != 16 {
		return core.Nil, errValidation("page_token: invalid")
	}
	var u core.UUID
	copy(u[:], b)
	return u, nil
}

// ---- store adapters ----------------------------------------------------

// userStore adapts Store to auth.UserStore.
type userStore struct{ st Store }

func (u userStore) FindByEmail(ctx context.Context, email string) (auth.User, error) {
	usr, err := u.st.GetUserByEmail(ctx, email)
	if errors.Is(err, ErrNotFound) {
		return auth.User{}, auth.ErrUserNotFound
	}
	if err != nil {
		return auth.User{}, err
	}
	return auth.User{ID: usr.ID, Email: usr.Email, PasswordHash: usr.PasswordHash, Status: usr.Status, SessionVersion: usr.SessionVersion}, nil
}

func (u userStore) SetPasswordHash(ctx context.Context, id core.UUID, hash string) error {
	return u.st.SetPasswordHash(ctx, id, hash)
}

// verifStore adapts Store to auth.VerificationStore.
type verifStore struct{ st Store }

func (v verifStore) Insert(ctx context.Context, x auth.Verification) error {
	return v.st.InsertVerification(ctx, x)
}

func (v verifStore) Consume(ctx context.Context, hash [32]byte, now time.Time) (auth.Verification, error) {
	return v.st.ConsumeVerification(ctx, hash, now)
}

// refreshStore adapts Store to auth.RefreshStore. Session-version bumps
// are published to the revocation cache and audit events also go to the
// outbox.
type refreshStore struct {
	s  *Service
	st Store
}

var _ auth.RefreshStore = refreshStore{}

func (r refreshStore) FindByHash(ctx context.Context, hash [32]byte) (auth.RefreshToken, error) {
	t, err := r.st.FindRefreshToken(ctx, hash)
	if errors.Is(err, ErrNotFound) {
		return auth.RefreshToken{}, auth.ErrNotFound
	}
	return t, err
}

func (r refreshStore) MarkUsed(ctx context.Context, id core.UUID, at time.Time) (bool, error) {
	return r.st.MarkRefreshUsed(ctx, id, at)
}

func (r refreshStore) Insert(ctx context.Context, t auth.RefreshToken) error {
	return r.st.InsertRefreshToken(ctx, t)
}

func (r refreshStore) RevokeFamily(ctx context.Context, family core.UUID, at time.Time) error {
	return r.st.RevokeFamily(ctx, family, at)
}

func (r refreshStore) BumpSessionVersion(ctx context.Context, user core.UUID) (int64, error) {
	v, err := r.st.BumpSessionVersion(ctx, user)
	if err != nil {
		return 0, err
	}
	return v, r.s.publishSessionVersion(ctx, user, v)
}

func (r refreshStore) Audit(ctx context.Context, e auth.AuditEvent) error {
	return r.s.record(ctx, r.st, e.Event, e.TenantID, e.UserID, ClientInfo{}, e.Detail)
}
