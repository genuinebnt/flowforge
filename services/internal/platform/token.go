package platform

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
)

// TokenSource mints the internal token a client sends for an identity
// (authorization: Bearer <token>, spec 11.1).
type TokenSource interface {
	Token(ctx context.Context, id Identity) (string, error)
}

// TokenVerifier verifies an internal token; *auth.Verifier implements it.
type TokenVerifier interface {
	Verify(ctx context.Context, token string) (*auth.Claims, error)
}

// MinterTokens mints internal tokens with an auth.Minter (60 s lifetime,
// audience flowforge-internal) and caches each one for half its lifetime.
type MinterTokens struct {
	Minter *auth.Minter
	Clock  core.Clock

	mu    sync.Mutex
	cache map[string]cachedToken // by cacheKey
}

type cachedToken struct {
	token string
	until time.Time
}

// NewMinterTokens returns a token source over m.
func NewMinterTokens(m *auth.Minter, clock core.Clock) *MinterTokens {
	return &MinterTokens{Minter: m, Clock: clock, cache: map[string]cachedToken{}}
}

// cacheKey identifies the claims a token carries. PORT: Identity holds a
// slice, so it cannot be a Go map key; the NUL-separated form is
// unambiguous because none of the parts may contain NUL.
func cacheKey(id Identity) string {
	return id.Tenant.String() + "\x00" + id.Actor + "\x00" + string(id.Role) + "\x00" + strings.Join(id.Groups, "\x00")
}

// Token implements TokenSource.
func (t *MinterTokens) Token(_ context.Context, id Identity) (string, error) {
	now := t.Clock.Now()
	key := cacheKey(id)
	t.mu.Lock()
	if c, ok := t.cache[key]; ok && now.Before(c.until) {
		t.mu.Unlock()
		return c.token, nil
	}
	t.mu.Unlock()
	tok, claims, err := t.Minter.Internal(auth.InternalGrant{TenantID: id.Tenant, Actor: id.Actor, Role: id.Role, Groups: id.Groups})
	if err != nil {
		return "", err
	}
	life := claims.Expiry().Sub(claims.Issued())
	t.mu.Lock()
	if len(t.cache) > 10000 {
		clear(t.cache)
	}
	t.cache[key] = cachedToken{token: tok, until: claims.Issued().Add(life / 2)}
	t.mu.Unlock()
	return tok, nil
}

// Keys is the internal-token key material of one process: a signer for
// the tokens it mints and the key resolver its verifier trusts.
type Keys struct {
	Issuer   string
	Signer   *auth.Signer
	Resolver auth.KeyResolver
}

// NewDevKeys generates an ephemeral Ed25519 key (dev and tests only): the
// returned Keys sign with it and trust only it.
func NewDevKeys(issuer string) (Keys, error) {
	k, err := auth.GenerateSigningKey(rand.Reader, "dev-"+fmt.Sprint(core.SystemRng{}.Uint64()%100000))
	if err != nil {
		return Keys{}, err
	}
	return KeysFromSigningKey(issuer, k)
}

// KeysFromSigningKey returns Keys that sign with k and trust k.
func KeysFromSigningKey(issuer string, k auth.SigningKey) (Keys, error) {
	s, err := auth.NewSigner(k)
	if err != nil {
		return Keys{}, err
	}
	set, err := auth.NewKeySet(k.Public())
	if err != nil {
		return Keys{}, err
	}
	return Keys{Issuer: issuer, Signer: s, Resolver: set}, nil
}

// Verifier returns the internal-token verifier for k.
func (k Keys) Verifier(clock core.Clock) (*auth.Verifier, error) {
	return auth.NewVerifier(auth.InternalVerifierConfig(k.Resolver, clock, k.Issuer))
}

// Tokens returns a caching token source signing with k (nil without a
// signer).
func (k Keys) Tokens(clock core.Clock) (*MinterTokens, error) {
	if k.Signer == nil {
		return nil, nil
	}
	m, err := auth.NewMinter(auth.MinterConfig{Signer: k.Signer, Clock: clock, IDs: core.SystemRng{}, Issuer: k.Issuer})
	if err != nil {
		return nil, err
	}
	return NewMinterTokens(m, clock), nil
}

// identityFromToken verifies a bearer token and checks the x-tenant-id and
// x-actor metadata against it. The tenant, actor and role come only from
// the token.
func identityFromToken(ctx context.Context, v TokenVerifier, authz, tenantMD, actorMD string) (Identity, error) {
	if authz == "" {
		return Identity{}, core.Errorf(core.KindUnauthenticated, core.ReasonTokenMissing, "missing authorization metadata")
	}
	tok, ok := strings.CutPrefix(authz, "Bearer ")
	if !ok || tok == "" {
		return Identity{}, core.Errorf(core.KindUnauthenticated, core.ReasonTokenMissing, "authorization must be a bearer token")
	}
	claims, err := v.Verify(ctx, tok)
	if err != nil {
		reason := core.ReasonTokenInvalid
		if auth.RejectionReason(err) == "expired" {
			reason = core.ReasonTokenExpired
		}
		return Identity{}, &core.Error{Kind: core.KindUnauthenticated, Reason: reason, Message: "internal token rejected", Err: err}
	}
	tenant, err := core.ParsePublicID(core.PrefixTenant, claims.TenantID)
	if err != nil {
		return Identity{}, core.Errorf(core.KindUnauthenticated, core.ReasonTokenInvalid, "token tenant is malformed")
	}
	if tenantMD != "" {
		mdTenant, err := core.ParsePublicID(core.PrefixTenant, tenantMD)
		if err != nil || mdTenant != tenant {
			return Identity{}, core.Errorf(core.KindUnauthenticated, core.ReasonTokenInvalid, "x-tenant-id does not match the token")
		}
	}
	actor := normalizeActor(claims.Subject)
	if actorMD != "" && normalizeActor(actorMD) != actor {
		return Identity{}, core.Errorf(core.KindUnauthenticated, core.ReasonTokenInvalid, "x-actor does not match the token")
	}
	role := claims.Role
	if role != "" && !role.Valid() {
		return Identity{}, core.Errorf(core.KindUnauthenticated, core.ReasonTokenInvalid, "token role is unknown")
	}
	return Identity{Tenant: tenant, Actor: actor, Role: role, Groups: claims.Groups}, nil
}

// IdentityFromBearer verifies an "authorization"-style header value
// ("Bearer <internal token>") and returns the identity it carries (the
// egress proxy authenticates the tool runner with it).
func IdentityFromBearer(ctx context.Context, v TokenVerifier, header string) (Identity, error) {
	return identityFromToken(ctx, v, header, "", "")
}

// normalizeActor accepts the token subject forms: "user:<uuid>" (kept),
// "usr_..." public ids (rendered user:<uuid>) and anything else verbatim.
func normalizeActor(s string) string {
	if strings.HasPrefix(s, string(core.PrefixUser)+"_") {
		if u, err := core.ParsePublicID(core.PrefixUser, s); err == nil {
			return "user:" + u.String()
		}
	}
	return s
}
