package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
)

// Profile is the answer to GET /me.
type Profile struct {
	User   *User         // nil for API keys
	APIKey *APIKeyRecord // nil for users
	Tenant Tenant
	Role   auth.Role
}

// Me returns the caller's profile.
func (s *Service) Me(ctx context.Context, a Actor) (Profile, error) {
	t, err := s.cfg.Store.GetTenant(ctx, a.TenantID)
	if errors.Is(err, ErrNotFound) {
		return Profile{}, errNotFound()
	}
	if err != nil {
		return Profile{}, errInternal(err)
	}
	p := Profile{Tenant: t, Role: a.Role}
	if a.KeyID.IsZero() {
		u, err := s.cfg.Store.GetUser(ctx, a.UserID)
		if err != nil {
			return Profile{}, errNotFound()
		}
		p.User = &u
		return p, nil
	}
	k, err := s.cfg.Store.GetAPIKey(ctx, a.TenantID, a.KeyID)
	if err != nil {
		return Profile{}, errNotFound()
	}
	p.APIKey = &k
	return p, nil
}

// TenantPlan returns the tenant's plan ("free" when unknown).
func (s *Service) TenantPlan(ctx context.Context, tenant core.UUID) string {
	t, err := s.cfg.Store.GetTenant(ctx, tenant)
	if err != nil || t.Plan == "" {
		return "free"
	}
	return t.Plan
}

// ListMembers pages through a tenant's members.
func (s *Service) ListMembers(ctx context.Context, tenant core.UUID, pageToken string, size int) ([]Member, string, error) {
	after, err := DecodePageToken(pageToken)
	if err != nil {
		return nil, "", err
	}
	ms, err := s.cfg.Store.ListMembers(ctx, tenant, after, size+1)
	if err != nil {
		return nil, "", errInternal(err)
	}
	next := ""
	if len(ms) > size {
		ms = ms[:size]
		next = EncodePageToken(ms[len(ms)-1].UserID)
	}
	return ms, next, nil
}

// GetMember returns one member of the tenant.
func (s *Service) GetMember(ctx context.Context, tenant, user core.UUID) (Member, error) {
	m, err := s.cfg.Store.GetMembership(ctx, tenant, user)
	if errors.Is(err, ErrNotFound) {
		return Member{}, errNotFound()
	}
	if err != nil {
		return Member{}, errInternal(err)
	}
	u, err := s.cfg.Store.GetUser(ctx, user)
	if err != nil {
		return Member{}, errInternal(err)
	}
	return Member{Membership: m, Email: u.Email, Status: u.Status}, nil
}

// SetMemberRole changes a member's role. Granting or removing the owner
// role needs tenant:owner, and the last owner cannot be demoted. The
// member's session version is bumped so tokens with the old role die.
func (s *Service) SetMemberRole(ctx context.Context, a Actor, user core.UUID, role auth.Role) (Member, error) {
	if !role.Valid() {
		return Member{}, errValidation("role: must be one of viewer, analyst, approver, admin, owner")
	}
	var sv int64
	var changed bool
	err := s.cfg.Store.WithTx(ctx, func(tx Store) error {
		m, err := tx.GetMembership(ctx, a.TenantID, user)
		if errors.Is(err, ErrNotFound) {
			return errNotFound()
		}
		if err != nil {
			return err
		}
		if (role == auth.RoleOwner || m.Role == auth.RoleOwner) && !auth.Can(a.Role, auth.PermTenantOwner) {
			return errForbidden("only an owner can grant or remove the owner role")
		}
		if m.Role == role {
			return nil
		}
		if m.Role == auth.RoleOwner {
			n, err := tx.CountOwners(ctx, a.TenantID)
			if err != nil {
				return err
			}
			if n <= 1 {
				return errValidation("role: a tenant must keep at least one owner")
			}
		}
		if err := tx.SetMemberRole(ctx, a.TenantID, user, role); err != nil {
			return err
		}
		if sv, err = tx.BumpSessionVersion(ctx, user); err != nil {
			return err
		}
		changed = true
		return s.record(ctx, tx, EventRoleChanged, a.TenantID, user, a.Client, map[string]string{
			"from": string(m.Role), "to": string(role), "by": actorName(a),
		})
	})
	if err != nil {
		return Member{}, passOrInternal(err)
	}
	if changed {
		if err := s.publishSessionVersion(ctx, user, sv); err != nil {
			return Member{}, errInternal(err)
		}
	}
	return s.GetMember(ctx, a.TenantID, user)
}

