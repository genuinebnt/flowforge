package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/services/gateway/identity"
)

// Cookie names (spec 13.5).
const (
	CookieAccess  = "ff_at"
	CookieRefresh = "ff_rt"
	CookieCSRF    = auth.CSRFCookie
)

// principal is the authenticated caller. Tenant and role come only from
// the token (spec 13.3).
type principal struct {
	TenantID  core.UUID
	Role      auth.Role
	UserID    core.UUID // users; for API keys the key's creator
	KeyID     core.UUID // API keys only
	Claims    *auth.Claims
	ViaCookie bool
}

func (p *principal) tenant() string { return core.PublicID(core.PrefixTenant, p.TenantID) }

// subject is the id used as the internal token's sub.
func (p *principal) subject() string {
	if !p.KeyID.IsZero() {
		return core.PublicID(core.PrefixAPIKey, p.KeyID)
	}
	return core.PublicID(core.PrefixUser, p.UserID)
}

// actor is the x-actor metadata value (spec 11.1).
func (p *principal) actor() string {
	if !p.KeyID.IsZero() {
		return "apikey:" + p.subject()
	}
	return "user:" + p.subject()
}

func (p *principal) identityActor(ip, ua string) identity.Actor {
	return identity.Actor{TenantID: p.TenantID, UserID: p.UserID, KeyID: p.KeyID, Role: p.Role, Client: identity.ClientInfo{IP: ip, UserAgent: ua}}
}

// sameTenant reports whether a client-supplied tenant id (ten_... or UUID)
// names the caller's tenant.
func (p *principal) sameTenant(v string) bool {
	u, err := core.ParsePublicID(core.PrefixTenant, v)
	return err == nil && u == p.TenantID
}

func hasCredentials(r *http.Request) bool {
	if r.Header.Get("Authorization") != "" {
		return true
	}
	_, err := r.Cookie(CookieAccess)
	return err == nil
}

func errUnauth(reason, msg string) *apiError {
	return newError(http.StatusUnauthorized, reason, "%s", msg)
}

// authenticate resolves the caller from Authorization: Bearer <JWT | ffk_>
// or, for the console, the ff_at cookie.
func (s *Server) authenticate(ctx context.Context, r *http.Request) (*principal, error) {
	h := r.Header.Get("Authorization")
	viaCookie := false
	var tok string
	switch {
	case h != "":
		scheme, rest, ok := strings.Cut(h, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(rest) == "" {
			return nil, errUnauth(core.ReasonTokenInvalid, "Authorization must be 'Bearer <token>'")
		}
		tok = strings.TrimSpace(rest)
	default:
		ck, err := r.Cookie(CookieAccess)
		if err != nil || ck.Value == "" {
			return nil, errUnauth(core.ReasonTokenMissing, "authentication required")
		}
		tok, viaCookie = ck.Value, true
	}
	if strings.HasPrefix(tok, auth.APIKeyPrefix) {
		if viaCookie {
			return nil, errUnauth(core.ReasonTokenInvalid, "API keys are only accepted in the Authorization header")
		}
		k, err := s.cfg.Identity.AuthenticateAPIKey(ctx, tok)
		if err != nil {
			return nil, err
		}
		return &principal{TenantID: k.TenantID, Role: k.Role, UserID: k.CreatedBy, KeyID: k.ID}, nil
	}
	claims, err := s.verifier.Verify(ctx, tok)
	switch {
	case errors.Is(err, auth.ErrExpired):
		return nil, errUnauth(core.ReasonTokenExpired, "access token has expired")
	case errors.Is(err, auth.ErrInvalidToken):
		return nil, errUnauth(core.ReasonTokenInvalid, "access token is invalid: "+auth.RejectionReason(err))
	case err != nil:
		return nil, newError(http.StatusServiceUnavailable, core.ReasonUnavailable, "token revocation state unavailable")
	}
	tid, err1 := core.ParsePublicID(core.PrefixTenant, claims.TenantID)
	uid, err2 := core.ParsePublicID(core.PrefixUser, claims.Subject)
	if err1 != nil || err2 != nil {
		return nil, errUnauth(core.ReasonTokenInvalid, "access token is invalid: claim_invalid")
	}
	return &principal{TenantID: tid, Role: claims.Role, UserID: uid, Claims: claims, ViaCookie: viaCookie}, nil
}

// ---- cookies -----------------------------------------------------------

func (s *Server) setSessionCookies(w http.ResponseWriter, sess identity.Session, csrf string) {
	now := s.cfg.Clock.Now()
	secure := !s.cfg.Cookies.Insecure
	http.SetCookie(w, &http.Cookie{
		Name: CookieAccess, Value: sess.AccessToken, Path: "/", Domain: s.cfg.Cookies.Domain,
		MaxAge: int(sess.ExpiresIn / time.Second), HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name: CookieRefresh, Value: sess.RefreshToken, Path: "/v1/auth", Domain: s.cfg.Cookies.Domain,
		MaxAge: int(sess.RefreshExpires.Sub(now) / time.Second), HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name: CookieCSRF, Value: csrf, Path: "/", Domain: s.cfg.Cookies.Domain,
		MaxAge: int(sess.RefreshExpires.Sub(now) / time.Second), HttpOnly: false, Secure: secure, SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) clearSessionCookies(w http.ResponseWriter) {
	secure := !s.cfg.Cookies.Insecure
	for _, c := range []struct {
		name, path string
		httpOnly   bool
		same       http.SameSite
	}{
		{CookieAccess, "/", true, http.SameSiteLaxMode},
		{CookieRefresh, "/v1/auth", true, http.SameSiteStrictMode},
		{CookieCSRF, "/", false, http.SameSiteStrictMode},
	} {
		http.SetCookie(w, &http.Cookie{Name: c.name, Value: "", Path: c.path, Domain: s.cfg.Cookies.Domain, MaxAge: -1,
			HttpOnly: c.httpOnly, Secure: secure, SameSite: c.same})
	}
}
