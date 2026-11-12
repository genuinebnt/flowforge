package identity

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
)

// MemStore is the in-memory Store. Operations are serialized by one mutex;
// WithTx holds it for the whole callback, so a transaction is isolated but
// not rolled back on error (callers validate before writing). It is safe
// for concurrent use.
type MemStore struct {
	mu   *sync.Mutex
	d    *memData
	inTx bool
}

type memData struct {
	tenants     map[core.UUID]Tenant
	users       map[core.UUID]User
	byEmail     map[string]core.UUID
	members     map[[2]core.UUID]Membership // tenant, user
	invites     map[core.UUID]Invite
	verifs      map[[32]byte]auth.Verification
	refresh     map[core.UUID]auth.RefreshToken
	refreshHash map[[32]byte]core.UUID
	keys        map[core.UUID]APIKeyRecord
	keyPrefix   map[string]core.UUID
	signing     map[string]SigningKeyRecord
	audit       []AuditRecord
	outbox      []OutboxEvent
	idem        map[idemKey]IdemRecord
}

type idemKey struct {
	tenant core.UUID
	key    string
}

var _ Store = (*MemStore)(nil)

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{mu: &sync.Mutex{}, d: &memData{
		tenants: map[core.UUID]Tenant{}, users: map[core.UUID]User{}, byEmail: map[string]core.UUID{},
		members: map[[2]core.UUID]Membership{}, invites: map[core.UUID]Invite{},
		verifs: map[[32]byte]auth.Verification{}, refresh: map[core.UUID]auth.RefreshToken{},
		refreshHash: map[[32]byte]core.UUID{}, keys: map[core.UUID]APIKeyRecord{}, keyPrefix: map[string]core.UUID{},
		signing: map[string]SigningKeyRecord{}, idem: map[idemKey]IdemRecord{},
	}}
}

func (s *MemStore) lock() func() {
	if s.inTx {
		return func() {}
	}
	s.mu.Lock()
	return s.mu.Unlock
}

func notFound(what string) error { return fmt.Errorf("%s: %w", what, ErrNotFound) }
func conflict(what string) error { return fmt.Errorf("%s: %w", what, ErrConflict) }

// WithTx implements Store.
func (s *MemStore) WithTx(_ context.Context, fn func(Store) error) error {
	if s.inTx {
		return fn(s)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(&MemStore{mu: s.mu, d: s.d, inTx: true})
}

// Ping implements Store.
func (s *MemStore) Ping(context.Context) error { return nil }

// CreateTenant implements Store.
func (s *MemStore) CreateTenant(_ context.Context, t Tenant) error {
	defer s.lock()()
	if _, ok := s.d.tenants[t.ID]; ok {
		return conflict("tenant")
	}
	for _, o := range s.d.tenants {
		if strings.EqualFold(o.Slug, t.Slug) {
			return conflict("tenant slug")
		}
	}
	s.d.tenants[t.ID] = t
	return nil
}

// GetTenant implements Store.
func (s *MemStore) GetTenant(_ context.Context, id core.UUID) (Tenant, error) {
	defer s.lock()()
	t, ok := s.d.tenants[id]
	if !ok {
		return Tenant{}, notFound("tenant")
	}
	return t, nil
}

// CreateUser implements Store.
func (s *MemStore) CreateUser(_ context.Context, u User) error {
	defer s.lock()()
	e := strings.ToLower(u.Email)
	if _, ok := s.d.byEmail[e]; ok {
		return conflict("user email")
	}
	if _, ok := s.d.users[u.ID]; ok {
		return conflict("user")
	}
	if u.SessionVersion == 0 {
		u.SessionVersion = 1
	}
	s.d.users[u.ID] = u
	s.d.byEmail[e] = u.ID
	return nil
}

// GetUser implements Store.
func (s *MemStore) GetUser(_ context.Context, id core.UUID) (User, error) {
	defer s.lock()()
	u, ok := s.d.users[id]
	if !ok {
		return User{}, notFound("user")
	}
	return u, nil
}

// GetUserByEmail implements Store.
func (s *MemStore) GetUserByEmail(_ context.Context, email string) (User, error) {
	defer s.lock()()
	id, ok := s.d.byEmail[strings.ToLower(email)]
	if !ok {
		return User{}, notFound("user")
	}
	return s.d.users[id], nil
}

func (s *MemStore) updateUser(id core.UUID, f func(*User)) error {
	defer s.lock()()
	u, ok := s.d.users[id]
	if !ok {
		return notFound("user")
	}
	f(&u)
	s.d.users[id] = u
	return nil
}

// SetPasswordHash implements Store.
func (s *MemStore) SetPasswordHash(_ context.Context, id core.UUID, hash string) error {
	return s.updateUser(id, func(u *User) { u.PasswordHash = hash })
}

// ActivateUser implements Store.
func (s *MemStore) ActivateUser(_ context.Context, id core.UUID, at time.Time) error {
	return s.updateUser(id, func(u *User) {
		if u.Status == auth.UserPending {
			u.Status = auth.UserActive
		}
		if u.EmailVerifiedAt.IsZero() {
			u.EmailVerifiedAt = at
		}
	})
}

// SetLastLogin implements Store.
func (s *MemStore) SetLastLogin(_ context.Context, id core.UUID, at time.Time) error {
	return s.updateUser(id, func(u *User) { u.LastLoginAt = at })
}

// BumpSessionVersion implements Store.
func (s *MemStore) BumpSessionVersion(_ context.Context, id core.UUID) (int64, error) {
	var v int64
	err := s.updateUser(id, func(u *User) { u.SessionVersion++; v = u.SessionVersion })
	return v, err
}

// CreateMembership implements Store.
func (s *MemStore) CreateMembership(_ context.Context, m Membership) error {
	defer s.lock()()
	k := [2]core.UUID{m.TenantID, m.UserID}
	if _, ok := s.d.members[k]; ok {
		return conflict("membership")
	}
	if _, ok := s.d.tenants[m.TenantID]; !ok {
		return notFound("tenant")
	}
	if _, ok := s.d.users[m.UserID]; !ok {
		return notFound("user")
	}
	s.d.members[k] = m
	return nil
}

// GetMembership implements Store.
func (s *MemStore) GetMembership(_ context.Context, tenant, user core.UUID) (Membership, error) {
	defer s.lock()()
	m, ok := s.d.members[[2]core.UUID{tenant, user}]
	if !ok {
		return Membership{}, notFound("membership")
	}
	return m, nil
}

// UserMemberships implements Store.
func (s *MemStore) UserMemberships(_ context.Context, user core.UUID) ([]Membership, error) {
	defer s.lock()()
	var out []Membership
	for _, m := range s.d.members {
		if m.UserID == user {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b Membership) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return bytes.Compare(a.TenantID[:], b.TenantID[:])
	})
	return out, nil
}

