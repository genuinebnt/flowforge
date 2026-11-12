package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// Refresh token lifetimes (spec 13.2).
const (
	RefreshSlidingTTL  = 30 * 24 * time.Hour
	RefreshAbsoluteTTL = 90 * 24 * time.Hour
)

// AuditRefreshReuse is the auth_audit event logged on refresh reuse.
const AuditRefreshReuse = "refresh_reuse"

// RefreshToken is an identity.refresh_tokens row. Zero times mean NULL.
type RefreshToken struct {
	ID        core.UUID
	FamilyID  core.UUID // one login session; the access token's sid
	UserID    core.UUID
	TenantID  core.UUID
	ParentID  core.UUID // core.Nil for the first token of a family
	TokenHash [32]byte
	CreatedAt time.Time
	// FamilyStartedAt is the creation time of the family's first token,
	// the origin of the 90-day absolute expiry. Adapters may derive it as
	// min(created_at) over the family.
	FamilyStartedAt time.Time
	ExpiresAt       time.Time
	UsedAt          time.Time
	RevokedAt       time.Time
}

// AuditEvent is an identity.auth_audit row.
type AuditEvent struct {
	At       time.Time
	TenantID core.UUID
	UserID   core.UUID
	Event    string
	Detail   map[string]string
}

// RefreshStore is the persistence port of refresh rotation (spec 13.4).
type RefreshStore interface {
	// FindByHash returns the row whose token_hash is hash, or ErrNotFound.
	FindByHash(ctx context.Context, hash [32]byte) (RefreshToken, error)
	// MarkUsed sets used_at = at if it is still NULL and reports whether
	// it did (a compare-and-set, so two concurrent rotations of one token
	// cannot both succeed).
	MarkUsed(ctx context.Context, id core.UUID, at time.Time) (bool, error)
	// Insert stores a new row.
	Insert(ctx context.Context, t RefreshToken) error
	// RevokeFamily sets revoked_at = at on every unrevoked row of family.
	RevokeFamily(ctx context.Context, family core.UUID, at time.Time) error
	// BumpSessionVersion increments users.session_version and returns the
	// new value. Adapters also refresh the sv:<user_id> revocation cache.
	BumpSessionVersion(ctx context.Context, user core.UUID) (int64, error)
	// Audit appends to auth_audit.
	Audit(ctx context.Context, e AuditEvent) error
}

// RefresherConfig configures a Refresher. Zero TTLs take the spec values.
type RefresherConfig struct {
	Store RefreshStore
	Clock core.Clock
	// Random supplies token secrets: crypto/rand.Reader in production.
	Random io.Reader
	// IDs supplies the randomness of row and family UUIDv7s.
	IDs         core.Rng
	SlidingTTL  time.Duration
	AbsoluteTTL time.Duration
}

// Refresher issues and rotates refresh tokens with reuse detection. It is
// safe for concurrent use if Store and Random are.
type Refresher struct {
	cfg RefresherConfig
	mu  sync.Mutex // guards cfg.IDs
}

// NewRefresher checks cfg and returns a Refresher.
func NewRefresher(cfg RefresherConfig) (*Refresher, error) {
	if cfg.Store == nil || cfg.Clock == nil || cfg.Random == nil || cfg.IDs == nil {
		return nil, fmt.Errorf("%w: refresher needs a store, clock, random reader and id source", ErrConfig)
	}
	if cfg.SlidingTTL <= 0 {
		cfg.SlidingTTL = RefreshSlidingTTL
	}
	if cfg.AbsoluteTTL <= 0 {
		cfg.AbsoluteTTL = RefreshAbsoluteTTL
	}
	return &Refresher{cfg: cfg}, nil
}

// IssuedRefresh is a refresh token handed to the client together with its
// stored row. The caller mints the matching access token with
// sid = Row.FamilyID.
type IssuedRefresh struct {
	Token string
	Row   RefreshToken
}

func (r *Refresher) newID() core.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return core.NewUUIDv7(r.cfg.Clock, r.cfg.IDs)
}

