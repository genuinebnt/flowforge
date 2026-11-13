package identity

import (
	"context"
	"crypto/ed25519"
	"errors"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
)

// Store sentinels. Repositories return them (possibly wrapped); the
// service turns them into client-facing *core.Error values.
var (
	// ErrNotFound means the row does not exist.
	ErrNotFound = errors.New("identity: not found")
	// ErrConflict means a uniqueness constraint was violated.
	ErrConflict = errors.New("identity: conflict")
)

// Tenant is a row of identity.tenants.
type Tenant struct {
	ID         core.UUID
	Name       string
	Slug       string
	Plan       string
	CreatedAt  time.Time
	DisabledAt time.Time
}

// User is a row of identity.users.
type User struct {
	ID              core.UUID
	Email           string
	EmailVerifiedAt time.Time
	PasswordHash    string
	Status          auth.UserStatus
	CreatedAt       time.Time
	LastLoginAt     time.Time
	SessionVersion  int64
}

// Membership is a row of identity.memberships.
type Membership struct {
	TenantID  core.UUID
	UserID    core.UUID
	Role      auth.Role
	CreatedAt time.Time
}

// Member is a membership joined with its user.
type Member struct {
	Membership
	Email  string
	Status auth.UserStatus
}

// Invite is a row of identity.invites.
type Invite struct {
	ID         core.UUID
	TenantID   core.UUID
	Email      string
	Role       auth.Role
	TokenHash  [32]byte
	ExpiresAt  time.Time
	AcceptedAt time.Time
	CreatedBy  core.UUID
	CreatedAt  time.Time
}

// APIKeyRecord is a row of identity.api_keys. The secret itself is never
// stored, only its SHA-256.
type APIKeyRecord struct {
	ID         core.UUID
	TenantID   core.UUID
	Name       string
	Prefix     string // ffk_<key id>
	SecretHash [32]byte
	Role       auth.Role
	CreatedBy  core.UUID
	CreatedAt  time.Time
	LastUsedAt time.Time
	ExpiresAt  time.Time
	RevokedAt  time.Time
}

// KeyID returns the key id embedded in the token (ffk_<key id>_<secret>).
func (k APIKeyRecord) KeyID() string { return APIKeyID(k.ID) }

// PublicID returns the key_... identifier used in REST paths.
func (k APIKeyRecord) PublicID() string { return core.PublicID(core.PrefixAPIKey, k.ID) }

// APIKeyID is the token key id for a key row id: the 26-character
// Crockford body of its public id.
func APIKeyID(id core.UUID) string {
	return core.PublicID(core.PrefixAPIKey, id)[len(core.PrefixAPIKey)+1:]
}

// SigningKeyRecord is a row of identity.signing_keys: the public key and a
// reference to the private key in the KeyStore, never the private key.
type SigningKeyRecord struct {
	KID           string
	Alg           string
	PublicKey     ed25519.PublicKey
	PrivateKeyRef string
	Status        auth.KeyState
	CreatedAt     time.Time
	RetireAt      time.Time
}

// AuditRecord is a row of identity.auth_audit.
type AuditRecord struct {
	At        time.Time
	TenantID  core.UUID // zero when unknown (failed logins)
	UserID    core.UUID
	Event     string
	IP        string
	UserAgent string
	Detail    map[string]string
}

// OutboxEvent is a row of identity.outbox (topic auth.events).
type OutboxEvent struct {
	EventID     core.UUID
	Topic       string
	OrderingKey string
	Payload     []byte // JSON
	CreatedAt   time.Time
}

// IdemRecord is a row of identity.http_idempotency.
type IdemRecord struct {
	TenantID    core.UUID
	Key         string
	Method      string
	Path        string
	RequestHash [32]byte
	Status      int
	Body        []byte
	ContentType string
	CreatedAt   time.Time
	CompletedAt time.Time // zero while the request is in flight
	ExpiresAt   time.Time
}

// RoleGroup is a tenant role group (identity.role_groups and
// role_group_members, migration 0015): a free-form approval role its
// members hold in addition to their membership role (spec 9.7).
type RoleGroup struct {
	Name    string
	Members []core.UUID
}

