// Package pgstore implements identity.Store over the identity schema in
// PostgreSQL (migrations 0002, 0008 and 0010) with pgx.
//
// The gateway's identity role must bypass row-level security (migration
// 0007): logins, API-key lookups and refresh rotation find rows before the
// tenant is known.
package pgstore

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/services/gateway/identity"
)

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store is identity.Store over Postgres.
type Store struct {
	pool *pgxpool.Pool
	q    querier
	tx   pgx.Tx
}

var _ identity.Store = (*Store)(nil)

// New returns a Store using pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool, q: pool} }

// WithTx implements identity.Store.
func (s *Store) WithTx(ctx context.Context, fn func(identity.Store) error) error {
	if s.tx != nil {
		return fn(s)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pgstore: begin: %w", err)
	}
	if err := fn(&Store{pool: s.pool, q: tx, tx: tx}); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return mapErr(fmt.Errorf("pgstore: commit: %w", err))
	}
	return nil
}

// Ping implements identity.Store.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %w", identity.ErrNotFound, err)
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "23505": // unique_violation
			return fmt.Errorf("%w: %s", identity.ErrConflict, pe.ConstraintName)
		case "23503": // foreign_key_violation
			return fmt.Errorf("%w: %s", identity.ErrNotFound, pe.ConstraintName)
		}
	}
	return err
}

func nt(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func ut(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return p.UTC()
}

func nu(u core.UUID) any {
	if u.IsZero() {
		return nil
	}
	return u
}

func un(p *core.UUID) core.UUID {
	if p == nil {
		return core.Nil
	}
	return *p
}

func h32(b []byte) [32]byte {
	var h [32]byte
	copy(h[:], b)
	return h
}

func (s *Store) exec1(ctx context.Context, what, sql string, args ...any) error {
	tag, err := s.q.Exec(ctx, sql, args...)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s: %w", what, identity.ErrNotFound)
	}
	return nil
}

// CreateTenant implements identity.Store.
func (s *Store) CreateTenant(ctx context.Context, t identity.Tenant) error {
	_, err := s.q.Exec(ctx, `INSERT INTO identity.tenants (id, name, slug, plan, created_at, disabled_at) VALUES ($1,$2,$3,$4,$5,$6)`,
		t.ID, t.Name, t.Slug, t.Plan, t.CreatedAt.UTC(), nt(t.DisabledAt))
	return mapErr(err)
}

// GetTenant implements identity.Store.
func (s *Store) GetTenant(ctx context.Context, id core.UUID) (identity.Tenant, error) {
	var t identity.Tenant
	var dis *time.Time
	err := s.q.QueryRow(ctx, `SELECT id, name, slug::text, plan, created_at, disabled_at FROM identity.tenants WHERE id = $1`, id).
		Scan(&t.ID, &t.Name, &t.Slug, &t.Plan, &t.CreatedAt, &dis)
	t.DisabledAt = ut(dis)
	return t, mapErr(err)
}

const userCols = `id, email::text, email_verified_at, password_hash, status, created_at, last_login_at, session_version`

func scanUser(row pgx.Row) (identity.User, error) {
	var u identity.User
	var ver, last *time.Time
	var status string
	err := row.Scan(&u.ID, &u.Email, &ver, &u.PasswordHash, &status, &u.CreatedAt, &last, &u.SessionVersion)
	u.EmailVerifiedAt, u.LastLoginAt, u.Status = ut(ver), ut(last), auth.UserStatus(status)
	return u, mapErr(err)
}