func actorName(a Actor) string {
	if !a.KeyID.IsZero() {
		return "apikey:" + core.PublicID(core.PrefixAPIKey, a.KeyID)
	}
	return "user:" + core.PublicID(core.PrefixUser, a.UserID)
}

// passOrInternal returns client-facing errors unchanged and wraps the rest.
func passOrInternal(err error) error {
	var ce *core.Error
	if errors.As(err, &ce) {
		return ce
	}
	return errInternal(err)
}

// CreateInvite invites an address into the actor's tenant; the token goes
// out by e-mail only.
func (s *Service) CreateInvite(ctx context.Context, a Actor, rawEmail string, role auth.Role) (Invite, error) {
	email, err := NormalizeEmail(rawEmail)
	if err != nil {
		return Invite{}, err
	}
	if !role.Valid() {
		return Invite{}, errValidation("role: must be one of viewer, analyst, approver, admin, owner")
	}
	if role == auth.RoleOwner && !auth.Can(a.Role, auth.PermTenantOwner) {
		return Invite{}, errForbidden("only an owner can invite owners")
	}
	if u, err := s.cfg.Store.GetUserByEmail(ctx, email); err == nil {
		if _, err := s.cfg.Store.GetMembership(ctx, a.TenantID, u.ID); err == nil {
			return Invite{}, core.Errorf(core.KindAlreadyExists, core.ReasonAlreadyExists, "%s is already a member", email)
		}
	}
	tok, err := s.newSecret()
	if err != nil {
		return Invite{}, errInternal(err)
	}
	now := s.now()
	inv := Invite{
		ID: s.newID(), TenantID: a.TenantID, Email: email, Role: role, TokenHash: auth.HashToken(tok),
		ExpiresAt: now.Add(s.cfg.InviteTTL), CreatedBy: a.UserID, CreatedAt: now,
	}
	err = s.cfg.Store.WithTx(ctx, func(tx Store) error {
		if err := tx.CreateInvite(ctx, inv); err != nil {
			return err
		}
		return s.record(ctx, tx, EventInviteCreated, a.TenantID, a.UserID, a.Client, map[string]string{"role": string(role), "by": actorName(a)})
	})
	if err != nil {
		return Invite{}, errInternal(err)
	}
	t, _ := s.cfg.Store.GetTenant(ctx, a.TenantID)
	s.send(ctx, Email{
		To: email, Kind: EmailInvite, Subject: "You are invited to " + t.Name + " on Flowforge", Token: tok,
		Body: fmt.Sprintf("Join %s as %s: %s/invites/accept?token=%s (valid until %s)", t.Name, role, s.cfg.PublicURL, tok, core.FormatTime(inv.ExpiresAt)),
	})
	return inv, nil
}

// AcceptedInvite is the outcome of AcceptInvite.
type AcceptedInvite struct {
	UserID   core.UUID
	TenantID core.UUID
	Role     auth.Role
	Created  bool // a new user was created
}

