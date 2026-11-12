package auth

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// InMemoryUserStore is a UserStore fake keyed by normalized email. It is
// safe for concurrent use.
type InMemoryUserStore struct {
	mu    sync.Mutex
	users map[string]User
}

var _ UserStore = (*InMemoryUserStore)(nil)

// NewInMemoryUserStore returns a store holding users.
func NewInMemoryUserStore(users ...User) *InMemoryUserStore {
	s := &InMemoryUserStore{users: map[string]User{}}
	for _, u := range users {
		s.Put(u)
	}
	return s
}

// Put inserts or replaces u.
func (s *InMemoryUserStore) Put(u User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u.Email = NormalizeEmail(u.Email)
	s.users[u.Email] = u
}

// Get returns the user with email.
func (s *InMemoryUserStore) Get(email string) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[NormalizeEmail(email)]
	return u, ok
}

// FindByEmail implements UserStore.
func (s *InMemoryUserStore) FindByEmail(_ context.Context, email string) (User, error) {
	if u, ok := s.Get(email); ok {
		return u, nil
	}
	return User{}, ErrUserNotFound
}

// SetPasswordHash implements UserStore.
func (s *InMemoryUserStore) SetPasswordHash(_ context.Context, id core.UUID, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for email, u := range s.users {
		if u.ID == id {
			u.PasswordHash = hash
			s.users[email] = u
			return nil
		}
	}
	return fmt.Errorf("auth: user %s: %w", id, ErrNotFound)
}

// CountingThrottle is a Throttle fake with fixed windows on the injected
// clock: each key admits limit.Count calls per window of limit.Per,
// starting at the key's first call. It is safe for concurrent use.
type CountingThrottle struct {
	clock core.Clock
	mu    sync.Mutex
	state map[string]*window
}

type window struct {
	start time.Time
	count int
}

var _ Throttle = (*CountingThrottle)(nil)

// NewCountingThrottle returns an empty throttle.
func NewCountingThrottle(clock core.Clock) *CountingThrottle {
	return &CountingThrottle{clock: clock, state: map[string]*window{}}
}

// Allow implements Throttle.
func (t *CountingThrottle) Allow(_ context.Context, key string, limit Limit) (bool, time.Duration, error) {
	now := t.clock.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	w, ok := t.state[key]
	if !ok || !now.Before(w.start.Add(limit.Per)) {
		w = &window{start: now}
		t.state[key] = w
	}
	if w.count >= limit.Count {
		return false, w.start.Add(limit.Per).Sub(now), nil
	}
	w.count++
	return true, 0, nil
}

// Count returns the calls admitted for key in its current window.
func (t *CountingThrottle) Count(key string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if w, ok := t.state[key]; ok {
		return w.count
	}
	return 0
}

// InMemoryLockoutStore is a LockoutStore fake. It is safe for concurrent
// use.
type InMemoryLockoutStore struct {
	mu    sync.Mutex
	state map[string]Lockout
}

var _ LockoutStore = (*InMemoryLockoutStore)(nil)

// NewInMemoryLockoutStore returns an empty store.
func NewInMemoryLockoutStore() *InMemoryLockoutStore {
	return &InMemoryLockoutStore{state: map[string]Lockout{}}
}

// Get implements LockoutStore.
func (s *InMemoryLockoutStore) Get(_ context.Context, key string) (Lockout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state[key], nil
}

// RecordFailure implements LockoutStore.
func (s *InMemoryLockoutStore) RecordFailure(_ context.Context, key string, now time.Time, threshold int, cooldown time.Duration) (Lockout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.state[key]
	l.Failures++
	if l.Failures >= threshold {
		l = Lockout{LockedUntil: now.Add(cooldown)}
	}
	s.state[key] = l
	return l, nil
}

// Reset implements LockoutStore.
func (s *InMemoryLockoutStore) Reset(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.state, key)
	return nil
}