// CreateUser implements identity.Store.
func (s *Store) CreateUser(ctx context.Context, u identity.User) error {
	if u.SessionVersion == 0 {
		u.SessionVersion = 1
	}
	_, err := s.q.Exec(ctx, `INSERT INTO identity.users (id, email, email_verified_at, password_hash, status, created_at, last_login_at, session_version) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		u.ID, u.Email, nt(u.EmailVerifiedAt), u.PasswordHash, string(u.Status), u.CreatedAt.UTC(), nt(u.LastLoginAt), u.SessionVersion)
	return mapErr(err)
}

// GetUser implements identity.Store.
func (s *Store) GetUser(ctx context.Context, id core.UUID) (identity.User, error) {
	return scanUser(s.q.QueryRow(ctx, `SELECT `+userCols+` FROM identity.users WHERE id = $1`, id))
}

// GetUserByEmail implements identity.Store.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (identity.User, error) {
	return scanUser(s.q.QueryRow(ctx, `SELECT `+userCols+` FROM identity.users WHERE email = $1`, email))
}

// SetPasswordHash implements identity.Store.
func (s *Store) SetPasswordHash(ctx context.Context, id core.UUID, hash string) error {
	return s.exec1(ctx, "user", `UPDATE identity.users SET password_hash = $2 WHERE id = $1`, id, hash)
}

// ActivateUser implements identity.Store.
func (s *Store) ActivateUser(ctx context.Context, id core.UUID, at time.Time) error {
	return s.exec1(ctx, "user", `UPDATE identity.users SET
		status = CASE WHEN status = 'pending' THEN 'active' ELSE status END,
		email_verified_at = COALESCE(email_verified_at, $2) WHERE id = $1`, id, at.UTC())
}

// SetLastLogin implements identity.Store.
func (s *Store) SetLastLogin(ctx context.Context, id core.UUID, at time.Time) error {
	return s.exec1(ctx, "user", `UPDATE identity.users SET last_login_at = $2 WHERE id = $1`, id, at.UTC())
}

// BumpSessionVersion implements identity.Store.
func (s *Store) BumpSessionVersion(ctx context.Context, id core.UUID) (int64, error) {
	var v int64
	err := s.q.QueryRow(ctx, `UPDATE identity.users SET session_version = session_version + 1 WHERE id = $1 RETURNING session_version`, id).Scan(&v)
	return v, mapErr(err)
}

// CreateMembership implements identity.Store.
func (s *Store) CreateMembership(ctx context.Context, m identity.Membership) error {
	_, err := s.q.Exec(ctx, `INSERT INTO identity.memberships (tenant_id, user_id, role, created_at) VALUES ($1,$2,$3,$4)`,
		m.TenantID, m.UserID, string(m.Role), m.CreatedAt.UTC())
	return mapErr(err)
}

func scanMembership(row pgx.Row) (identity.Membership, error) {
	var m identity.Membership
	var role string
	err := row.Scan(&m.TenantID, &m.UserID, &role, &m.CreatedAt)
	m.Role = auth.Role(role)
	return m, mapErr(err)
}

// GetMembership implements identity.Store.
func (s *Store) GetMembership(ctx context.Context, tenant, user core.UUID) (identity.Membership, error) {
	return scanMembership(s.q.QueryRow(ctx, `SELECT tenant_id, user_id, role, created_at FROM identity.memberships WHERE tenant_id = $1 AND user_id = $2`, tenant, user))
}

// UserMemberships implements identity.Store.
func (s *Store) UserMemberships(ctx context.Context, user core.UUID) ([]identity.Membership, error) {
	rows, err := s.q.Query(ctx, `SELECT tenant_id, user_id, role, created_at FROM identity.memberships WHERE user_id = $1 ORDER BY created_at, tenant_id`, user)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []identity.Membership
	for rows.Next() {
		m, err := scanMembership(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, mapErr(rows.Err())
}

// ListMembers implements identity.Store.
func (s *Store) ListMembers(ctx context.Context, tenant, after core.UUID, limit int) ([]identity.Member, error) {
	rows, err := s.q.Query(ctx, `SELECT m.tenant_id, m.user_id, m.role, m.created_at, u.email::text, u.status
		FROM identity.memberships m JOIN identity.users u ON u.id = m.user_id
		WHERE m.tenant_id = $1 AND m.user_id > $2 ORDER BY m.user_id LIMIT $3`, tenant, after, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []identity.Member
	for rows.Next() {
		var m identity.Member
		var role, status string
		if err := rows.Scan(&m.TenantID, &m.UserID, &role, &m.CreatedAt, &m.Email, &status); err != nil {
			return nil, mapErr(err)
		}
		m.Role, m.Status = auth.Role(role), auth.UserStatus(status)
		out = append(out, m)
	}
	return out, mapErr(rows.Err())
}

// SetMemberRole implements identity.Store.
func (s *Store) SetMemberRole(ctx context.Context, tenant, user core.UUID, role auth.Role) error {
	return s.exec1(ctx, "membership", `UPDATE identity.memberships SET role = $3 WHERE tenant_id = $1 AND user_id = $2`, tenant, user, string(role))
}

// AddRoleGroupMember implements identity.Store. The group row is created
// in the same statement as the member row, so the FK of migration 0015
// never sees a member without its group.
func (s *Store) AddRoleGroupMember(ctx context.Context, tenant core.UUID, name string, user core.UUID, at time.Time) error {
	if _, err := s.GetMembership(ctx, tenant, user); err != nil {
		return err
	}
	_, err := s.q.Exec(ctx, `WITH g AS (INSERT INTO identity.role_groups (tenant_id, name, created_at) VALUES ($1, $2, $4) ON CONFLICT DO NOTHING)
INSERT INTO identity.role_group_members (tenant_id, name, user_id, created_at) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, tenant, name, user, at)
	return mapErr(err)
}

