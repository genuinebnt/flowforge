package auth

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// VerificationTTL is the lifetime of an email verification token.
const VerificationTTL = 24 * time.Hour

// Verification is an identity.email_verifications row.
type Verification struct {
	TokenHash [32]byte
	UserID    core.UUID
	ExpiresAt time.Time
	UsedAt    time.Time // zero while unused
}

// VerificationStore persists email verifications.
type VerificationStore interface {
	Insert(ctx context.Context, v Verification) error
	// Consume atomically marks the verification with hash as used at now,
	// provided it exists, is unused and now is before its expiry, and
	// returns it. Otherwise it returns ErrVerificationInvalid.
	Consume(ctx context.Context, hash [32]byte, now time.Time) (Verification, error)
}

// EmailVerifier issues and confirms single-use email verification tokens:
// 32 random bytes (base64url), stored as SHA-256, valid 24 hours (spec
// 13.1).
type EmailVerifier struct {
	store VerificationStore
	clock core.Clock
	rand  io.Reader
}

// NewEmailVerifier returns an EmailVerifier.
func NewEmailVerifier(store VerificationStore, clock core.Clock, rand io.Reader) (*EmailVerifier, error) {
	if store == nil || clock == nil || rand == nil {
		return nil, fmt.Errorf("%w: email verifier needs a store, clock and random reader", ErrConfig)
	}
	return &EmailVerifier{store: store, clock: clock, rand: rand}, nil
}

// Issue creates a verification for user and returns the token to email.
func (e *EmailVerifier) Issue(ctx context.Context, user core.UUID) (string, error) {
	tok, err := newSecret(e.rand)
	if err != nil {
		return "", err
	}
	v := Verification{TokenHash: HashToken(tok), UserID: user, ExpiresAt: e.clock.Now().Add(VerificationTTL)}
	if err := e.store.Insert(ctx, v); err != nil {
		return "", fmt.Errorf("auth: store verification: %w", err)
	}
	return tok, nil
}

// Confirm consumes token and returns the verified user. Unknown, used and
// expired tokens all return ErrVerificationInvalid.
func (e *EmailVerifier) Confirm(ctx context.Context, token string) (core.UUID, error) {
	if !isSecret(token) {
		return core.Nil, ErrVerificationInvalid
	}
	v, err := e.store.Consume(ctx, HashToken(token), e.clock.Now())
	if err != nil {
		return core.Nil, err
	}
	return v.UserID, nil
}

// InMemoryVerificationStore is a VerificationStore fake. It is safe for
// concurrent use.
type InMemoryVerificationStore struct {
	mu   sync.Mutex
	rows map[[32]byte]Verification
}

var _ VerificationStore = (*InMemoryVerificationStore)(nil)

// NewInMemoryVerificationStore returns an empty store.
func NewInMemoryVerificationStore() *InMemoryVerificationStore {
	return &InMemoryVerificationStore{rows: map[[32]byte]Verification{}}
}

// Insert implements VerificationStore.
func (s *InMemoryVerificationStore) Insert(_ context.Context, v Verification) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.rows[v.TokenHash]; dup {
		return fmt.Errorf("auth: duplicate verification token hash")
	}
	s.rows[v.TokenHash] = v
	return nil
}

// Consume implements VerificationStore.
func (s *InMemoryVerificationStore) Consume(_ context.Context, hash [32]byte, now time.Time) (Verification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.rows[hash]
	if !ok || !v.UsedAt.IsZero() || !now.Before(v.ExpiresAt) {
		return Verification{}, ErrVerificationInvalid
	}
	v.UsedAt = now
	s.rows[hash] = v
	return v, nil
}
