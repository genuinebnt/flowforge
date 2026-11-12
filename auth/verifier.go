package auth

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// Token audiences and lifetimes (spec 13.2, 13.3).
const (
	AudienceAPI      = "flowforge-api"
	AudienceInternal = "flowforge-internal"
	AccessTokenTTL   = 10 * time.Minute
	InternalTokenTTL = 60 * time.Second
	// DefaultSkew is the clock-skew allowance for exp, iat and nbf.
	DefaultSkew = 30 * time.Second
)

// VerifierConfig configures a Verifier.
type VerifierConfig struct {
	Keys     KeyResolver // KeySet or KeyCache
	Clock    core.Clock
	Issuer   string // required exact iss
	Audience string // required exact aud (AudienceAPI or AudienceInternal)
	// Skew is the clock-skew allowance; zero means DefaultSkew.
	Skew time.Duration
	// MaxLifetime, when positive, rejects tokens whose exp - iat exceeds it
	// (InternalTokenTTL for internal tokens).
	MaxLifetime time.Duration
	// Revocation is required; pass NoRevocation{} to opt out explicitly.
	Revocation RevocationChecker
}

// Verifier validates access and internal tokens exactly as spec 13.3
// prescribes. It is safe for concurrent use.
type Verifier struct {
	cfg VerifierConfig
}

// NewVerifier checks cfg and returns a Verifier.
func NewVerifier(cfg VerifierConfig) (*Verifier, error) {
	switch {
	case cfg.Keys == nil || cfg.Clock == nil || cfg.Revocation == nil:
		return nil, fmt.Errorf("%w: verifier needs keys, a clock and a revocation checker", ErrConfig)
	case cfg.Issuer == "" || cfg.Audience == "":
		return nil, fmt.Errorf("%w: verifier needs an issuer and an audience", ErrConfig)
	case cfg.Skew < 0 || cfg.MaxLifetime < 0:
		return nil, fmt.Errorf("%w: negative skew or lifetime", ErrConfig)
	}
	if cfg.Skew == 0 {
		cfg.Skew = DefaultSkew
	}
	return &Verifier{cfg: cfg}, nil
}

// Verify validates token and returns its claims. The steps, in order:
//
//  1. structure: three base64url segments, a JSON header without
//     duplicate or crit members (ErrMalformed, ErrCritUnsupported);
//  2. alg is exactly "EdDSA", never chosen by the header (ErrAlgNone,
//     ErrAlgNotAllowed); typ is "JWT" (ErrTypInvalid);
//  3. the key named by kid (ErrKidMissing, ErrUnknownKid, ErrKeyRetired);
//  4. the Ed25519 signature (ErrBadSignature); nothing in the payload is
//     read before this point;
//  5. iss, aud, exp, iat and nbf with the skew allowance (ErrIssuer,
//     ErrAudience, ErrExpired, ErrIssuedInFuture, ErrNotYetValid), the
//     lifetime bound and the required claims;
//  6. revocation by jti and session version (ErrRevoked,
//     ErrSessionRevoked, ErrRevocationUnavailable).
//
// Every rejection except ErrRevocationUnavailable matches ErrInvalidToken.
func (v *Verifier) Verify(ctx context.Context, token string) (*Claims, error) {
	tok, err := parseCompact(token)
	if err != nil {
		return nil, err
	}
	h := tok.header
	switch {
	case strings.EqualFold(h.Alg, "none"):
		return nil, ErrAlgNone
	case h.Alg != Algorithm:
		return nil, fmt.Errorf("%w: %q", ErrAlgNotAllowed, h.Alg)
	case h.Typ != TokenType:
		return nil, fmt.Errorf("%w: %q", ErrTypInvalid, h.Typ)
	case h.Crit != nil:
		return nil, ErrCritUnsupported
	case h.Kid == "":
		return nil, ErrKidMissing
	}
	key, err := v.cfg.Keys.ResolveKey(ctx, h.Kid)
	if err != nil {
		return nil, err
	}
	if len(key.Key) != ed25519.PublicKeySize || !ed25519.Verify(key.Key, []byte(tok.signingInput), tok.signature) {
		return nil, ErrBadSignature
	}

	var c Claims
	if err := decodeObject(tok.payload, &c, claimNames...); err != nil {
		return nil, fmt.Errorf("%w: claims: %w", ErrMalformed, err)
	}
	if err := v.checkClaims(&c); err != nil {
		return nil, err
	}
	if err := v.checkRevocation(ctx, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (v *Verifier) checkClaims(c *Claims) error {
	now := v.cfg.Clock.Now()
	skew := v.cfg.Skew
	switch {
	case c.Issuer != v.cfg.Issuer:
		return fmt.Errorf("%w: %q", ErrIssuer, c.Issuer)
	case c.Audience != v.cfg.Audience:
		return fmt.Errorf("%w: %q", ErrAudience, c.Audience)
	case c.ExpiresAt == 0:
		return fmt.Errorf("%w: exp", ErrClaimMissing)
	case !now.Before(time.Unix(c.ExpiresAt, 0).Add(skew)):
		return ErrExpired
	case c.IssuedAt == 0:
		return fmt.Errorf("%w: iat", ErrClaimMissing)
	case time.Unix(c.IssuedAt, 0).After(now.Add(skew)):
		return ErrIssuedInFuture
	case c.NotBefore != 0 && time.Unix(c.NotBefore, 0).After(now.Add(skew)):
		return ErrNotYetValid
	case c.ExpiresAt <= c.IssuedAt:
		return fmt.Errorf("%w: exp is not after iat", ErrClaimInvalid)
	case v.cfg.MaxLifetime > 0 && time.Duration(c.ExpiresAt-c.IssuedAt)*time.Second > v.cfg.MaxLifetime:
		return ErrLifetimeTooLong
	}
	for _, f := range []struct{ name, val string }{
		{"sub", c.Subject}, {"tid", c.TenantID}, {"role", string(c.Role)}, {"jti", c.ID},
	} {
		if f.val == "" {
			return fmt.Errorf("%w: %s", ErrClaimMissing, f.name)
		}
	}
	if !c.Role.Valid() {
		return fmt.Errorf("%w: role %q", ErrClaimInvalid, c.Role)
	}
	if c.SessionVersion < 0 {
		return fmt.Errorf("%w: sv %d", ErrClaimInvalid, c.SessionVersion)
	}
	return nil
}

func (v *Verifier) checkRevocation(ctx context.Context, c *Claims) error {
	revoked, err := v.cfg.Revocation.IsRevoked(ctx, c.ID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRevocationUnavailable, err)
	}
	if revoked {
		return ErrRevoked
	}
	cur, ok, err := v.cfg.Revocation.SessionVersion(ctx, c.Subject)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRevocationUnavailable, err)
	}
	if ok && c.SessionVersion < cur {
		return ErrSessionRevoked
	}
	return nil
}
