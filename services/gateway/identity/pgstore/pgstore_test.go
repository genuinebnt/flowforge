package pgstore_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/migrations"
	"github.com/genuinebnt/flowforge/services/gateway/identity"
	"github.com/genuinebnt/flowforge/services/gateway/identity/identitytest"
	"github.com/genuinebnt/flowforge/services/gateway/identity/pgstore"
	"github.com/genuinebnt/flowforge/testkit"
)

var dbSeq int

// freshStore creates a throwaway database on FF_TEST_PG_DSN, applies the
// migrations and returns a Store on it.
func freshStore(t *testing.T) *pgstore.Store {
	t.Helper()
	st, _ := freshStorePool(t)
	return st
}

// freshStorePool is freshStore that also returns the pool, for tests that
// inspect the database directly.
func freshStorePool(t *testing.T) (*pgstore.Store, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("FF_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("FF_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	dbSeq++
	name := fmt.Sprintf("ff_identity_%d_%d", os.Getpid(), dbSeq)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = admin.Close(ctx)
	})
	if err := migrations.Apply(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pgstore.New(pool), pool
}

func TestIdentityPGStoreContract(t *testing.T) {
	identitytest.RunStoreContract(t, func(t *testing.T) identity.Store { return freshStore(t) })
}

func TestIdentityPGFlows(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	clock := testkit.NewFakeClock(time.Date(2026, 11, 12, 13, 0, 0, 0, time.UTC))
	f := identitytest.New(t, clock, identitytest.Options{Store: st})
	ci := identity.ClientInfo{IP: "203.0.113.5", UserAgent: "test"}
	pw := "correct horse battery staple"
	res, err := f.Service.Signup(ctx, identity.SignupInput{Email: "dev@example.com", Password: pw, TenantName: "Example Security"}, ci)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := f.Email.Last("dev@example.com", identity.EmailVerify)
	if _, err := f.Service.VerifyEmail(ctx, m.Token, ci); err != nil {
		t.Fatal(err)
	}
	s, err := f.Service.Login(ctx, identity.LoginInput{Email: "DEV@example.com", Password: pw}, ci)
	if err != nil {
		t.Fatal(err)
	}
	if s.Tenant.ID != res.TenantID || s.Role != auth.RoleOwner {
		t.Fatalf("session %+v", s)
	}
	s2, err := f.Service.Refresh(ctx, s.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Service.Refresh(ctx, s.RefreshToken); err == nil {
		t.Fatal("reuse accepted")
	}
	if _, err := f.Service.Refresh(ctx, s2.RefreshToken); err == nil {
		t.Fatal("family survived reuse")
	}
	u, _ := st.GetUser(ctx, res.UserID)
	if u.SessionVersion != 2 {
		t.Fatalf("session_version %d", u.SessionVersion)
	}
	k, err := f.Service.CreateAPIKey(ctx, identity.Actor{TenantID: res.TenantID, UserID: res.UserID, Role: auth.RoleOwner}, "ci", auth.RoleAnalyst, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Service.AuthenticateAPIKey(ctx, k.Token); err != nil {
		t.Fatal(err)
	}
	// Key rotation persists through the store.
	clock.Advance(identity.RotationPeriod)
	if err := f.Keys.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	keys, _ := st.ListSigningKeys(ctx)
	if len(keys) != 2 || keys[0].Status != auth.KeyRetiring || keys[1].Status != auth.KeyActive {
		t.Fatalf("keys after rotation: %+v", keys)
	}
}

// TestRegress_G_SVC_23_RoleGroupAddRemoveRace: removing a group's last
// member deleted the member and the group in one statement, whose snapshot
// could not see a member added concurrently; the group delete then
// cascaded to that new member, which vanished although its add had
// succeeded. Both orders are driven here with real transactions: the
// second operation is started only once it is waiting on the first's
// row lock.
func TestRegress_G_SVC_23_RoleGroupAddRemoveRace(t *testing.T) {
	ctx := context.Background()
	st, pool := freshStorePool(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	tenant := core.MustParseUUID("0190a1b2-0000-7000-8000-00000000a001")
	if err := st.CreateTenant(ctx, identity.Tenant{ID: tenant, Name: "t", Slug: "t", Plan: "free", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	users := map[string]core.UUID{}
	for i, n := range []string{"x", "y"} {
		u := core.MustParseUUID(fmt.Sprintf("0190a1b2-0000-7000-8000-0000000000%02x", i+1))
		users[n] = u
		if err := st.CreateUser(ctx, identity.User{ID: u, Email: n + "@t.test", PasswordHash: "h", Status: auth.UserActive, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateMembership(ctx, identity.Membership{TenantID: tenant, UserID: u, Role: auth.RoleViewer, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	// waitForLockWaiter polls until a session of this database waits on a
	// lock, so the second operation is known to be blocked.
	waitForLockWaiter := func() {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n > 0 {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("the second operation never waited on the first")
	}
	groupsOf := func(u core.UUID) []string {
		t.Helper()
		gs, err := st.UserRoleGroups(ctx, tenant, u)
		if err != nil {
			t.Fatal(err)
		}
		return gs
	}

	t.Run("add commits first", func(t *testing.T) {
		if err := st.AddRoleGroupMember(ctx, tenant, "g1", users["y"], now); err != nil {
			t.Fatal(err)
		}
		added, release := make(chan struct{}), make(chan struct{})
		defer releaseOnce(release)
		addDone := make(chan error, 1)
		go func() {
			addDone <- st.WithTx(ctx, func(tx identity.Store) error {
				if err := tx.AddRoleGroupMember(ctx, tenant, "g1", users["x"], now); err != nil {
					return err
				}
				close(added)
				<-release
				return nil
			})
		}()
		<-added
		removeDone := make(chan error, 1)
		go func() {
			ok, err := st.RemoveRoleGroupMember(ctx, tenant, "g1", users["y"])
			if err == nil && !ok {
				err = fmt.Errorf("y was not removed")
			}
			removeDone <- err
		}()
		waitForLockWaiter()
		releaseOnce(release)
		if err := <-addDone; err != nil {
			t.Fatal(err)
		}
		if err := <-removeDone; err != nil {
			t.Fatal(err)
		}
		if gs := groupsOf(users["x"]); len(gs) != 1 || gs[0] != "g1" {
			t.Fatalf("x's groups after a concurrent remove of y: %v", gs)
		}
		if gs := groupsOf(users["y"]); len(gs) != 0 {
			t.Fatalf("y's groups: %v", gs)
		}
	})

	t.Run("remove commits first", func(t *testing.T) {
		if err := st.AddRoleGroupMember(ctx, tenant, "g2", users["y"], now); err != nil {
			t.Fatal(err)
		}
		removed, release := make(chan struct{}), make(chan struct{})
		defer releaseOnce(release)
		removeDone := make(chan error, 1)
		go func() {
			removeDone <- st.WithTx(ctx, func(tx identity.Store) error {
				if ok, err := tx.RemoveRoleGroupMember(ctx, tenant, "g2", users["y"]); err != nil || !ok {
					return fmt.Errorf("remove y: %v %v", ok, err)
				}
				close(removed)
				<-release
				return nil
			})
		}()
		<-removed
		addDone := make(chan error, 1)
		go func() { addDone <- st.AddRoleGroupMember(ctx, tenant, "g2", users["x"], now) }()
		waitForLockWaiter()
		releaseOnce(release)
		if err := <-removeDone; err != nil {
			t.Fatal(err)
		}
		if err := <-addDone; err != nil {
			t.Fatalf("add after a concurrent remove of the last member: %v", err)
		}
		if gs := groupsOf(users["x"]); !slices.Contains(gs, "g2") {
			t.Fatalf("x's groups: %v", gs)
		}
	})
}

// releaseOnce closes c unless it is closed, so a failing test still ends
// the transaction it holds open (the pool cannot close with it checked
// out).
func releaseOnce(c chan struct{}) {
	select {
	case <-c:
	default:
		close(c)
	}
}
