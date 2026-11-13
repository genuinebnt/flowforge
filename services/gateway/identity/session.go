package identity

import (
	"context"
	"errors"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
)

// Session is the result of a login or refresh: an access token and the
// rotated refresh token of the session family.
type Session struct {
	AccessToken    string
	Claims         auth.Claims
	ExpiresIn      time.Duration
	RefreshToken   string
	RefreshExpires time.Time
	User           User
	Tenant         Tenant
	Role           auth.Role
	SessionID      core.UUID
}

// LoginInput is the body of POST /auth/login. TenantID selects one of the
// user's tenants; zero means the oldest membership.
type LoginInput struct {
	Email    string
	Password string
	TenantID core.UUID
}

// Login exchanges credentials for a session (spec 13.1, 13.2). Unknown
// e-mails, wrong passwords and missing memberships all fail with the same
// error after the same amount of hashing; throttles and lockouts apply to
// unknown e-mails exactly as to known ones.
func (s *Service) Login(ctx context.Context, in LoginInput, ci ClientInfo) (Session, error) {
	res, err := s.login.Login(ctx, auth.LoginRequest{Email: in.Email, Password: in.Password, IP: ci.IP})
	switch {
	case errors.Is(err, auth.ErrThrottled):
		s.recordBestEffort(ctx, EventLoginThrottled, core.Nil, core.Nil, ci, map[string]string{"account": auth.AccountThrottleKey(in.Email)})
		return Session{}, throttleErr(err)
	case errors.Is(err, auth.ErrInvalidCredentials):
		s.recordBestEffort(ctx, EventLoginFailed, core.Nil, core.Nil, ci, map[string]string{"account": auth.AccountThrottleKey(in.Email)})
		return Session{}, errInvalidCredentials()
	case errors.Is(err, auth.ErrPendingUser):
		// Only reachable with the right password, so it reveals nothing.
		return Session{}, errUnauthenticated(core.ReasonTokenInvalid, "email address not verified")
	case errors.Is(err, auth.ErrUserDisabled):
		return Session{}, errInvalidCredentials()
	case err != nil:
		return Session{}, errInternal(err)
	}
	user, err := s.cfg.Store.GetUser(ctx, res.User.ID)
	if err != nil {
		return Session{}, errInternal(err)
	}
	m, err := s.pickMembership(ctx, user.ID, in.TenantID)
	if err != nil {
		return Session{}, err
	}
	sess, err := s.startSession(ctx, user, m)
	if err != nil {
		return Session{}, err
	}
	_ = s.cfg.Store.SetLastLogin(ctx, user.ID, s.now())
	s.recordBestEffort(ctx, EventLogin, m.TenantID, user.ID, ci, map[string]string{"session_id": core.PublicID(core.PrefixSession, sess.SessionID)})
	return sess, nil
}

func (s *Service) pickMembership(ctx context.Context, user, tenant core.UUID) (Membership, error) {
	var m Membership
	if !tenant.IsZero() {
		var err error
		m, err = s.cfg.Store.GetMembership(ctx, tenant, user)
		if errors.Is(err, ErrNotFound) {
			return Membership{}, errInvalidCredentials()
		}
		if err != nil {
			return Membership{}, errInternal(err)
		}
	} else {
		ms, err := s.cfg.Store.UserMemberships(ctx, user)
		if err != nil {
			return Membership{}, errInternal(err)
		}
		if len(ms) == 0 {
			return Membership{}, errInvalidCredentials()
		}
		m = ms[0]
	}
	t, err := s.cfg.Store.GetTenant(ctx, m.TenantID)
	if err != nil || !t.DisabledAt.IsZero() {
		return Membership{}, errInvalidCredentials()
	}
	return m, nil
}

func (s *Service) startSession(ctx context.Context, user User, m Membership) (Session, error) {
	issued, err := s.refresher.Start(ctx, user.ID, m.TenantID)
	if err != nil {
		return Session{}, errInternal(err)
	}
	return s.finish(ctx, user, m, issued)
}

func (s *Service) finish(ctx context.Context, user User, m Membership, issued auth.IssuedRefresh) (Session, error) {
	t, err := s.cfg.Store.GetTenant(ctx, m.TenantID)
	if err != nil {
		return Session{}, errInternal(err)
	}
	tok, claims, err := s.cfg.Keys.Minter().Access(auth.AccessGrant{
		UserID: user.ID, TenantID: m.TenantID, Role: m.Role, SessionID: issued.Row.FamilyID, SessionVersion: user.SessionVersion,
	})
	if err != nil {
		return Session{}, errInternal(err)
	}
	return Session{
		AccessToken: tok, Claims: claims, ExpiresIn: time.Duration(claims.ExpiresAt-claims.IssuedAt) * time.Second,
		RefreshToken: issued.Token, RefreshExpires: issued.Row.ExpiresAt,
		User: user, Tenant: t, Role: m.Role, SessionID: issued.Row.FamilyID,
	}, nil
}

