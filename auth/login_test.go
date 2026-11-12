package auth_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// countingHasher records how much Argon2 work each login does.
type countingHasher struct {
	auth.PasswordHasher
	verifies atomic.Int64
}

func (c *countingHasher) Verify(pw, enc string) (bool, error) {
	c.verifies.Add(1)
	return c.PasswordHasher.Verify(pw, enc)
}

type loginFixture struct {
	clk      *testkit.FakeClock
	users    *auth.InMemoryUserStore
	throttle *auth.CountingThrottle
	hasher   *countingHasher
	svc      *auth.LoginService
}

const goodPassword = "correct horse battery staple"

func newLoginFixture(t *testing.T, cfg auth.LoginConfig) loginFixture {
	t.Helper()
	clk := testkit.NewFakeClock(specNow)
	h := &countingHasher{PasswordHasher: fastHasher(t, 11)}
	enc, err := h.Hash(goodPassword)
	if err != nil {
		t.Fatal(err)
	}
	ids := core.IDGen{Clock: clk, Rng: core.NewSplitMix64(12)}
	users := auth.NewInMemoryUserStore(
		auth.User{ID: ids.New(), Email: "dev@example.com", PasswordHash: enc, Status: auth.UserActive, SessionVersion: 1},
		auth.User{ID: ids.New(), Email: "new@example.com", PasswordHash: enc, Status: auth.UserPending, SessionVersion: 1},
		auth.User{ID: ids.New(), Email: "gone@example.com", PasswordHash: enc, Status: auth.UserDisabled, SessionVersion: 1},
	)
	throttle := auth.NewCountingThrottle(clk)
	cfg.Users, cfg.Throttle, cfg.Lockouts, cfg.Passwords, cfg.Clock = users, throttle, auth.NewInMemoryLockoutStore(), h, clk
	svc, err := auth.NewLoginService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return loginFixture{clk: clk, users: users, throttle: throttle, hasher: h, svc: svc}
}

func (f loginFixture) login(email, pw, ip string) error {
	_, err := f.svc.Login(context.Background(), auth.LoginRequest{Email: email, Password: pw, IP: ip})
	return err
}

func TestAuth005_NoEnumeration(t *testing.T) {
	// Generous per-email budget so the failure cool-down is what trips.
	f := newLoginFixture(t, auth.LoginConfig{EmailLimit: auth.Limit{Count: 100, Per: 10 * time.Minute}, IPLimit: auth.Limit{Count: 1000, Per: 10 * time.Minute}})

	t.Run("same response for unknown and known email", func(t *testing.T) {
		before := f.hasher.verifies.Load()
		errUnknown := f.login("nobody@example.com", "whatever password", "198.51.100.1")
		mid := f.hasher.verifies.Load()
		errKnown := f.login("dev@example.com", "wrong password here", "198.51.100.1")
		after := f.hasher.verifies.Load()
		if errUnknown != errKnown || !errors.Is(errUnknown, auth.ErrInvalidCredentials) {
			t.Fatalf("unknown email: %v; wrong password: %v; want the identical ErrInvalidCredentials", errUnknown, errKnown)
		}
		if mid-before != 1 || after-mid != 1 {
			t.Fatalf("Argon2 verifications: unknown %d, known %d; want 1 each", mid-before, after-mid)
		}
	})

	t.Run("cool-down after 10 failures applies to both", func(t *testing.T) {
		for _, email := range []string{"ghost@example.com", "dev@example.com"} {
			if email == "dev@example.com" { // clear the failure left by the previous subtest
				if err := f.login(email, goodPassword, "198.51.100.2"); err != nil {
					t.Fatal(err)
				}
			}
			var last error
			for i := range 11 {
				last = f.login(email, "wrong password here", "198.51.100.2")
				if i < 10 && !errors.Is(last, auth.ErrInvalidCredentials) {
					t.Fatalf("%s failure %d: %v", email, i+1, last)
				}
			}
			var te *auth.ThrottledError
			if !errors.As(last, &te) || te.RetryAfter != auth.DefaultCooldown {
				t.Fatalf("%s 11th attempt: %v, want a 15-minute cool-down", email, last)
			}
			// Even the right password is refused during the cool-down.
			if err := f.login(email, goodPassword, "198.51.100.2"); !errors.Is(err, auth.ErrThrottled) {
				t.Fatalf("%s during cool-down: %v", email, err)
			}
		}
		f.clk.Advance(auth.DefaultCooldown)
		if err := f.login("dev@example.com", goodPassword, "198.51.100.2"); err != nil {
			t.Fatalf("after the cool-down: %v", err)
		}
	})

	t.Run("pending and disabled refused only with the right password", func(t *testing.T) {
		if err := f.login("new@example.com", "wrong password here", "198.51.100.3"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("pending user, wrong password: %v", err)
		}
		if err := f.login("NEW@example.com ", goodPassword, "198.51.100.3"); !errors.Is(err, auth.ErrPendingUser) {
			t.Fatalf("pending user: %v, want ErrPendingUser", err)
		}
		if err := f.login("gone@example.com", goodPassword, "198.51.100.3"); !errors.Is(err, auth.ErrUserDisabled) {
			t.Fatalf("disabled user: %v, want ErrUserDisabled", err)
		}
	})
}

