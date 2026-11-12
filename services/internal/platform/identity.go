package platform

import (
	"context"
	"strings"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
)

// Metadata keys of spec 11.1 (gRPC metadata keys are lower case).
const (
	MDAuthorization  = "authorization"
	MDTenant         = "x-tenant-id"
	MDActor          = "x-actor"
	MDRequestID      = "x-request-id"
	MDTraceparent    = "traceparent"
	MDIdempotencyKey = "idempotency-key"
)

// Identity is the verified caller of an internal RPC: the tenant, the actor
// ("user:<id>", "apikey:<id>", "service:<name>") and the membership role,
// all taken from the internal token (spec 3.2, 11.1).
type Identity struct {
	Tenant core.UUID
	Actor  string
	Role   auth.Role
}

// ServiceActor returns the actor of a service account ("service:<name>").
func ServiceActor(name string) string { return "service:" + name }

// IsService reports whether the actor is a service account.
func (i Identity) IsService() bool { return strings.HasPrefix(i.Actor, "service:") }

// AtLeast reports whether the identity's role is r or a more privileged
// built-in role (viewer < analyst < approver < admin < owner).
func (i Identity) AtLeast(r auth.Role) bool { return roleRank(i.Role) >= roleRank(r) }

func roleRank(r auth.Role) int {
	for n, x := range auth.Roles() {
		if x == r {
			return n + 1
		}
	}
	return 0
}

// EngineCaller converts the identity to the engine's Caller.
func (i Identity) EngineCaller() engine.Caller {
	var roles []string
	if i.Role != "" {
		roles = []string{string(i.Role)}
	}
	return engine.Caller{Tenant: i.Tenant, Actor: i.Actor, Roles: roles}
}

// TenantPublicID returns the tenant as a public id (ten_...).
func (i Identity) TenantPublicID() string { return core.PublicID(core.PrefixTenant, i.Tenant) }

type identityKey struct{}

// WithIdentity returns ctx carrying id. Server interceptors set it from the
// token; background workers set a service identity before calling other
// services, and client interceptors mint the outgoing token for it.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// IdentityFrom returns the identity in ctx.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// MustIdentity returns the identity in ctx or an UNAUTHENTICATED error.
func MustIdentity(ctx context.Context) (Identity, error) {
	id, ok := IdentityFrom(ctx)
	if !ok {
		return Identity{}, core.Errorf(core.KindUnauthenticated, core.ReasonTokenMissing, "no verified identity")
	}
	return id, nil
}

// AsService returns ctx carrying a service identity for tenant, unless ctx
// already carries an identity of that tenant (a user's request being served),
// which is kept so the downstream service sees the original actor.
func AsService(ctx context.Context, tenant core.UUID, service string) context.Context {
	if id, ok := IdentityFrom(ctx); ok && id.Tenant == tenant {
		return ctx
	}
	return WithIdentity(ctx, Identity{Tenant: tenant, Actor: ServiceActor(service), Role: auth.RoleAnalyst})
}

type idemKey struct{}

// WithIdempotencyKey returns ctx carrying the idempotency key the client
// interceptor sends as idempotency-key metadata.
func WithIdempotencyKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, idemKey{}, key)
}

// IdempotencyKeyFrom returns the idempotency key in ctx (set from incoming
// metadata by the server interceptor).
func IdempotencyKeyFrom(ctx context.Context) string {
	k, _ := ctx.Value(idemKey{}).(string)
	return k
}

type requestIDKey struct{}

// WithRequestID returns ctx carrying a request id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFrom returns the request id in ctx.
func RequestIDFrom(ctx context.Context) string {
	s, _ := ctx.Value(requestIDKey{}).(string)
	return s
}

// NewRequestID returns a fresh public request id (req_...).
func NewRequestID() string {
	return core.PublicID(core.PrefixRequest, core.NewUUIDv7(systemClock, core.SystemRng{}))
}

var systemClock = &core.SystemClock{}
