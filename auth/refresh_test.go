package auth_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

type refreshFixture struct {
	clk          *testkit.FakeClock
	store        *auth.InMemoryRefreshStore
	r            *auth.Refresher
	user, tenant core.UUID
}

func newRefreshFixture(t *testing.T) refreshFixture {
	t.Helper()
	clk := testkit.NewFakeClock(specNow)
	store := auth.NewInMemoryRefreshStore()
	r, err := auth.NewRefresher(auth.RefresherConfig{Store: store, Clock: clk, Random: newReader(9), IDs: core.NewSplitMix64(9)})
	if err != nil {
		t.Fatal(err)
	}
	ids := core.IDGen{Clock: clk, Rng: core.NewSplitMix64(10)}
	return refreshFixture{clk: clk, store: store, r: r, user: ids.New(), tenant: ids.New()}
}

func TestAuth003_RefreshReuse(t *testing.T) {
	f := newRefreshFixture(t)
	ctx := context.Background()
	first, err := f.r.Start(ctx, f.user, f.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first.Token, auth.RefreshTokenPrefix) || first.Row.TokenHash != auth.HashToken(first.Token) {
		t.Fatalf("issued token %q / row hash mismatch", first.Token)
	}
	f.clk.Advance(time.Minute)
	second, err := f.r.Rotate(ctx, first.Token)
	if err != nil {
		t.Fatal(err)
	}
	if second.Row.FamilyID != first.Row.FamilyID || second.Row.ParentID != first.Row.ID || second.Token == first.Token {
		t.Fatalf("rotation row = %+v", second.Row)
	}
	svBefore := f.store.SessionVersion(f.user)

	// The old token is presented again: theft suspected.
	f.clk.Advance(time.Minute)
	_, err = f.r.Rotate(ctx, first.Token)
	if !errors.Is(err, auth.ErrRefreshReused) || !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("reuse: %v, want ErrRefreshReused", err)
	}
	for _, row := range f.store.Family(first.Row.FamilyID) {
		if row.RevokedAt.IsZero() {
			t.Fatalf("family row %s not revoked", row.ID)
		}
	}
	if got := f.store.SessionVersion(f.user); got != svBefore+1 {
		t.Fatalf("session version = %d, want %d", got, svBefore+1)
	}
	ev := f.store.AuditEvents()
	if len(ev) != 1 || ev[0].Event != auth.AuditRefreshReuse || ev[0].UserID != f.user || ev[0].Detail["family_id"] != first.Row.FamilyID.String() {
		t.Fatalf("audit = %+v", ev)
	}
	// The legitimate child is dead too, and so is the reused token.
	for _, tok := range []string{second.Token, first.Token} {
		if _, err := f.r.Rotate(ctx, tok); !errors.Is(err, auth.ErrRefreshRevoked) {
			t.Fatalf("Rotate after family revocation: %v, want ErrRefreshRevoked", err)
		}
	}
	if len(f.store.AuditEvents()) != 1 || f.store.SessionVersion(f.user) != svBefore+1 {
		t.Fatal("rejections after revocation must not bump or audit again")
	}
}

func TestRefreshExpiry(t *testing.T) {
	f := newRefreshFixture(t)
	ctx := context.Background()
	start := f.clk.Now()
	cur, err := f.r.Start(ctx, f.user, f.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if want := start.Add(auth.RefreshSlidingTTL); !cur.Row.ExpiresAt.Equal(want) {
		t.Fatalf("first expiry = %v, want %v", cur.Row.ExpiresAt, want)
	}
	// Sliding: each rotation within 30 days extends the expiry...
	for _, day := range []int{20, 40, 60, 80} {
		f.clk.SetWall(start.Add(time.Duration(day) * 24 * time.Hour))
		cur, err = f.r.Rotate(ctx, cur.Token)
		if err != nil {
			t.Fatalf("day %d: %v", day, err)
		}
		want := f.clk.Now().Add(auth.RefreshSlidingTTL)
		if abs := start.Add(auth.RefreshAbsoluteTTL); abs.Before(want) {
			want = abs // ...but never past 90 days from the family start.
		}
		if !cur.Row.ExpiresAt.Equal(want) {
			t.Fatalf("day %d: expiry = %v, want %v", day, cur.Row.ExpiresAt, want)
		}
	}
	f.clk.SetWall(start.Add(auth.RefreshAbsoluteTTL))
	if _, err := f.r.Rotate(ctx, cur.Token); err != nil {
		t.Fatalf("at exactly expires_at the token is still valid (13.4 rejects expires_at < now): %v", err)
	}

	idle, err := f.r.Start(ctx, f.user, f.tenant)
	if err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(auth.RefreshSlidingTTL + time.Second)
	if _, err := f.r.Rotate(ctx, idle.Token); !errors.Is(err, auth.ErrRefreshExpired) {
		t.Fatalf("idle 30 days: %v, want ErrRefreshExpired", err)
	}
}

func TestRefreshRejectsUnknown(t *testing.T) {
	f := newRefreshFixture(t)
	valid, _ := auth.NewRefreshToken(newReader(77))
	for _, tok := range []string{"", "ffr_", "ffr_short", "bearer xyz", valid} {
		if _, err := f.r.Rotate(context.Background(), tok); !errors.Is(err, auth.ErrRefreshUnknown) {
			t.Errorf("Rotate(%q) = %v, want ErrRefreshUnknown", tok, err)
		}
	}
}

func TestRefreshConcurrentRotation(t *testing.T) {
	f := newRefreshFixture(t)
	ctx := context.Background()
	first, err := f.r.Start(ctx, f.user, f.tenant)
	if err != nil {
		t.Fatal(err)
	}
	// The Refresher's random reader is not concurrency-safe in this test,
	// so only the store and id paths race; at most one rotation may win.
	var (
		mu   sync.Mutex
		wins int
		wg   sync.WaitGroup
	)
	r, err := auth.NewRefresher(auth.RefresherConfig{Store: f.store, Clock: f.clk, Random: &lockedReader{r: newReader(3)}, IDs: core.NewSplitMix64(4)})
	if err != nil {
		t.Fatal(err)
	}
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Rotate(ctx, first.Token); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			} else if !errors.Is(err, auth.ErrInvalidRefresh) {
				t.Errorf("Rotate: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins > 1 {
		t.Fatalf("%d concurrent rotations of one token succeeded", wins)
	}
}

type lockedReader struct {
	mu sync.Mutex
	r  *detReader
}

func (l *lockedReader) Read(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.r.Read(p)
}

func TestRevokeSession(t *testing.T) {
	f := newRefreshFixture(t)
	ctx := context.Background()
	tok, _ := f.r.Start(ctx, f.user, f.tenant)
	if err := f.r.RevokeSession(ctx, tok.Row.FamilyID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Rotate(ctx, tok.Token); !errors.Is(err, auth.ErrRefreshRevoked) {
		t.Fatalf("Rotate after logout: %v", err)
	}
}
