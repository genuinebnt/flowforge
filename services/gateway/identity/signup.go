package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/limiter"
)

// SignupInput is the body of POST /auth/signup.
type SignupInput struct {
	Email      string
	Password   string
	TenantName string
}

// SignupResult identifies the pending user and tenant. When the e-mail
// already exists the ids are fresh, unused values, so the response cannot
// tell the two cases apart (spec 13.1).
type SignupResult struct {
	UserID   core.UUID
	TenantID core.UUID
}

func (s *Service) throttle(ctx context.Context, key string, l auth.Limit) error {
	ok, retry, err := s.cfg.Limiter.Allow(ctx, key, l)
	if err != nil {
		// Fail closed (spec 2.4): an unreachable limiter denies, as
		// UNAVAILABLE so clients retry with backoff (11.8).
		return &core.Error{Kind: core.KindUnavailable, Reason: core.ReasonUnavailable, Message: "rate limiter unavailable; retry later", Err: err}
	}
	if !ok {
		return errRateLimited(retry)
	}
	return nil
}

func accountKey(email, endpoint string) string {
	k := limiter.AuthAccountKey(email)
	if endpoint == "" {
		return k
	}
	return k + ":" + endpoint
}

// Signup creates a pending user, a tenant and an owner membership, and
// e-mails a verification token. It always succeeds the same way for an
// existing e-mail, which instead receives a "someone tried to sign up"
// notice.
func (s *Service) Signup(ctx context.Context, in SignupInput, ci ClientInfo) (SignupResult, error) {
	if err := s.throttle(ctx, limiter.AuthIPKey(ci.IP, "signup"), s.cfg.Limits.Signup); err != nil {
		if errors.Is(err, core.ErrExhausted) {
			// auth.signup_throttled feeds the default signup-abuse alert
			// rule (alerts.DefaultRuleSignupAbuse, spec 15.4; G-LIM-07).
			s.recordBestEffort(ctx, EventSignupThrottled, core.Nil, core.Nil, ci, map[string]string{"endpoint": "signup"})
		}
		return SignupResult{}, err
	}
	email, err := NormalizeEmail(in.Email)
	if err != nil {
		return SignupResult{}, err
	}
	name := strings.TrimSpace(in.TenantName)
	if n := utf8.RuneCountInString(name); n < 1 || n > 100 {
		return SignupResult{}, errValidation("tenant_name: must be 1-100 characters")
	}
	if err := s.checkPassword(ctx, in.Password); err != nil {
		return SignupResult{}, err
	}
	// Hash in every branch so both take the same time.
	hash, err := s.cfg.Passwords.Hash(in.Password)
	if err != nil {
		return SignupResult{}, errInternal(err)
	}
	res := SignupResult{UserID: s.newID(), TenantID: s.newID()}

	existing, err := s.cfg.Store.GetUserByEmail(ctx, email)
	switch {
	case err == nil:
		s.signupExisting(ctx, existing, ci)
		return res, nil
	case !errors.Is(err, ErrNotFound):
		return SignupResult{}, errInternal(err)
	}

	now := s.now()
	slug, err := s.slug(name)
	if err != nil {
		return SignupResult{}, errInternal(err)
	}
	var token string
	err = s.cfg.Store.WithTx(ctx, func(tx Store) error {
		if err := tx.CreateTenant(ctx, Tenant{ID: res.TenantID, Name: name, Slug: slug, Plan: "free", CreatedAt: now}); err != nil {
			return err
		}
		if err := tx.CreateUser(ctx, User{
			ID: res.UserID, Email: email, PasswordHash: hash, Status: auth.UserPending, CreatedAt: now, SessionVersion: 1,
		}); err != nil {
			return err
		}
		if err := tx.CreateMembership(ctx, Membership{TenantID: res.TenantID, UserID: res.UserID, Role: auth.RoleOwner, CreatedAt: now}); err != nil {
			return err
		}
		v, err := auth.NewEmailVerifier(verifStore{tx}, s.cfg.Clock, s.cfg.Random)
		if err != nil {
			return err
		}
		if token, err = v.Issue(ctx, res.UserID); err != nil {
			return err
		}
		return s.record(ctx, tx, EventSignup, res.TenantID, res.UserID, ci, map[string]string{"tenant_slug": slug})
	})
	if errors.Is(err, ErrConflict) {
		// Lost a race with a concurrent signup for the same address.
		if u, gerr := s.cfg.Store.GetUserByEmail(ctx, email); gerr == nil {
			s.signupExisting(ctx, u, ci)
		}
		return SignupResult{UserID: s.newID(), TenantID: s.newID()}, nil
	}
	if err != nil {
		return SignupResult{}, errInternal(err)
	}
	s.send(ctx, Email{
		To: email, Kind: EmailVerify, Subject: "Verify your Flowforge e-mail address", Token: token,
		Body: fmt.Sprintf("Confirm your address: %s/verify-email?token=%s (valid 24 hours)", s.cfg.PublicURL, token),
	})
	return res, nil
}

func (s *Service) signupExisting(ctx context.Context, u User, ci ClientInfo) {
	s.send(ctx, Email{
		To: u.Email, Kind: EmailSignupExists, Subject: "Someone tried to sign up with your e-mail address",
		Body: "Someone tried to create a Flowforge account with this address. If it was you, log in or reset your password: " +
			s.cfg.PublicURL + "/password/forgot. Otherwise you can ignore this message.",
	})
	s.recordBestEffort(ctx, EventSignupExisting, core.Nil, u.ID, ci, nil)
}

func (s *Service) slug(name string) (string, error) {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		base = "tenant"
	}
	return fmt.Sprintf("%s-%08x", base, s.ids.Uint64()&0xffffffff), nil
}