// Refresh rotates a refresh token (spec 13.4) and mints a new access token
// with the user's current role and session version. Presenting a rotated
// token again revokes the whole family and bumps the session version. A
// session whose membership is gone, whose user is no longer active or
// whose tenant is disabled is revoked and fails like an invalid token, as
// Login refuses those (GAPS G-SVC-21).
func (s *Service) Refresh(ctx context.Context, token string) (Session, error) {
	issued, err := s.refresher.Rotate(ctx, token)
	switch {
	case errors.Is(err, auth.ErrRefreshExpired):
		return Session{}, errUnauthenticated(core.ReasonTokenExpired, "refresh token has expired")
	case errors.Is(err, auth.ErrInvalidRefresh):
		return Session{}, errUnauthenticated(core.ReasonTokenInvalid, "refresh token is invalid")
	case err != nil:
		return Session{}, errInternal(err)
	}
	row := issued.Row
	user, err := s.cfg.Store.GetUser(ctx, row.UserID)
	if err != nil {
		return Session{}, errInternal(err)
	}
	m, err := s.cfg.Store.GetMembership(ctx, row.TenantID, row.UserID)
	t, terr := s.cfg.Store.GetTenant(ctx, row.TenantID)
	if errors.Is(err, ErrNotFound) || errors.Is(terr, ErrNotFound) || user.Status != auth.UserActive || (terr == nil && !t.DisabledAt.IsZero()) {
		_ = s.refresher.RevokeSession(ctx, row.FamilyID)
		return Session{}, errUnauthenticated(core.ReasonTokenInvalid, "refresh token is invalid")
	}
	if err != nil {
		return Session{}, errInternal(err)
	}
	if terr != nil {
		return Session{}, errInternal(terr)
	}
	return s.finish(ctx, user, m, issued)
}

// LogoutInput identifies the session to end: the access token's claims
// (sid, jti, exp), a refresh token, or both.
type LogoutInput struct {
	Claims       *auth.Claims
	RefreshToken string
}

// Logout revokes the session family and denies the presented access token
// until it expires.
func (s *Service) Logout(ctx context.Context, in LogoutInput, ci ClientInfo) error {
	now := s.now()
	var tenant, user core.UUID
	if c := in.Claims; c != nil {
		if c.SessionID != "" {
			fam, err := core.ParsePublicID(core.PrefixSession, c.SessionID)
			if err == nil {
				if err := s.cfg.Store.RevokeFamily(ctx, fam, now); err != nil {
					return errInternal(err)
				}
			}
		}
		if ttl := c.Expiry().Add(auth.DefaultSkew).Sub(now); ttl > 0 {
			if err := s.cfg.Revocations.DenyJTI(ctx, c.ID, ttl); err != nil {
				return errInternal(err)
			}
		}
		tenant, _ = core.ParsePublicID(core.PrefixTenant, c.TenantID)
		user, _ = core.ParsePublicID(core.PrefixUser, c.Subject)
	}
	if in.RefreshToken != "" && auth.IsRefreshTokenFormat(in.RefreshToken) {
		row, err := s.cfg.Store.FindRefreshToken(ctx, auth.HashToken(in.RefreshToken))
		switch {
		// A refresh token of another user than the access token's is
		// ignored: one user's logout cannot end someone else's session.
		case err == nil && (user.IsZero() || row.UserID == user):
			if err := s.cfg.Store.RevokeFamily(ctx, row.FamilyID, now); err != nil {
				return errInternal(err)
			}
			tenant, user = row.TenantID, row.UserID
		case err != nil && !errors.Is(err, ErrNotFound):
			return errInternal(err)
		}
	}
	if !user.IsZero() {
		s.recordBestEffort(ctx, EventLogout, tenant, user, ci, nil)
	}
	return nil
}

// RevokeAllSessions ends every session of a user (all refresh families and,
// through the session version, all access tokens).
func (s *Service) RevokeAllSessions(ctx context.Context, user core.UUID, ci ClientInfo) error {
	var sv int64
	err := s.cfg.Store.WithTx(ctx, func(tx Store) error {
		if err := tx.RevokeUserSessions(ctx, user, s.now()); err != nil {
			return err
		}
		var err error
		if sv, err = tx.BumpSessionVersion(ctx, user); err != nil {
			return err
		}
		return s.record(ctx, tx, EventSessionsRevoked, core.Nil, user, ci, nil)
	})
	if err != nil {
		return errInternal(err)
	}
	if err := s.publishSessionVersion(ctx, user, sv); err != nil {
		return errInternal(err)
	}
	return nil
}
