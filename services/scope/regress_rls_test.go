package scope_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/migrations"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
	scopesvc "github.com/genuinebnt/flowforge/services/scope"
	"github.com/genuinebnt/flowforge/testkit"
)

// TestRegress_G_OPS_02_ScopeAsServiceRole runs the scope repository as
// ff_scope, the service role of migration 0018 (own schema only, row-level
// security forced, no app.tenant_id set): the request paths work, and the
// cross-tenant activation scan (DueVersions, G-CORE-01) sees every tenant's
// future-dated versions through the svc_ff_scope policy.
func TestRegress_G_OPS_02_ScopeAsServiceRole(t *testing.T) {
	ctx := context.Background()
	dsn := servicetest.FreshDB(t)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	if err := migrations.Apply(ctx, admin); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["role"] = "ff_scope"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var user string
	if err := pool.QueryRow(ctx, `SELECT current_user`).Scan(&user); err != nil || user != "ff_scope" {
		t.Fatalf("connected as %q (%v)", user, err)
	}

	runEffectiveFrom(t, &scopesvc.PgRepo{DB: pool})

	repo := &scopesvc.PgRepo{DB: pool}
	clock := testkit.NewFakeClock(time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC))
	srv := scopesvc.NewServer(scopesvc.Options{Repo: repo, Clock: clock, Resolver: servicetest.NewResolver(nil)})
	future := clock.Now().Add(time.Hour)
	for i, tenant := range []string{servicetest.TenantA.String(), servicetest.TenantB.String()} {
		id := platform.Identity{Tenant: servicetest.TenantA, Actor: servicetest.UserA, Role: "admin"}
		if i == 1 {
			id = platform.Identity{Tenant: servicetest.TenantB, Actor: servicetest.UserB, Role: "admin"}
		}
		c := platform.WithIdentity(ctx, id)
		p, err := srv.CreateProgram(c, &pb.CreateProgramRequest{Name: "due-" + tenant[:8], InitialScope: servicetest.ScopeDoc("a.test")})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := srv.PutScope(c, &pb.PutScopeRequest{ProgramId: p.GetId(), BaseVersion: 1,
			Document: servicetest.ScopeDoc("a.test", "b.test"), EffectiveFrom: timestamppb.New(future)}); err != nil {
			t.Fatal(err)
		}
	}
	due, err := repo.DueVersions(ctx, clock.Now(), nil, future.Add(time.Minute), 100)
	if err != nil {
		t.Fatalf("DueVersions as ff_scope: %v", err)
	}
	tenants := map[string]bool{}
	for _, d := range due {
		tenants[d.Tenant.String()] = true
	}
	if !tenants[servicetest.TenantA.String()] || !tenants[servicetest.TenantB.String()] {
		t.Fatalf("DueVersions as ff_scope saw tenants %v, want both", tenants)
	}
	// The role is still denied outside its schema.
	if _, err := pool.Exec(ctx, `SELECT 1 FROM engine.runs`); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("ff_scope reading engine.runs: %v, want permission denied", err)
	}
}
