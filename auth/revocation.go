package auth

import (
	"context"
	"sync"
)

// RevocationChecker is the revocation port of spec 13.3, backed in
// production by Redis keys deny:jti:<jti> and sv:<user_id>.
type RevocationChecker interface {
	// IsRevoked reports whether the token id is on the deny list.
	IsRevoked(ctx context.Context, jti string) (bool, error)
	// SessionVersion returns the current session version of subject. ok is
	// false when no version is cached: the sv:<user_id> entry lives as long
	// as an access token, so its absence means no bump happened within any
	// live token's lifetime.
	SessionVersion(ctx context.Context, subject string) (version int64, ok bool, err error)
}

// NoRevocation is a RevocationChecker that never revokes. Use it only for
// tokens too short-lived to revoke, such as 60-second internal tokens.
type NoRevocation struct{}

// IsRevoked implements RevocationChecker.
func (NoRevocation) IsRevoked(context.Context, string) (bool, error) { return false, nil }

// SessionVersion implements RevocationChecker.
func (NoRevocation) SessionVersion(context.Context, string) (int64, bool, error) {
	return 0, false, nil
}

// InMemoryRevocation is an in-memory RevocationChecker. It is safe for
// concurrent use.
type InMemoryRevocation struct {
	mu       sync.RWMutex
	denied   map[string]struct{}
	versions map[string]int64
}

var _ RevocationChecker = (*InMemoryRevocation)(nil)

// NewInMemoryRevocation returns an empty checker.
func NewInMemoryRevocation() *InMemoryRevocation {
	return &InMemoryRevocation{denied: map[string]struct{}{}, versions: map[string]int64{}}
}

// Deny adds jti to the deny list.
func (r *InMemoryRevocation) Deny(jti string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.denied[jti] = struct{}{}
}

// SetSessionVersion records the current session version of subject.
func (r *InMemoryRevocation) SetSessionVersion(subject string, v int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.versions[subject] = v
}

// IsRevoked implements RevocationChecker.
func (r *InMemoryRevocation) IsRevoked(_ context.Context, jti string) (bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.denied[jti]
	return ok, nil
}

// SessionVersion implements RevocationChecker.
func (r *InMemoryRevocation) SessionVersion(_ context.Context, subject string) (int64, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.versions[subject]
	return v, ok, nil
}
