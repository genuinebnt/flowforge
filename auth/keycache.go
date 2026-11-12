package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// KeySource fetches the current key set, for example the issuer's
// /.well-known/jwks.json (via ParseJWKS) or the signing_keys table.
type KeySource interface {
	FetchKeys(ctx context.Context) (*KeySet, error)
}

// KeySourceFunc adapts a function to KeySource.
type KeySourceFunc func(ctx context.Context) (*KeySet, error)

// FetchKeys implements KeySource.
func (f KeySourceFunc) FetchKeys(ctx context.Context) (*KeySet, error) { return f(ctx) }

// Key cache timings (spec 13.3, 13.7).
const (
	// MinKeyRefresh bounds refreshes triggered by unknown kids.
	MinKeyRefresh = time.Minute
	// KeySetMaxAge matches the JWKS Cache-Control max-age of 300 s.
	KeySetMaxAge = 5 * time.Minute
)

// KeyCacheConfig configures a KeyCache. Zero durations take the defaults
// above.
type KeyCacheConfig struct {
	Source     KeySource
	Clock      core.Clock
	MinRefresh time.Duration
	MaxAge     time.Duration
}

// KeyCache is a KeyResolver over a cached KeySet. It fetches on first use,
// refetches when the set is older than MaxAge, and on an unknown kid
// refetches at most once per MinRefresh (spec 13.3) so a flood of tokens
// with random kids cannot hammer the key source. A failed refresh keeps
// the previous set. Intervals use the monotonic clock. It is safe for
// concurrent use; fetches are serialized.
type KeyCache struct {
	cfg KeyCacheConfig

	mu          sync.Mutex
	set         *KeySet
	fetchedAt   time.Duration
	attempted   bool
	attemptedAt time.Duration
}

var _ KeyResolver = (*KeyCache)(nil)

var errRefreshThrottled = errors.New("auth: key refresh throttled")

// NewKeyCache returns an empty cache.
func NewKeyCache(cfg KeyCacheConfig) (*KeyCache, error) {
	if cfg.Source == nil || cfg.Clock == nil {
		return nil, fmt.Errorf("%w: key cache needs a source and a clock", ErrConfig)
	}
	if cfg.MinRefresh <= 0 {
		cfg.MinRefresh = MinKeyRefresh
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = KeySetMaxAge
	}
	return &KeyCache{cfg: cfg}, nil
}

// refresh fetches the set unless an attempt happened within MinRefresh.
// The caller holds c.mu.
func (c *KeyCache) refresh(ctx context.Context) error {
	now := c.cfg.Clock.Monotonic()
	if c.attempted && now-c.attemptedAt < c.cfg.MinRefresh {
		return errRefreshThrottled
	}
	c.attempted, c.attemptedAt = true, now
	set, err := c.cfg.Source.FetchKeys(ctx)
	if err == nil && set == nil {
		err = errors.New("key source returned no key set")
	}
	if err != nil {
		return fmt.Errorf("auth: fetch keys: %w", err)
	}
	c.set, c.fetchedAt = set, now
	return nil
}

// ResolveKey implements KeyResolver.
func (c *KeyCache) ResolveKey(ctx context.Context, kid string) (PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var refreshErr error
	if c.set == nil || c.cfg.Clock.Monotonic()-c.fetchedAt >= c.cfg.MaxAge {
		refreshErr = c.refresh(ctx)
	}
	var k PublicKey
	ok := false
	if c.set != nil {
		k, ok = c.set.Lookup(kid)
	}
	if !ok && refreshErr == nil {
		refreshErr = c.refresh(ctx)
		if c.set != nil {
			k, ok = c.set.Lookup(kid)
		}
	}
	switch {
	case !ok && refreshErr != nil && !errors.Is(refreshErr, errRefreshThrottled):
		return PublicKey{}, fmt.Errorf("%w (%w)", ErrUnknownKid, refreshErr)
	case !ok:
		return PublicKey{}, ErrUnknownKid
	case k.State == KeyRetired:
		return PublicKey{}, ErrKeyRetired
	}
	return k, nil
}

// MemoryKeySource is an in-memory KeySource fake that counts fetches. It is
// safe for concurrent use.
type MemoryKeySource struct {
	mu      sync.Mutex
	set     *KeySet
	err     error
	fetches int
}

var _ KeySource = (*MemoryKeySource)(nil)

// NewMemoryKeySource returns a source serving set.
func NewMemoryKeySource(set *KeySet) *MemoryKeySource { return &MemoryKeySource{set: set} }

// Put replaces the served set and clears any injected error.
func (m *MemoryKeySource) Put(set *KeySet) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.set, m.err = set, nil
}

// Fail makes subsequent fetches return err.
func (m *MemoryKeySource) Fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

// Fetches returns the number of FetchKeys calls so far.
func (m *MemoryKeySource) Fetches() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fetches
}

// FetchKeys implements KeySource.
func (m *MemoryKeySource) FetchKeys(context.Context) (*KeySet, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fetches++
	if m.err != nil {
		return nil, m.err
	}
	return m.set, nil
}