// AcceptInvite redeems an invite token. An unknown address becomes a new
// active user (the token proves the address) with password; an existing
// user just gains the membership.
func (s *Service) AcceptInvite(ctx context.Context, token, password string, ci ClientInfo) (AcceptedInvite, error) {
	invalid := errValidation("token: invalid, used or expired")
	if !isSecret(token) {
		return AcceptedInvite{}, invalid
	}
	inv, err := s.cfg.Store.GetInviteByHash(ctx, auth.HashToken(token))
	if errors.Is(err, ErrNotFound) {
		return AcceptedInvite{}, invalid
	}
	if err != nil {
		return AcceptedInvite{}, errInternal(err)
	}
	now := s.now()
	if !inv.AcceptedAt.IsZero() || !now.Before(inv.ExpiresAt) {
		return AcceptedInvite{}, invalid
	}
	existing, err := s.cfg.Store.GetUserByEmail(ctx, inv.Email)
	known := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return AcceptedInvite{}, errInternal(err)
	}
	var hash string
	if !known {
		if password == "" {
			return AcceptedInvite{}, errValidation("password: required to create the account")
		}
		if err := s.checkPassword(ctx, password); err != nil {
			return AcceptedInvite{}, err
		}
		if hash, err = s.cfg.Passwords.Hash(password); err != nil {
			return AcceptedInvite{}, errInternal(err)
		}
	}
	out := AcceptedInvite{TenantID: inv.TenantID, Role: inv.Role, Created: !known}
	err = s.cfg.Store.WithTx(ctx, func(tx Store) error {
		ok, err := tx.AcceptInvite(ctx, inv.ID, now)
		if err != nil {
			return err
		}
		if !ok {
			return invalid
		}
		if known {
			out.UserID = existing.ID
			if err := tx.ActivateUser(ctx, existing.ID, now); err != nil {
				return err
			}
		} else {
			out.UserID = s.newID()
			if err := tx.CreateUser(ctx, User{
				ID: out.UserID, Email: inv.Email, EmailVerifiedAt: now, PasswordHash: hash,
				Status: auth.UserActive, CreatedAt: now, SessionVersion: 1,
			}); err != nil {
				return err
			}
		}
		if err := tx.CreateMembership(ctx, Membership{TenantID: inv.TenantID, UserID: out.UserID, Role: inv.Role, CreatedAt: now}); err != nil {
			if errors.Is(err, ErrConflict) {
				return core.Errorf(core.KindAlreadyExists, core.ReasonAlreadyExists, "already a member of this tenant")
			}
			return err
		}
		return s.record(ctx, tx, EventInviteAccepted, inv.TenantID, out.UserID, ci, map[string]string{"role": string(inv.Role)})
	})
	if err != nil {
		return AcceptedInvite{}, passOrInternal(err)
	}
	return out, nil
}

// ---- API keys ----------------------------------------------------------

// CreatedAPIKey is a new key with its secret, which is never shown again.
type CreatedAPIKey struct {
	Record APIKeyRecord
	Token  string
}

// CreateAPIKey creates a tenant API key. Keys may hold any role up to
// admin; owner keys are refused (owner operations need a person).
func (s *Service) CreateAPIKey(ctx context.Context, a Actor, name string, role auth.Role, expiresAt time.Time) (CreatedAPIKey, error) {
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n < 1 || n > 100 {
		return CreatedAPIKey{}, errValidation("name: must be 1-100 characters")
	}
	if !role.Valid() || role == auth.RoleOwner {
		return CreatedAPIKey{}, errValidation("role: must be one of viewer, analyst, approver, admin")
	}
	now := s.now()
	if !expiresAt.IsZero() && !expiresAt.After(now) {
		return CreatedAPIKey{}, errValidation("expires_at: must be in the future")
	}
	id := s.newID()
	k, err := auth.NewAPIKey(s.cfg.Random, APIKeyID(id))
	if err != nil {
		return CreatedAPIKey{}, errInternal(err)
	}
	rec := APIKeyRecord{
		ID: id, TenantID: a.TenantID, Name: name, Prefix: k.Prefix, SecretHash: k.SecretHash, Role: role,
		CreatedBy: a.UserID, CreatedAt: now, ExpiresAt: expiresAt,
	}
	err = s.cfg.Store.WithTx(ctx, func(tx Store) error {
		if err := tx.CreateAPIKey(ctx, rec); err != nil {
			return err
		}
		return s.record(ctx, tx, EventAPIKeyCreated, a.TenantID, a.UserID, a.Client, map[string]string{
			"key_id": rec.PublicID(), "role": string(role), "by": actorName(a),
		})
	})
	if err != nil {
		return CreatedAPIKey{}, errInternal(err)
	}
	return CreatedAPIKey{Record: rec, Token: k.Token}, nil
}