func (r *Refresher) insert(ctx context.Context, row RefreshToken) (IssuedRefresh, error) {
	tok, err := NewRefreshToken(r.cfg.Random)
	if err != nil {
		return IssuedRefresh{}, err
	}
	row.ID = r.newID()
	row.TokenHash = HashToken(tok)
	if err := r.cfg.Store.Insert(ctx, row); err != nil {
		return IssuedRefresh{}, fmt.Errorf("auth: insert refresh token: %w", err)
	}
	return IssuedRefresh{Token: tok, Row: row}, nil
}

// Start opens a new session family at login.
func (r *Refresher) Start(ctx context.Context, user, tenant core.UUID) (IssuedRefresh, error) {
	now := r.cfg.Clock.Now()
	return r.insert(ctx, RefreshToken{
		FamilyID: r.newID(), UserID: user, TenantID: tenant,
		CreatedAt: now, FamilyStartedAt: now,
		ExpiresAt: now.Add(min(r.cfg.SlidingTTL, r.cfg.AbsoluteTTL)),
	})
}

// Rotate implements spec 13.4: it exchanges a valid, unused refresh token
// for a new one in the same family. Presenting a token that was already
// rotated revokes the whole family, bumps the user's session version
// (invalidating outstanding access tokens), writes a refresh_reuse audit
// event and returns ErrRefreshReused. All rejections match
// ErrInvalidRefresh (HTTP 401).
func (r *Refresher) Rotate(ctx context.Context, token string) (IssuedRefresh, error) {
	if !IsRefreshTokenFormat(token) {
		return IssuedRefresh{}, ErrRefreshUnknown
	}
	row, err := r.cfg.Store.FindByHash(ctx, HashToken(token))
	if errors.Is(err, ErrNotFound) {
		return IssuedRefresh{}, ErrRefreshUnknown
	}
	if err != nil {
		return IssuedRefresh{}, fmt.Errorf("auth: find refresh token: %w", err)
	}
	now := r.cfg.Clock.Now()
	switch {
	case !row.RevokedAt.IsZero():
		return IssuedRefresh{}, ErrRefreshRevoked
	case row.ExpiresAt.Before(now):
		return IssuedRefresh{}, ErrRefreshExpired
	case !row.UsedAt.IsZero():
		return IssuedRefresh{}, r.reuse(ctx, row, now)
	}
	marked, err := r.cfg.Store.MarkUsed(ctx, row.ID, now)
	if err != nil {
		return IssuedRefresh{}, fmt.Errorf("auth: mark refresh token used: %w", err)
	}
	if !marked { // lost a race with another presentation of the same token
		return IssuedRefresh{}, r.reuse(ctx, row, now)
	}
	return r.insert(ctx, RefreshToken{
		FamilyID: row.FamilyID, UserID: row.UserID, TenantID: row.TenantID, ParentID: row.ID,
		CreatedAt: now, FamilyStartedAt: row.FamilyStartedAt,
		ExpiresAt: minTime(now.Add(r.cfg.SlidingTTL), row.FamilyStartedAt.Add(r.cfg.AbsoluteTTL)),
	})
}

// reuse handles theft suspicion. It always returns an error matching
// ErrRefreshReused, joined with any store failure.
func (r *Refresher) reuse(ctx context.Context, row RefreshToken, now time.Time) error {
	errs := []error{ErrRefreshReused}
	if err := r.cfg.Store.RevokeFamily(ctx, row.FamilyID, now); err != nil {
		errs = append(errs, fmt.Errorf("auth: revoke family: %w", err))
	}
	if _, err := r.cfg.Store.BumpSessionVersion(ctx, row.UserID); err != nil {
		errs = append(errs, fmt.Errorf("auth: bump session version: %w", err))
	}
	if err := r.cfg.Store.Audit(ctx, AuditEvent{
		At: now, TenantID: row.TenantID, UserID: row.UserID, Event: AuditRefreshReuse,
		Detail: map[string]string{"family_id": row.FamilyID.String(), "token_id": row.ID.String()},
	}); err != nil {
		errs = append(errs, fmt.Errorf("auth: audit: %w", err))
	}
	return errors.Join(errs...)
}