// VerifyEmail redeems a verification token and activates the user.
func (s *Service) VerifyEmail(ctx context.Context, token string, ci ClientInfo) (User, error) {
	v, err := auth.NewEmailVerifier(verifStore{s.cfg.Store}, s.cfg.Clock, s.cfg.Random)
	if err != nil {
		return User{}, errInternal(err)
	}
	uid, err := v.Confirm(ctx, token)
	if errors.Is(err, auth.ErrVerificationInvalid) {
		return User{}, errValidation("token: invalid, used or expired")
	}
	if err != nil {
		return User{}, errInternal(err)
	}
	var u User
	err = s.cfg.Store.WithTx(ctx, func(tx Store) error {
		if err := tx.ActivateUser(ctx, uid, s.now()); err != nil {
			return err
		}
		var err error
		if u, err = tx.GetUser(ctx, uid); err != nil {
			return err
		}
		return s.record(ctx, tx, EventEmailVerified, core.Nil, uid, ci, nil)
	})
	if err != nil {
		return User{}, errInternal(err)
	}
	return u, nil
}

// ResendVerification e-mails a new verification token to a pending user.
// The outcome is the same whether or not the address exists.
func (s *Service) ResendVerification(ctx context.Context, rawEmail string, ci ClientInfo) error {
	email, err := NormalizeEmail(rawEmail)
	if err != nil {
		return err
	}
	if err := s.throttle(ctx, accountKey(email, "resend"), s.cfg.Limits.Resend); err != nil {
		return err
	}
	u, err := s.cfg.Store.GetUserByEmail(ctx, email)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return errInternal(err)
	}
	if u.Status != auth.UserPending {
		return nil
	}
	v, err := auth.NewEmailVerifier(verifStore{s.cfg.Store}, s.cfg.Clock, s.cfg.Random)
	if err != nil {
		return errInternal(err)
	}
	tok, err := v.Issue(ctx, u.ID)
	if err != nil {
		return errInternal(err)
	}
	s.send(ctx, Email{
		To: email, Kind: EmailVerify, Subject: "Verify your Flowforge e-mail address", Token: tok,
		Body: fmt.Sprintf("Confirm your address: %s/verify-email?token=%s (valid 24 hours)", s.cfg.PublicURL, tok),
	})
	s.recordBestEffort(ctx, EventVerifyResent, core.Nil, u.ID, ci, nil)
	return nil
}

// ForgotPassword e-mails a password-reset token if the address belongs to
// an account that is not disabled. The outcome is the same either way.
func (s *Service) ForgotPassword(ctx context.Context, rawEmail string, ci ClientInfo) error {
	if err := s.throttle(ctx, limiter.AuthIPKey(ci.IP, "password_forgot"), s.cfg.Limits.Forgot); err != nil {
		return err
	}
	email, err := NormalizeEmail(rawEmail)
	if err != nil {
		return err
	}
	if err := s.throttle(ctx, accountKey(email, "forgot"), s.cfg.Limits.ForgotAccount); err != nil {
		return err
	}
	u, err := s.cfg.Store.GetUserByEmail(ctx, email)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return errInternal(err)
	}
	if u.Status == auth.UserDisabled {
		return nil
	}
	tok, err := s.newSecret()
	if err != nil {
		return errInternal(err)
	}
	if err := s.cfg.Store.InsertVerification(ctx, auth.Verification{
		TokenHash: resetHash(tok), UserID: u.ID, ExpiresAt: s.now().Add(s.cfg.ResetTTL),
	}); err != nil {
		return errInternal(err)
	}
	s.send(ctx, Email{
		To: email, Kind: EmailPasswordReset, Subject: "Reset your Flowforge password", Token: tok,
		Body: fmt.Sprintf("Choose a new password: %s/password/reset?token=%s (valid %s)", s.cfg.PublicURL, tok, s.cfg.ResetTTL),
	})
	s.recordBestEffort(ctx, EventPasswordForgot, core.Nil, u.ID, ci, nil)
	return nil
}

// ResetPassword redeems a reset token: it sets the new password, marks the
// address verified, revokes every session of the user and bumps the
// session version (so outstanding access tokens die too).
func (s *Service) ResetPassword(ctx context.Context, token, password string, ci ClientInfo) error {
	if err := s.checkPassword(ctx, password); err != nil {
		return err
	}
	invalid := errValidation("token: invalid, used or expired")
	if !isSecret(token) {
		return invalid
	}
	hash, err := s.cfg.Passwords.Hash(password)
	if err != nil {
		return errInternal(err)
	}
	now := s.now()
	var uid core.UUID
	var sv int64
	err = s.cfg.Store.WithTx(ctx, func(tx Store) error {
		v, err := tx.ConsumeVerification(ctx, resetHash(token), now)
		if err != nil {
			return err
		}
		uid = v.UserID
		if err := tx.SetPasswordHash(ctx, uid, hash); err != nil {
			return err
		}
		if err := tx.ActivateUser(ctx, uid, now); err != nil {
			return err
		}
		if err := tx.RevokeUserSessions(ctx, uid, now); err != nil {
			return err
		}
		if sv, err = tx.BumpSessionVersion(ctx, uid); err != nil {
			return err
		}
		return s.record(ctx, tx, EventPasswordReset, core.Nil, uid, ci, nil)
	})
	if errors.Is(err, auth.ErrVerificationInvalid) {
		return invalid
	}
	if err != nil {
		return errInternal(err)
	}
	if err := s.publishSessionVersion(ctx, uid, sv); err != nil {
		return errInternal(err)
	}
	if u, err := s.cfg.Store.GetUser(ctx, uid); err == nil {
		_ = s.cfg.Lockouts.Reset(ctx, auth.AccountThrottleKey(u.Email))
	}
	return nil
}
