package pgstore_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/genuinebnt/flowforge/auth"
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
	return pgstore.New(pool)
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
