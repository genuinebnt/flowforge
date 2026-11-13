// Package identitytest builds an in-memory identity service for tests and
// provides a shared Store contract suite run against every Store
// implementation. It is imported only from tests.
package identitytest

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/services/gateway/identity"
)

// Issuer is the token issuer used by fixtures.
const Issuer = "https://api.flowforge.test"

// Fixture is a wired identity service over in-memory ports.
type Fixture struct {
	Service     *identity.Service
	Store       identity.Store
	Mem         *identity.MemStore // nil when Store is not a MemStore
	Keys        *identity.KeyManager
	Revocations *identity.MemRevocations
	Email       *identity.MemEmailSender
	Limiter     *identity.RateLimiter
	Hasher      *CountingHasher
	Clock       core.Clock
}

// Options tunes New.
type Options struct {
	Store  identity.Store // default: a new MemStore
	Limits identity.Limits
	// LimiterFactory backs the auth throttles (default: in-process GCRA);
	// tests inject a failing one to prove the throttles fail closed.
	LimiterFactory identity.LimiterFactory
}

// CountingHasher wraps auth.Hasher with weak (fast) parameters and counts
// Verify calls, so tests can prove both login paths do the same hashing.
type CountingHasher struct {
	*auth.Hasher
	verifies chan struct{}
}

// Verify implements auth.PasswordHasher.
func (h *CountingHasher) Verify(password, encoded string) (bool, error) {
	select {
	case h.verifies <- struct{}{}:
	default:
	}
	return h.Hasher.Verify(password, encoded)
}

// Verifies drains and returns the number of Verify calls since the last
// call.
func (h *CountingHasher) Verifies() int {
	n := 0
	for {
		select {
		case <-h.verifies:
			n++
		default:
			return n
		}
	}
}

// New returns a fixture on clock.
func New(t testing.TB, clock core.Clock, opt Options) *Fixture {
	t.Helper()
	ctx := context.Background()
	st := opt.Store
	mem, _ := st.(*identity.MemStore)
	if st == nil {
		mem = identity.NewMemStore()
		st = mem
	}
	ids := core.NewSplitMix64(uint64(clock.Now().UnixNano()) ^ 0x5eed)
	keys, err := identity.NewKeyManager(ctx, identity.KeyManagerConfig{
		Store: st, Secrets: identity.NewMemKeyStore(), Clock: clock, IDs: ids, Random: rand.Reader, Issuer: Issuer,
	})
	if err != nil {
		t.Fatal(err)
	}
	weak, err := auth.NewHasher(rand.Reader, auth.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}, auth.AllowWeakParams())
	if err != nil {
		t.Fatal(err)
	}
	h := &CountingHasher{Hasher: weak, verifies: make(chan struct{}, 1<<16)}
	rev := identity.NewMemRevocations(clock)
	lim := identity.NewRateLimiter(opt.LimiterFactory, clock)
	email := &identity.MemEmailSender{}
	svc, err := identity.New(identity.Config{
		Store: st, Keys: keys, Revocations: rev, Limiter: lim, Lockouts: auth.NewInMemoryLockoutStore(),
		Passwords: h, Policy: auth.PasswordPolicy{Breached: auth.NewBreachedSet("password1234", "correct horse")},
		Email: email, Clock: clock, IDs: ids, Random: rand.Reader, PublicURL: "https://console.flowforge.test",
		Limits: opt.Limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.Verifies()
	return &Fixture{Service: svc, Store: st, Mem: mem, Keys: keys, Revocations: rev, Email: email, Limiter: lim, Hasher: h, Clock: clock}
}

// Account is a ready-to-use tenant member.
type Account struct {
	UserID   core.UUID
	TenantID core.UUID
	Email    string
	Password string
	Role     auth.Role
}

// Password is the password of every seeded account.
const Password = "seeded account password 1"

// SeedTenant creates an active tenant with one active member per role;
// emails are <role>@<slug>.test.
func (f *Fixture) SeedTenant(t testing.TB, slug string) (core.UUID, map[auth.Role]Account) {
	t.Helper()
	return f.SeedTenantPlan(t, slug, "free")
}

// SeedTenantPlan is SeedTenant with a plan.
func (f *Fixture) SeedTenantPlan(t testing.TB, slug, plan string) (core.UUID, map[auth.Role]Account) {
	t.Helper()
	ctx := context.Background()
	ids := core.NewSplitMix64(uint64(len(slug)) * 7919)
	for _, c := range slug {
		ids = core.NewSplitMix64(ids.Uint64() ^ uint64(c))
	}
	gen := core.IDGen{Clock: f.Clock, Rng: ids}
	now := f.Clock.Now()
	tid := gen.New()
	if err := f.Store.CreateTenant(ctx, identity.Tenant{ID: tid, Name: slug, Slug: slug, Plan: plan, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	hash, err := f.Hasher.Hash(Password)
	if err != nil {
		t.Fatal(err)
	}
	out := map[auth.Role]Account{}
	for i, r := range auth.Roles() {
		uid := gen.New()
		email := string(r) + "@" + slug + ".test"
		if err := f.Store.CreateUser(ctx, identity.User{
			ID: uid, Email: email, PasswordHash: hash, Status: auth.UserActive, CreatedAt: now, EmailVerifiedAt: now, SessionVersion: 1,
		}); err != nil {
			t.Fatal(err)
		}
		if err := f.Store.CreateMembership(ctx, identity.Membership{
			TenantID: tid, UserID: uid, Role: r, CreatedAt: now.Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatal(err)
		}
		out[r] = Account{UserID: uid, TenantID: tid, Email: email, Password: Password, Role: r}
	}
	return tid, out
}

// AccessToken mints an access token for a seeded account (session family
// started through the real refresher).
func (f *Fixture) AccessToken(t testing.TB, a Account) string {
	t.Helper()
	s, err := f.Service.Login(context.Background(), identity.LoginInput{Email: a.Email, Password: a.Password, TenantID: a.TenantID},
		identity.ClientInfo{IP: "seed-" + a.Email})
	if err != nil {
		t.Fatalf("seed login %s: %v", a.Email, err)
	}
	return s.AccessToken
}
