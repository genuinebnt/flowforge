package auth

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"unicode/utf8"
)

// Password length limits from spec 13.1, in Unicode code points.
const (
	MinPasswordLength = 12
	MaxPasswordLength = 128
)

// BreachedChecker reports whether a password appears in a breached-password
// corpus (a k-anonymity API or a local bloom filter, spec 13.1).
type BreachedChecker interface {
	IsBreached(ctx context.Context, password string) (bool, error)
}

// PasswordPolicy is the spec 13.1 policy: 12 to 128 characters, not
// breached, and no composition rules.
type PasswordPolicy struct {
	Breached BreachedChecker
}

// Check returns nil when password is acceptable, a policy error matching
// ErrPasswordPolicy when it is not, or a wrapped checker error. A missing
// or failing breached checker rejects the password (fail closed).
func (p PasswordPolicy) Check(ctx context.Context, password string) error {
	if !utf8.ValidString(password) {
		return ErrPasswordEncoding
	}
	switch n := utf8.RuneCountInString(password); {
	case n < MinPasswordLength:
		return ErrPasswordTooShort
	case n > MaxPasswordLength:
		return ErrPasswordTooLong
	}
	if p.Breached == nil {
		return fmt.Errorf("%w: password policy has no breached checker", ErrConfig)
	}
	breached, err := p.Breached.IsBreached(ctx, password)
	if err != nil {
		return fmt.Errorf("auth: breached-password check: %w", err)
	}
	if breached {
		return ErrPasswordBreached
	}
	return nil
}

// BreachedSet is an in-memory BreachedChecker holding SHA-256 digests of
// the listed passwords. It is safe for concurrent use.
type BreachedSet struct {
	mu  sync.RWMutex
	set map[[32]byte]struct{}
}

var _ BreachedChecker = (*BreachedSet)(nil)

// NewBreachedSet returns a set containing passwords.
func NewBreachedSet(passwords ...string) *BreachedSet {
	s := &BreachedSet{set: make(map[[32]byte]struct{}, len(passwords))}
	s.Add(passwords...)
	return s
}

// Add inserts passwords into the set.
func (s *BreachedSet) Add(passwords ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, pw := range passwords {
		s.set[sha256.Sum256([]byte(pw))] = struct{}{}
	}
}

// IsBreached implements BreachedChecker.
func (s *BreachedSet) IsBreached(_ context.Context, password string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.set[sha256.Sum256([]byte(password))]
	return ok, nil
}