// ListAPIKeys pages through a tenant's API keys.
func (s *Service) ListAPIKeys(ctx context.Context, tenant core.UUID, pageToken string, size int) ([]APIKeyRecord, string, error) {
	after, err := DecodePageToken(pageToken)
	if err != nil {
		return nil, "", err
	}
	ks, err := s.cfg.Store.ListAPIKeys(ctx, tenant, after, size+1)
	if err != nil {
		return nil, "", errInternal(err)
	}
	next := ""
	if len(ks) > size {
		ks = ks[:size]
		next = EncodePageToken(ks[len(ks)-1].ID)
	}
	return ks, next, nil
}

// RevokeAPIKey revokes a key of the actor's tenant.
func (s *Service) RevokeAPIKey(ctx context.Context, a Actor, id core.UUID) error {
	err := s.cfg.Store.WithTx(ctx, func(tx Store) error {
		ok, err := tx.RevokeAPIKey(ctx, a.TenantID, id, s.now())
		if err != nil {
			return err
		}
		if !ok {
			return errNotFound()
		}
		return s.record(ctx, tx, EventAPIKeyRevoked, a.TenantID, a.UserID, a.Client, map[string]string{
			"key_id": core.PublicID(core.PrefixAPIKey, id), "by": actorName(a),
		})
	})
	return passOrInternalOrNil(err)
}

func passOrInternalOrNil(err error) error {
	if err == nil {
		return nil
	}
	return passOrInternal(err)
}

// AuthenticateAPIKey resolves a presented ffk_ token (spec 13.2): the
// prefix finds the row, the SHA-256 of the secret must match, and the key
// must be unrevoked, unexpired and in an enabled tenant. Every failure is
// UNAUTHENTICATED (TOKEN_EXPIRED for an expired key).
func (s *Service) AuthenticateAPIKey(ctx context.Context, token string) (APIKeyRecord, error) {
	invalid := errUnauthenticated(core.ReasonTokenInvalid, "API key is invalid")
	p, err := auth.ParseAPIKey(token)
	if err != nil {
		return APIKeyRecord{}, invalid
	}
	k, err := s.cfg.Store.GetAPIKeyByPrefix(ctx, p.Prefix)
	if errors.Is(err, ErrNotFound) {
		return APIKeyRecord{}, invalid
	}
	if err != nil {
		return APIKeyRecord{}, errInternal(err)
	}
	now := s.now()
	// The secret is checked first, so the revoked and expired messages
	// are only ever shown to a holder of the real secret.
	switch {
	case !p.Matches(k.SecretHash[:]):
		return APIKeyRecord{}, invalid
	case !k.RevokedAt.IsZero():
		return APIKeyRecord{}, errUnauthenticated(core.ReasonTokenInvalid, "API key has been revoked")
	case !k.ExpiresAt.IsZero() && !now.Before(k.ExpiresAt):
		return APIKeyRecord{}, errUnauthenticated(core.ReasonTokenExpired, "API key has expired")
	}
	if t, err := s.cfg.Store.GetTenant(ctx, k.TenantID); err != nil || !t.DisabledAt.IsZero() {
		return APIKeyRecord{}, invalid
	}
	// last_used_at is written at most once a minute per key, so a busy key
	// does not turn every request into a row update.
	if k.LastUsedAt.IsZero() || now.Sub(k.LastUsedAt) >= time.Minute {
		if err := s.cfg.Store.TouchAPIKey(ctx, k.ID, now); err == nil {
			k.LastUsedAt = now
		}
	}
	return k, nil
}