// RevokeSession revokes a whole family (POST /auth/logout).
func (r *Refresher) RevokeSession(ctx context.Context, family core.UUID) error {
	if err := r.cfg.Store.RevokeFamily(ctx, family, r.cfg.Clock.Now()); err != nil {
		return fmt.Errorf("auth: revoke session: %w", err)
	}
	return nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// InMemoryRefreshStore is a RefreshStore fake. It is safe for concurrent
// use.
type InMemoryRefreshStore struct {
	mu       sync.Mutex
	rows     map[core.UUID]RefreshToken
	byHash   map[[32]byte]core.UUID
	versions map[core.UUID]int64
	audit    []AuditEvent
}

var _ RefreshStore = (*InMemoryRefreshStore)(nil)

// NewInMemoryRefreshStore returns an empty store. Session versions start
// at 1, the column default.
func NewInMemoryRefreshStore() *InMemoryRefreshStore {
	return &InMemoryRefreshStore{
		rows: map[core.UUID]RefreshToken{}, byHash: map[[32]byte]core.UUID{}, versions: map[core.UUID]int64{},
	}
}

// FindByHash implements RefreshStore.
func (s *InMemoryRefreshStore) FindByHash(_ context.Context, hash [32]byte) (RefreshToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byHash[hash]
	if !ok {
		return RefreshToken{}, ErrNotFound
	}
	return s.rows[id], nil
}

// MarkUsed implements RefreshStore.
func (s *InMemoryRefreshStore) MarkUsed(_ context.Context, id core.UUID, at time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[id]
	if !ok {
		return false, ErrNotFound
	}
	if !row.UsedAt.IsZero() {
		return false, nil
	}
	row.UsedAt = at
	s.rows[id] = row
	return true, nil
}

// Insert implements RefreshStore.
func (s *InMemoryRefreshStore) Insert(_ context.Context, t RefreshToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.rows[t.ID]; dup {
		return fmt.Errorf("auth: duplicate refresh token id %s", t.ID)
	}
	if _, dup := s.byHash[t.TokenHash]; dup {
		return fmt.Errorf("auth: duplicate refresh token hash")
	}
	s.rows[t.ID] = t
	s.byHash[t.TokenHash] = t.ID
	return nil
}

// RevokeFamily implements RefreshStore.
func (s *InMemoryRefreshStore) RevokeFamily(_ context.Context, family core.UUID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, row := range s.rows {
		if row.FamilyID == family && row.RevokedAt.IsZero() {
			row.RevokedAt = at
			s.rows[id] = row
		}
	}
	return nil
}

// BumpSessionVersion implements RefreshStore.
func (s *InMemoryRefreshStore) BumpSessionVersion(_ context.Context, user core.UUID) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.sessionVersion(user) + 1
	s.versions[user] = v
	return v, nil
}

// Audit implements RefreshStore.
func (s *InMemoryRefreshStore) Audit(_ context.Context, e AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = append(s.audit, e)
	return nil
}

func (s *InMemoryRefreshStore) sessionVersion(user core.UUID) int64 {
	if v, ok := s.versions[user]; ok {
		return v
	}
	return 1
}

// SessionVersion returns the user's current session version.
func (s *InMemoryRefreshStore) SessionVersion(user core.UUID) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionVersion(user)
}

// AuditEvents returns a copy of the audit log.
func (s *InMemoryRefreshStore) AuditEvents() []AuditEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]AuditEvent(nil), s.audit...)
}

// Family returns every row of family, in no particular order.
func (s *InMemoryRefreshStore) Family(family core.UUID) []RefreshToken {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []RefreshToken
	for _, row := range s.rows {
		if row.FamilyID == family {
			out = append(out, row)
		}
	}
	return out
}