// RemoveRoleGroupMember implements identity.Store. The group is deleted in
// the same statement when user was its last member. A concurrent add of
// another user to the same group then fails its FK check (the delete
// holds the group row) instead of leaving a member without a group.
func (s *Store) RemoveRoleGroupMember(ctx context.Context, tenant core.UUID, name string, user core.UUID) (bool, error) {
	var n int
	err := s.q.QueryRow(ctx, `WITH d AS (DELETE FROM identity.role_group_members WHERE tenant_id = $1 AND name = $2 AND user_id = $3 RETURNING 1),
g AS (DELETE FROM identity.role_groups r WHERE r.tenant_id = $1 AND r.name = $2 AND EXISTS (SELECT 1 FROM d)
      AND NOT EXISTS (SELECT 1 FROM identity.role_group_members m WHERE m.tenant_id = $1 AND m.name = $2 AND m.user_id <> $3))
SELECT count(*) FROM d`, tenant, name, user).Scan(&n)
	return n > 0, mapErr(err)
}

// ListRoleGroups implements identity.Store.
func (s *Store) ListRoleGroups(ctx context.Context, tenant core.UUID) ([]identity.RoleGroup, error) {
	rows, err := s.q.Query(ctx, `SELECT name, user_id FROM identity.role_group_members WHERE tenant_id = $1 ORDER BY name, user_id::text`, tenant)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []identity.RoleGroup
	for rows.Next() {
		var name string
		var u core.UUID
		if err := rows.Scan(&name, &u); err != nil {
			return nil, mapErr(err)
		}
		if len(out) == 0 || out[len(out)-1].Name != name {
			out = append(out, identity.RoleGroup{Name: name})
		}
		out[len(out)-1].Members = append(out[len(out)-1].Members, u)
	}
	return out, mapErr(rows.Err())
}

// UserRoleGroups implements identity.Store.
func (s *Store) UserRoleGroups(ctx context.Context, tenant, user core.UUID) ([]string, error) {
	rows, err := s.q.Query(ctx, `SELECT name FROM identity.role_group_members WHERE tenant_id = $1 AND user_id = $2 ORDER BY name`, tenant, user)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, n)
	}
	return out, mapErr(rows.Err())
}

// CountOwners implements identity.Store. Inside a transaction the owner
// rows are locked so two concurrent demotions cannot both pass.
func (s *Store) CountOwners(ctx context.Context, tenant core.UUID) (int, error) {
	q := `SELECT count(*) FROM identity.memberships WHERE tenant_id = $1 AND role = 'owner'`
	if s.tx != nil {
		q = `SELECT count(*) FROM (SELECT 1 FROM identity.memberships WHERE tenant_id = $1 AND role = 'owner' FOR UPDATE) o`
	}
	var n int
	err := s.q.QueryRow(ctx, q, tenant).Scan(&n)
	return n, mapErr(err)
}