// ListMembers implements Store.
func (s *MemStore) ListMembers(_ context.Context, tenant, after core.UUID, limit int) ([]Member, error) {
	defer s.lock()()
	var out []Member
	for _, m := range s.d.members {
		if m.TenantID == tenant && bytes.Compare(m.UserID[:], after[:]) > 0 {
			u := s.d.users[m.UserID]
			out = append(out, Member{Membership: m, Email: u.Email, Status: u.Status})
		}
	}
	slices.SortFunc(out, func(a, b Member) int { return bytes.Compare(a.UserID[:], b.UserID[:]) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SetMemberRole implements Store.
func (s *MemStore) SetMemberRole(_ context.Context, tenant, user core.UUID, role auth.Role) error {
	defer s.lock()()
	k := [2]core.UUID{tenant, user}
	m, ok := s.d.members[k]
	if !ok {
		return notFound("membership")
	}
	m.Role = role
	s.d.members[k] = m
	return nil
}

// CountOwners implements Store.
func (s *MemStore) CountOwners(_ context.Context, tenant core.UUID) (int, error) {
	defer s.lock()()
	n := 0
	for _, m := range s.d.members {
		if m.TenantID == tenant && m.Role == auth.RoleOwner {
			n++
		}
	}
	return n, nil
}

// CreateInvite implements Store.
func (s *MemStore) CreateInvite(_ context.Context, inv Invite) error {
	defer s.lock()()
	for _, o := range s.d.invites {
		if o.TokenHash == inv.TokenHash {
			return conflict("invite token")
		}
	}
	s.d.invites[inv.ID] = inv
	return nil
}

// GetInviteByHash implements Store.
func (s *MemStore) GetInviteByHash(_ context.Context, hash [32]byte) (Invite, error) {
	defer s.lock()()
	for _, inv := range s.d.invites {
		if inv.TokenHash == hash {
			return inv, nil
		}
	}
	return Invite{}, notFound("invite")
}

// AcceptInvite implements Store.
func (s *MemStore) AcceptInvite(_ context.Context, id core.UUID, at time.Time) (bool, error) {
	defer s.lock()()
	inv, ok := s.d.invites[id]
	if !ok {
		return false, notFound("invite")
	}
	if !inv.AcceptedAt.IsZero() {
		return false, nil
	}
	inv.AcceptedAt = at
	s.d.invites[id] = inv
	return true, nil
}

// InsertVerification implements Store.
func (s *MemStore) InsertVerification(_ context.Context, v auth.Verification) error {
	defer s.lock()()
	if _, ok := s.d.verifs[v.TokenHash]; ok {
		return conflict("verification")
	}
	s.d.verifs[v.TokenHash] = v
	return nil
}

// ConsumeVerification implements Store.
func (s *MemStore) ConsumeVerification(_ context.Context, hash [32]byte, now time.Time) (auth.Verification, error) {
	defer s.lock()()
	v, ok := s.d.verifs[hash]
	if !ok || !v.UsedAt.IsZero() || !now.Before(v.ExpiresAt) {
		return auth.Verification{}, auth.ErrVerificationInvalid
	}
	v.UsedAt = now
	s.d.verifs[hash] = v
	return v, nil
}

// InsertRefreshToken implements Store.
func (s *MemStore) InsertRefreshToken(_ context.Context, t auth.RefreshToken) error {
	defer s.lock()()
	if _, ok := s.d.refresh[t.ID]; ok {
		return conflict("refresh token")
	}
	if _, ok := s.d.refreshHash[t.TokenHash]; ok {
		return conflict("refresh token hash")
	}
	s.d.refresh[t.ID] = t
	s.d.refreshHash[t.TokenHash] = t.ID
	return nil
}

// FindRefreshToken implements Store.
func (s *MemStore) FindRefreshToken(_ context.Context, hash [32]byte) (auth.RefreshToken, error) {
	defer s.lock()()
	id, ok := s.d.refreshHash[hash]
	if !ok {
		return auth.RefreshToken{}, notFound("refresh token")
	}
	return s.d.refresh[id], nil
}

// MarkRefreshUsed implements Store.
func (s *MemStore) MarkRefreshUsed(_ context.Context, id core.UUID, at time.Time) (bool, error) {
	defer s.lock()()
	t, ok := s.d.refresh[id]
	if !ok {
		return false, notFound("refresh token")
	}
	if !t.UsedAt.IsZero() {
		return false, nil
	}
	t.UsedAt = at
	s.d.refresh[id] = t
	return true, nil
}

// RevokeFamily implements Store.
func (s *MemStore) RevokeFamily(_ context.Context, family core.UUID, at time.Time) error {
	defer s.lock()()
	for id, t := range s.d.refresh {
		if t.FamilyID == family && t.RevokedAt.IsZero() {
			t.RevokedAt = at
			s.d.refresh[id] = t
		}
	}
	return nil
}

// RevokeUserSessions implements Store.
func (s *MemStore) RevokeUserSessions(_ context.Context, user core.UUID, at time.Time) error {
	defer s.lock()()
	for id, t := range s.d.refresh {
		if t.UserID == user && t.RevokedAt.IsZero() {
			t.RevokedAt = at
			s.d.refresh[id] = t
		}
	}
	return nil
}

// CreateAPIKey implements Store.
func (s *MemStore) CreateAPIKey(_ context.Context, k APIKeyRecord) error {
	defer s.lock()()
	if _, ok := s.d.keyPrefix[k.Prefix]; ok {
		return conflict("api key prefix")
	}
	if _, ok := s.d.keys[k.ID]; ok {
		return conflict("api key")
	}
	s.d.keys[k.ID] = k
	s.d.keyPrefix[k.Prefix] = k.ID
	return nil
}

// GetAPIKeyByPrefix implements Store.
func (s *MemStore) GetAPIKeyByPrefix(_ context.Context, prefix string) (APIKeyRecord, error) {
	defer s.lock()()
	id, ok := s.d.keyPrefix[prefix]
	if !ok {
		return APIKeyRecord{}, notFound("api key")
	}
	return s.d.keys[id], nil
}

// GetAPIKey implements Store.
func (s *MemStore) GetAPIKey(_ context.Context, tenant, id core.UUID) (APIKeyRecord, error) {
	defer s.lock()()
	k, ok := s.d.keys[id]
	if !ok || k.TenantID != tenant {
		return APIKeyRecord{}, notFound("api key")
	}
	return k, nil
}

// ListAPIKeys implements Store.
func (s *MemStore) ListAPIKeys(_ context.Context, tenant, after core.UUID, limit int) ([]APIKeyRecord, error) {
	defer s.lock()()
	var out []APIKeyRecord
	for _, k := range s.d.keys {
		if k.TenantID == tenant && bytes.Compare(k.ID[:], after[:]) > 0 {
			out = append(out, k)
		}
	}
	slices.SortFunc(out, func(a, b APIKeyRecord) int { return bytes.Compare(a.ID[:], b.ID[:]) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// RevokeAPIKey implements Store.
func (s *MemStore) RevokeAPIKey(_ context.Context, tenant, id core.UUID, at time.Time) (bool, error) {
	defer s.lock()()
	k, ok := s.d.keys[id]
	if !ok || k.TenantID != tenant || !k.RevokedAt.IsZero() {
		return false, nil
	}
	k.RevokedAt = at
	s.d.keys[id] = k
	return true, nil
}

// TouchAPIKey implements Store.
func (s *MemStore) TouchAPIKey(_ context.Context, id core.UUID, at time.Time) error {
	defer s.lock()()
	k, ok := s.d.keys[id]
	if !ok {
		return notFound("api key")
	}
	k.LastUsedAt = at
	s.d.keys[id] = k
	return nil
}

// PutSigningKey implements Store.
func (s *MemStore) PutSigningKey(_ context.Context, k SigningKeyRecord) error {
	defer s.lock()()
	if _, ok := s.d.signing[k.KID]; ok {
		return conflict("signing key")
	}
	s.d.signing[k.KID] = k
	return nil
}

// ListSigningKeys implements Store.
func (s *MemStore) ListSigningKeys(context.Context) ([]SigningKeyRecord, error) {
	defer s.lock()()
	out := make([]SigningKeyRecord, 0, len(s.d.signing))
	for _, k := range s.d.signing {
		out = append(out, k)
	}
	slices.SortFunc(out, func(a, b SigningKeyRecord) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.KID, b.KID)
	})
	return out, nil
}

// SetSigningKeyStatus implements Store.
func (s *MemStore) SetSigningKeyStatus(_ context.Context, kid string, status auth.KeyState, retireAt time.Time) error {
	defer s.lock()()
	k, ok := s.d.signing[kid]
	if !ok {
		return notFound("signing key")
	}
	k.Status, k.RetireAt = status, retireAt
	s.d.signing[kid] = k
	return nil
}

// AppendAudit implements Store.
func (s *MemStore) AppendAudit(_ context.Context, a AuditRecord) error {
	defer s.lock()()
	s.d.audit = append(s.d.audit, a)
	return nil
}

// AppendOutbox implements Store.
func (s *MemStore) AppendOutbox(_ context.Context, e OutboxEvent) error {
	defer s.lock()()
	for _, o := range s.d.outbox {
		if o.EventID == e.EventID {
			return conflict("outbox event")
		}
	}
	s.d.outbox = append(s.d.outbox, e)
	return nil
}

// AuditLog returns a copy of the audit rows (tests).
func (s *MemStore) AuditLog() []AuditRecord {
	defer s.lock()()
	return slices.Clone(s.d.audit)
}

// Outbox returns a copy of the outbox rows (tests).
func (s *MemStore) Outbox() []OutboxEvent {
	defer s.lock()()
	return slices.Clone(s.d.outbox)
}

// IdemBegin implements Store.
func (s *MemStore) IdemBegin(_ context.Context, rec IdemRecord, staleAfter time.Duration) (IdemRecord, bool, error) {
	defer s.lock()()
	k := idemKey{rec.TenantID, rec.Key}
	if old, ok := s.d.idem[k]; ok {
		expired := !rec.CreatedAt.Before(old.ExpiresAt)
		stale := old.CompletedAt.IsZero() && rec.CreatedAt.Sub(old.CreatedAt) >= staleAfter
		if !expired && !stale {
			return old, false, nil
		}
	}
	s.d.idem[k] = rec
	return rec, true, nil
}

// IdemComplete implements Store.
func (s *MemStore) IdemComplete(_ context.Context, tenant core.UUID, key string, status int, body []byte, contentType string, at time.Time) error {
	defer s.lock()()
	k := idemKey{tenant, key}
	r, ok := s.d.idem[k]
	if !ok {
		return notFound("idempotency key")
	}
	r.Status, r.Body, r.ContentType, r.CompletedAt = status, bytes.Clone(body), contentType, at
	s.d.idem[k] = r
	return nil
}

// IdemAbandon implements Store.
func (s *MemStore) IdemAbandon(_ context.Context, tenant core.UUID, key string) error {
	defer s.lock()()
	delete(s.d.idem, idemKey{tenant, key})
	return nil
}

// IdemPurge implements Store.
func (s *MemStore) IdemPurge(_ context.Context, now time.Time) (int, error) {
	defer s.lock()()
	n := 0
	for k, r := range s.d.idem {
		if !now.Before(r.ExpiresAt) {
			delete(s.d.idem, k)
			n++
		}
	}
	return n, nil
}
