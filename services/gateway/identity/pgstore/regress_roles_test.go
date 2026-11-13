package pgstore_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/genuinebnt/flowforge/migrations"
	"github.com/genuinebnt/flowforge/services/gateway/identity"
	"github.com/genuinebnt/flowforge/services/gateway/identity/identitytest"
	"github.com/genuinebnt/flowforge/services/gateway/identity/pgstore"
)

// TestRegress_G_OPS_01_IdentityAsGatewayRole runs the identity store
// contract as ff_gateway, the service role of migration 0018 (identity
// schema only, forced row-level security, no app.tenant_id): login, refresh
// and API-key lookups precede the tenant and rely on the svc_ff_gateway
// policy; role groups (0015) are covered too.
func TestRegress_G_OPS_01_IdentityAsGatewayRole(t *testing.T) {
	identitytest.RunStoreContract(t, func(t *testing.T) identity.Store {
		dsn := os.Getenv("FF_TEST_PG_DSN")
		if dsn == "" {
			t.Skip("FF_TEST_PG_DSN not set")
		}
		ctx := context.Background()
		admin, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		dbSeq++
		name := fmt.Sprintf("ff_identity_role_%d_%d", os.Getpid(), dbSeq)
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
			t.Fatal(err)
		}
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		cfg.ConnConfig.Database = name
		owner, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := migrations.Apply(ctx, owner); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		owner.Close()
		cfg.ConnConfig.RuntimeParams["role"] = "ff_gateway"
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			pool.Close()
			_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = admin.Close(ctx)
		})
		return pgstore.New(pool)
	})
}