// Store is the identity repository port. Implementations: MemStore (this
// package) and pgstore.Store (identity schema in Postgres). Every method
// returns ErrNotFound or ErrConflict (wrapped) for missing rows and
// uniqueness violations.
type Store interface {
	// WithTx runs fn with a Store whose operations commit or roll back
	// together.
	WithTx(ctx context.Context, fn func(Store) error) error
	// Ping checks connectivity (readiness).
	Ping(ctx context.Context) error

	CreateTenant(ctx context.Context, t Tenant) error
	GetTenant(ctx context.Context, id core.UUID) (Tenant, error)

	CreateUser(ctx context.Context, u User) error
	GetUser(ctx context.Context, id core.UUID) (User, error)
	GetUserByEmail(ctx context.Context, email string) (User, error)
	SetPasswordHash(ctx context.Context, id core.UUID, hash string) error
	// ActivateUser moves a pending user to active (a disabled user stays
	// disabled) and sets email_verified_at to at unless it is already set.
	ActivateUser(ctx context.Context, id core.UUID, at time.Time) error
	SetLastLogin(ctx context.Context, id core.UUID, at time.Time) error
	// BumpSessionVersion increments users.session_version and returns it.
	BumpSessionVersion(ctx context.Context, id core.UUID) (int64, error)

	CreateMembership(ctx context.Context, m Membership) error
	GetMembership(ctx context.Context, tenant, user core.UUID) (Membership, error)
	// UserMemberships lists a user's memberships, oldest first.
	UserMemberships(ctx context.Context, user core.UUID) ([]Membership, error)
	// ListMembers returns up to limit members with user id > after.
	ListMembers(ctx context.Context, tenant, after core.UUID, limit int) ([]Member, error)
	SetMemberRole(ctx context.Context, tenant, user core.UUID, role auth.Role) error
	CountOwners(ctx context.Context, tenant core.UUID) (int, error)

	// AddRoleGroupMember adds user (a member of tenant, else ErrNotFound)
	// to role group name, creating the group; adding an existing member is
	// a no-op (G-LIM-05).
	AddRoleGroupMember(ctx context.Context, tenant core.UUID, name string, user core.UUID, at time.Time) error
	// RemoveRoleGroupMember removes user from name, deleting the group with
	// its last member; it reports whether a row was removed.
	RemoveRoleGroupMember(ctx context.Context, tenant core.UUID, name string, user core.UUID) (bool, error)
	// ListRoleGroups lists the tenant's groups by name with members sorted
	// by user id.
	ListRoleGroups(ctx context.Context, tenant core.UUID) ([]RoleGroup, error)
	// UserRoleGroups lists the names of the groups user belongs to in
	// tenant, sorted.
	UserRoleGroups(ctx context.Context, tenant, user core.UUID) ([]string, error)

	CreateInvite(ctx context.Context, inv Invite) error
	GetInviteByHash(ctx context.Context, hash [32]byte) (Invite, error)
	// AcceptInvite sets accepted_at if it is unset; false if already set.
	AcceptInvite(ctx context.Context, id core.UUID, at time.Time) (bool, error)

	InsertVerification(ctx context.Context, v auth.Verification) error
	// ConsumeVerification marks an unused, unexpired row used and returns
	// it; otherwise auth.ErrVerificationInvalid.
	ConsumeVerification(ctx context.Context, hash [32]byte, now time.Time) (auth.Verification, error)

	InsertRefreshToken(ctx context.Context, t auth.RefreshToken) error
	FindRefreshToken(ctx context.Context, hash [32]byte) (auth.RefreshToken, error)
	MarkRefreshUsed(ctx context.Context, id core.UUID, at time.Time) (bool, error)
	RevokeFamily(ctx context.Context, family core.UUID, at time.Time) error
	RevokeUserSessions(ctx context.Context, user core.UUID, at time.Time) error

	CreateAPIKey(ctx context.Context, k APIKeyRecord) error
	GetAPIKeyByPrefix(ctx context.Context, prefix string) (APIKeyRecord, error)
	// GetAPIKey returns a key of tenant by row id.
	GetAPIKey(ctx context.Context, tenant, id core.UUID) (APIKeyRecord, error)
	// ListAPIKeys returns up to limit keys of tenant with id > after.
	ListAPIKeys(ctx context.Context, tenant, after core.UUID, limit int) ([]APIKeyRecord, error)
	// RevokeAPIKey sets revoked_at; false if the key is unknown in tenant or
	// already revoked.
	RevokeAPIKey(ctx context.Context, tenant, id core.UUID, at time.Time) (bool, error)
	TouchAPIKey(ctx context.Context, id core.UUID, at time.Time) error

	PutSigningKey(ctx context.Context, k SigningKeyRecord) error
	ListSigningKeys(ctx context.Context) ([]SigningKeyRecord, error)
	SetSigningKeyStatus(ctx context.Context, kid string, status auth.KeyState, retireAt time.Time) error

	AppendAudit(ctx context.Context, a AuditRecord) error
	AppendOutbox(ctx context.Context, e OutboxEvent) error

	// IdemBegin inserts an in-flight row for (tenant, key) unless one
	// exists. It returns the existing row and false, or rec and true. A
	// row past its expires_at, and an in-flight row older than staleAfter
	// (its request died), is replaced.
	IdemBegin(ctx context.Context, rec IdemRecord, staleAfter time.Duration) (IdemRecord, bool, error)
	// IdemComplete stores the response of the in-flight row; ErrNotFound
	// when the row is gone.
	IdemComplete(ctx context.Context, tenant core.UUID, key string, status int, body []byte, contentType string, at time.Time) error
	// IdemAbandon deletes the row if it is still in flight, so the key can
	// be retried (the gateway abandons after a 5xx); a completed row, for
	// example one a later request took over and finished, is kept.
	IdemAbandon(ctx context.Context, tenant core.UUID, key string) error
	// IdemPurge deletes rows past expires_at.
	IdemPurge(ctx context.Context, now time.Time) (int, error)
}