// CreateInvite implements identity.Store.
func (s *Store) CreateInvite(ctx context.Context, inv identity.Invite) error {
	_, err := s.q.Exec(ctx, `INSERT INTO identity.invites (id, tenant_id, email, role, token_hash, expires_at, accepted_at, created_by, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		inv.ID, inv.TenantID, inv.Email, string(inv.Role), inv.TokenHash[:], inv.ExpiresAt.UTC(), nt(inv.AcceptedAt), inv.CreatedBy, inv.CreatedAt.UTC())
	return mapErr(err)
}

// GetInviteByHash implements identity.Store.
func (s *Store) GetInviteByHash(ctx context.Context, hash [32]byte) (identity.Invite, error) {
	var inv identity.Invite
	var role string
	var th []byte
	var acc *time.Time
	err := s.q.QueryRow(ctx, `SELECT id, tenant_id, email::text, role, token_hash, expires_at, accepted_at, created_by, created_at
		FROM identity.invites WHERE token_hash = $1`, hash[:]).
		Scan(&inv.ID, &inv.TenantID, &inv.Email, &role, &th, &inv.ExpiresAt, &acc, &inv.CreatedBy, &inv.CreatedAt)
	inv.Role, inv.TokenHash, inv.AcceptedAt = auth.Role(role), h32(th), ut(acc)
	return inv, mapErr(err)
}

// AcceptInvite implements identity.Store.
func (s *Store) AcceptInvite(ctx context.Context, id core.UUID, at time.Time) (bool, error) {
	tag, err := s.q.Exec(ctx, `UPDATE identity.invites SET accepted_at = $2 WHERE id = $1 AND accepted_at IS NULL`, id, at.UTC())
	if err != nil {
		return false, mapErr(err)
	}
	return tag.RowsAffected() == 1, nil
}

// InsertVerification implements identity.Store.
func (s *Store) InsertVerification(ctx context.Context, v auth.Verification) error {
	_, err := s.q.Exec(ctx, `INSERT INTO identity.email_verifications (token_hash, user_id, expires_at, used_at) VALUES ($1,$2,$3,$4)`,
		v.TokenHash[:], v.UserID, v.ExpiresAt.UTC(), nt(v.UsedAt))
	return mapErr(err)
}

// ConsumeVerification implements identity.Store.
func (s *Store) ConsumeVerification(ctx context.Context, hash [32]byte, now time.Time) (auth.Verification, error) {
	var v auth.Verification
	var th []byte
	var used *time.Time
	err := s.q.QueryRow(ctx, `UPDATE identity.email_verifications SET used_at = $2
		WHERE token_hash = $1 AND used_at IS NULL AND expires_at > $2
		RETURNING token_hash, user_id, expires_at, used_at`, hash[:], now.UTC()).Scan(&th, &v.UserID, &v.ExpiresAt, &used)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.Verification{}, auth.ErrVerificationInvalid
	}
	v.TokenHash, v.UsedAt = h32(th), ut(used)
	return v, mapErr(err)
}

// InsertRefreshToken implements identity.Store. The family start is kept
// in the first token's created_at; later tokens look it up.
func (s *Store) InsertRefreshToken(ctx context.Context, t auth.RefreshToken) error {
	_, err := s.q.Exec(ctx, `INSERT INTO identity.refresh_tokens (id, family_id, user_id, tenant_id, token_hash, parent_id, created_at, expires_at, used_at, revoked_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		t.ID, t.FamilyID, t.UserID, t.TenantID, t.TokenHash[:], nu(t.ParentID), t.CreatedAt.UTC(), t.ExpiresAt.UTC(), nt(t.UsedAt), nt(t.RevokedAt))
	return mapErr(err)
}

// FindRefreshToken implements identity.Store.
func (s *Store) FindRefreshToken(ctx context.Context, hash [32]byte) (auth.RefreshToken, error) {
	var t auth.RefreshToken
	var th []byte
	var parent *core.UUID
	var used, rev *time.Time
	err := s.q.QueryRow(ctx, `SELECT r.id, r.family_id, r.user_id, r.tenant_id, r.token_hash, r.parent_id, r.created_at, r.expires_at, r.used_at, r.revoked_at,
			(SELECT min(f.created_at) FROM identity.refresh_tokens f WHERE f.family_id = r.family_id)
		FROM identity.refresh_tokens r WHERE r.token_hash = $1`, hash[:]).
		Scan(&t.ID, &t.FamilyID, &t.UserID, &t.TenantID, &th, &parent, &t.CreatedAt, &t.ExpiresAt, &used, &rev, &t.FamilyStartedAt)
	t.TokenHash, t.ParentID, t.UsedAt, t.RevokedAt = h32(th), un(parent), ut(used), ut(rev)
	return t, mapErr(err)
}

// MarkRefreshUsed implements identity.Store.
func (s *Store) MarkRefreshUsed(ctx context.Context, id core.UUID, at time.Time) (bool, error) {
	tag, err := s.q.Exec(ctx, `UPDATE identity.refresh_tokens SET used_at = $2 WHERE id = $1 AND used_at IS NULL`, id, at.UTC())
	if err != nil {
		return false, mapErr(err)
	}
	return tag.RowsAffected() == 1, nil
}

// RevokeFamily implements identity.Store.
func (s *Store) RevokeFamily(ctx context.Context, family core.UUID, at time.Time) error {
	_, err := s.q.Exec(ctx, `UPDATE identity.refresh_tokens SET revoked_at = $2 WHERE family_id = $1 AND revoked_at IS NULL`, family, at.UTC())
	return mapErr(err)
}

// RevokeUserSessions implements identity.Store.
func (s *Store) RevokeUserSessions(ctx context.Context, user core.UUID, at time.Time) error {
	_, err := s.q.Exec(ctx, `UPDATE identity.refresh_tokens SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL`, user, at.UTC())
	return mapErr(err)
}

const keyCols = `id, tenant_id, name, prefix, secret_hash, role, created_by, created_at, last_used_at, expires_at, revoked_at`

func scanKey(row pgx.Row) (identity.APIKeyRecord, error) {
	var k identity.APIKeyRecord
	var sh []byte
	var role string
	var last, exp, rev *time.Time
	err := row.Scan(&k.ID, &k.TenantID, &k.Name, &k.Prefix, &sh, &role, &k.CreatedBy, &k.CreatedAt, &last, &exp, &rev)
	k.SecretHash, k.Role, k.LastUsedAt, k.ExpiresAt, k.RevokedAt = h32(sh), auth.Role(role), ut(last), ut(exp), ut(rev)
	return k, mapErr(err)
}

// CreateAPIKey implements identity.Store.
func (s *Store) CreateAPIKey(ctx context.Context, k identity.APIKeyRecord) error {
	_, err := s.q.Exec(ctx, `INSERT INTO identity.api_keys (`+keyCols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		k.ID, k.TenantID, k.Name, k.Prefix, k.SecretHash[:], string(k.Role), k.CreatedBy, k.CreatedAt.UTC(), nt(k.LastUsedAt), nt(k.ExpiresAt), nt(k.RevokedAt))
	return mapErr(err)
}

// GetAPIKeyByPrefix implements identity.Store.
func (s *Store) GetAPIKeyByPrefix(ctx context.Context, prefix string) (identity.APIKeyRecord, error) {
	return scanKey(s.q.QueryRow(ctx, `SELECT `+keyCols+` FROM identity.api_keys WHERE prefix = $1`, prefix))
}

// GetAPIKey implements identity.Store.
func (s *Store) GetAPIKey(ctx context.Context, tenant, id core.UUID) (identity.APIKeyRecord, error) {
	return scanKey(s.q.QueryRow(ctx, `SELECT `+keyCols+` FROM identity.api_keys WHERE tenant_id = $1 AND id = $2`, tenant, id))
}

// ListAPIKeys implements identity.Store.
func (s *Store) ListAPIKeys(ctx context.Context, tenant, after core.UUID, limit int) ([]identity.APIKeyRecord, error) {
	rows, err := s.q.Query(ctx, `SELECT `+keyCols+` FROM identity.api_keys WHERE tenant_id = $1 AND id > $2 ORDER BY id LIMIT $3`, tenant, after, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []identity.APIKeyRecord
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, mapErr(rows.Err())
}

// RevokeAPIKey implements identity.Store.
func (s *Store) RevokeAPIKey(ctx context.Context, tenant, id core.UUID, at time.Time) (bool, error) {
	tag, err := s.q.Exec(ctx, `UPDATE identity.api_keys SET revoked_at = $3 WHERE tenant_id = $1 AND id = $2 AND revoked_at IS NULL`, tenant, id, at.UTC())
	if err != nil {
		return false, mapErr(err)
	}
	return tag.RowsAffected() == 1, nil
}

// TouchAPIKey implements identity.Store.
func (s *Store) TouchAPIKey(ctx context.Context, id core.UUID, at time.Time) error {
	return s.exec1(ctx, "api key", `UPDATE identity.api_keys SET last_used_at = $2 WHERE id = $1`, id, at.UTC())
}

type publicJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	X   string `json:"x"`
}

// PutSigningKey implements identity.Store. Only the public JWK and the
// private-key reference are stored.
func (s *Store) PutSigningKey(ctx context.Context, k identity.SigningKeyRecord) error {
	jwk, err := json.Marshal(publicJWK{Kty: "OKP", Crv: "Ed25519", Kid: k.KID, Use: "sig", Alg: k.Alg, X: base64.RawURLEncoding.EncodeToString(k.PublicKey)})
	if err != nil {
		return err
	}
	_, err = s.q.Exec(ctx, `INSERT INTO identity.signing_keys (kid, alg, public_jwk, private_key_ref, status, created_at, retire_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		k.KID, k.Alg, jwk, k.PrivateKeyRef, string(k.Status), k.CreatedAt.UTC(), nt(k.RetireAt))
	return mapErr(err)
}

// ListSigningKeys implements identity.Store.
func (s *Store) ListSigningKeys(ctx context.Context) ([]identity.SigningKeyRecord, error) {
	rows, err := s.q.Query(ctx, `SELECT kid, alg, public_jwk, private_key_ref, status, created_at, retire_at FROM identity.signing_keys ORDER BY created_at, kid`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []identity.SigningKeyRecord
	for rows.Next() {
		var k identity.SigningKeyRecord
		var raw []byte
		var status string
		var ret *time.Time
		if err := rows.Scan(&k.KID, &k.Alg, &raw, &k.PrivateKeyRef, &status, &k.CreatedAt, &ret); err != nil {
			return nil, mapErr(err)
		}
		var j publicJWK
		if err := json.Unmarshal(raw, &j); err != nil {
			return nil, fmt.Errorf("pgstore: signing key %s jwk: %w", k.KID, err)
		}
		x, err := base64.RawURLEncoding.DecodeString(j.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("pgstore: signing key %s: bad public key", k.KID)
		}
		k.PublicKey, k.Status, k.RetireAt = x, auth.KeyState(status), ut(ret)
		out = append(out, k)
	}
	return out, mapErr(rows.Err())
}

// SetSigningKeyStatus implements identity.Store.
func (s *Store) SetSigningKeyStatus(ctx context.Context, kid string, status auth.KeyState, retireAt time.Time) error {
	return s.exec1(ctx, "signing key", `UPDATE identity.signing_keys SET status = $2, retire_at = $3 WHERE kid = $1`, kid, string(status), nt(retireAt))
}

// AppendAudit implements identity.Store.
func (s *Store) AppendAudit(ctx context.Context, a identity.AuditRecord) error {
	detail, err := json.Marshal(nonNil(a.Detail))
	if err != nil {
		return err
	}
	var ip any
	if addr, err := netip.ParseAddr(a.IP); err == nil {
		ip = addr.String()
	}
	var ua any
	if a.UserAgent != "" {
		ua = a.UserAgent
	}
	_, err = s.q.Exec(ctx, `INSERT INTO identity.auth_audit (at, tenant_id, user_id, event, ip, user_agent, detail)
		VALUES ($1,$2,$3,$4,$5::inet,$6,$7)`,
		a.At.UTC(), nu(a.TenantID), nu(a.UserID), a.Event, ip, ua, detail)
	return mapErr(err)
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// AppendOutbox implements identity.Store.
func (s *Store) AppendOutbox(ctx context.Context, e identity.OutboxEvent) error {
	var ok any
	if e.OrderingKey != "" {
		ok = e.OrderingKey
	}
	_, err := s.q.Exec(ctx, `INSERT INTO identity.outbox (event_id, topic, ordering_key, payload, created_at) VALUES ($1,$2,$3,$4,$5)`,
		e.EventID, e.Topic, ok, e.Payload, e.CreatedAt.UTC())
	return mapErr(err)
}

const idemCols = `tenant_id, key, method, path, request_hash, status_code, response_body, content_type, created_at, completed_at, expires_at`

func scanIdem(row pgx.Row) (identity.IdemRecord, error) {
	var r identity.IdemRecord
	var rh []byte
	var status *int32
	var ct *string
	var done *time.Time
	err := row.Scan(&r.TenantID, &r.Key, &r.Method, &r.Path, &rh, &status, &r.Body, &ct, &r.CreatedAt, &done, &r.ExpiresAt)
	r.RequestHash, r.CompletedAt = h32(rh), ut(done)
	if status != nil {
		r.Status = int(*status)
	}
	if ct != nil {
		r.ContentType = *ct
	}
	return r, mapErr(err)
}

// IdemBegin implements identity.Store.
func (s *Store) IdemBegin(ctx context.Context, rec identity.IdemRecord, staleAfter time.Duration) (identity.IdemRecord, bool, error) {
	// Insert, or take over an expired or stale in-flight row; otherwise
	// return the existing row.
	row := s.q.QueryRow(ctx, `INSERT INTO identity.http_idempotency (tenant_id, key, method, path, request_hash, created_at, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (tenant_id, key) DO UPDATE SET method = EXCLUDED.method, path = EXCLUDED.path, request_hash = EXCLUDED.request_hash,
			status_code = NULL, response_body = NULL, content_type = NULL, created_at = EXCLUDED.created_at, completed_at = NULL, expires_at = EXCLUDED.expires_at
		WHERE identity.http_idempotency.expires_at <= EXCLUDED.created_at
		   OR (identity.http_idempotency.completed_at IS NULL AND identity.http_idempotency.created_at <= EXCLUDED.created_at - $8::interval)
		RETURNING `+idemCols,
		rec.TenantID, rec.Key, rec.Method, rec.Path, rec.RequestHash[:], rec.CreatedAt.UTC(), rec.ExpiresAt.UTC(), fmt.Sprintf("%d milliseconds", staleAfter.Milliseconds()))
	got, err := scanIdem(row)
	if err == nil {
		return got, true, nil
	}
	if !errors.Is(err, identity.ErrNotFound) {
		return identity.IdemRecord{}, false, err
	}
	got, err = scanIdem(s.q.QueryRow(ctx, `SELECT `+idemCols+` FROM identity.http_idempotency WHERE tenant_id = $1 AND key = $2`, rec.TenantID, rec.Key))
	if err != nil {
		return identity.IdemRecord{}, false, err
	}
	return got, false, nil
}

// IdemComplete implements identity.Store.
func (s *Store) IdemComplete(ctx context.Context, tenant core.UUID, key string, status int, body []byte, contentType string, at time.Time) error {
	return s.exec1(ctx, "idempotency key", `UPDATE identity.http_idempotency SET status_code = $3, response_body = $4, content_type = $5, completed_at = $6
		WHERE tenant_id = $1 AND key = $2`, tenant, key, status, body, contentType, at.UTC())
}

// IdemAbandon implements identity.Store.
func (s *Store) IdemAbandon(ctx context.Context, tenant core.UUID, key string) error {
	_, err := s.q.Exec(ctx, `DELETE FROM identity.http_idempotency WHERE tenant_id = $1 AND key = $2 AND completed_at IS NULL`, tenant, key)
	return mapErr(err)
}

// IdemPurge implements identity.Store.
func (s *Store) IdemPurge(ctx context.Context, now time.Time) (int, error) {
	tag, err := s.q.Exec(ctx, `DELETE FROM identity.http_idempotency WHERE expires_at <= $1`, now.UTC())
	if err != nil {
		return 0, mapErr(err)
	}
	return int(tag.RowsAffected()), nil
}