func TestLoginThrottles(t *testing.T) {
	f := newLoginFixture(t, auth.LoginConfig{})
	ip := "203.0.113.9"
	// Per email: 10 per 10 minutes (successes count too).
	for i := range 10 {
		if err := f.login("dev@example.com", goodPassword, ip); err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	if err := f.login("dev@example.com", goodPassword, ip); !errors.Is(err, auth.ErrThrottled) {
		t.Fatalf("11th attempt for one email: %v", err)
	}
	if n := f.throttle.Count(auth.AccountThrottleKey(" DEV@example.com")); n != 10 {
		t.Fatalf("email key admitted %d, want 10 (key must be normalized)", n)
	}
	// Per IP: 20 per 10 minutes; 11 attempts so far.
	for i := range 9 {
		if err := f.login("other@example.com", "wrong password here", ip); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("IP attempt %d: %v", 12+i, err)
		}
	}
	err := f.login("third@example.com", "wrong password here", ip)
	var te *auth.ThrottledError
	if !errors.As(err, &te) || te.RetryAfter != 10*time.Minute {
		t.Fatalf("21st attempt from one IP: %v", err)
	}
	f.clk.Advance(10 * time.Minute)
	if err := f.login("dev@example.com", goodPassword, ip); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

func TestLoginRehash(t *testing.T) {
	f := newLoginFixture(t, auth.LoginConfig{})
	p := fastParams()
	p.Time = 2
	stronger, err := auth.NewHasher(newReader(13), p, auth.AllowWeakParams())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := auth.NewLoginService(auth.LoginConfig{
		Users: f.users, Throttle: f.throttle, Lockouts: auth.NewInMemoryLockoutStore(), Passwords: stronger, Clock: f.clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Login(context.Background(), auth.LoginRequest{Email: "dev@example.com", Password: goodPassword, IP: "192.0.2.1"})
	if err != nil || !res.Rehashed {
		t.Fatalf("Login = %+v, %v; want a rehash", res, err)
	}
	u, _ := f.users.Get("dev@example.com")
	if stronger.NeedsRehash(u.PasswordHash) || u.PasswordHash != res.User.PasswordHash {
		t.Fatalf("stored hash %q was not upgraded", u.PasswordHash)
	}
	res, err = svc.Login(context.Background(), auth.LoginRequest{Email: "dev@example.com", Password: goodPassword, IP: "192.0.2.1"})
	if err != nil || res.Rehashed {
		t.Fatalf("second Login = %+v, %v; want no rehash", res, err)
	}
}

func TestThrottleKeys(t *testing.T) {
	if got := auth.IPThrottleKey("192.0.2.1", "login"); got != "rl:auth:ip:192.0.2.1:login" {
		t.Errorf("IPThrottleKey = %q", got)
	}
	// sha256("dev@example.com")
	if got := auth.AccountThrottleKey("Dev@Example.com"); got != "rl:auth:acct:"+core.SHA256Hex([]byte("dev@example.com")) {
		t.Errorf("AccountThrottleKey = %q", got)
	}
}
