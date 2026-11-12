package auth

import (
	"fmt"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// MinterConfig configures a Minter.
type MinterConfig struct {
	Signer *Signer
	Clock  core.Clock
	// IDs supplies the randomness of jti values (UUIDv7). Token ids must be
	// unique, not secret, so a core.Rng is appropriate.
	IDs    core.Rng
	Issuer string
	// AccessTTL defaults to AccessTokenTTL.
	AccessTTL time.Duration
}

// Minter issues access tokens and internal tokens. It is safe for
// concurrent use.
type Minter struct {
	cfg MinterConfig
	mu  sync.Mutex // guards cfg.IDs
}

// NewMinter checks cfg and returns a Minter.
func NewMinter(cfg MinterConfig) (*Minter, error) {
	if cfg.Signer == nil || cfg.Clock == nil || cfg.IDs == nil || cfg.Issuer == "" {
		return nil, fmt.Errorf("%w: minter needs a signer, clock, id source and issuer", ErrConfig)
	}
	if cfg.AccessTTL <= 0 {
		cfg.AccessTTL = AccessTokenTTL
	}
	return &Minter{cfg: cfg}, nil
}

// AccessGrant describes the session an access token is minted for.
type AccessGrant struct {
	UserID         core.UUID
	TenantID       core.UUID
	Role           Role
	SessionID      core.UUID // the refresh family id
	SessionVersion int64     // user.session_version
}

// InternalGrant describes an internal (service-to-service) token: the
// tenant, the acting principal and its role (spec 13.2).
type InternalGrant struct {
	TenantID core.UUID
	Actor    string // usr_... or a service account id
	Role     Role
}

func (m *Minter) jti() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return core.NewUUIDv7(m.cfg.Clock, m.cfg.IDs).String()
}

func (m *Minter) mint(c Claims, ttl time.Duration) (string, Claims, error) {
	if !c.Role.Valid() {
		return "", Claims{}, fmt.Errorf("auth: cannot mint a token for role %q", c.Role)
	}
	now := m.cfg.Clock.Now().Unix()
	c.Issuer = m.cfg.Issuer
	c.ID = m.jti()
	c.IssuedAt = now
	c.ExpiresAt = now + int64(ttl/time.Second)
	tok, err := m.cfg.Signer.Sign(c)
	if err != nil {
		return "", Claims{}, err
	}
	return tok, c, nil
}

// Access mints a 10-minute access token for g (aud flowforge-api).
func (m *Minter) Access(g AccessGrant) (string, Claims, error) {
	return m.mint(Claims{
		Audience:       AudienceAPI,
		Subject:        core.PublicID(core.PrefixUser, g.UserID),
		TenantID:       core.PublicID(core.PrefixTenant, g.TenantID),
		Role:           g.Role,
		SessionID:      core.PublicID(core.PrefixSession, g.SessionID),
		SessionVersion: g.SessionVersion,
	}, m.cfg.AccessTTL)
}

// Internal mints a 60-second internal token for g (aud
// flowforge-internal). The actor is carried in sub.
func (m *Minter) Internal(g InternalGrant) (string, Claims, error) {
	if g.Actor == "" {
		return "", Claims{}, fmt.Errorf("auth: internal token needs an actor")
	}
	return m.mint(Claims{
		Audience: AudienceInternal,
		Subject:  g.Actor,
		TenantID: core.PublicID(core.PrefixTenant, g.TenantID),
		Role:     g.Role,
	}, InternalTokenTTL)
}

// InternalVerifierConfig returns the VerifierConfig for internal tokens:
// audience flowforge-internal, lifetime at most 60 seconds, no revocation
// lookup (the lifetime is shorter than any revocation would take to
// propagate).
func InternalVerifierConfig(keys KeyResolver, clock core.Clock, issuer string) VerifierConfig {
	return VerifierConfig{
		Keys: keys, Clock: clock, Issuer: issuer, Audience: AudienceInternal,
		MaxLifetime: InternalTokenTTL, Revocation: NoRevocation{},
	}
}
