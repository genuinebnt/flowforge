package notifier_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/genuinebnt/flowforge/migrations"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
	"github.com/genuinebnt/flowforge/services/notifier"
)

// TestRegress_G_OPS_01_NotifierAsServiceRole runs the durable alert state
// scenario as ff_notifier, the service role of migration 0018 (notifier
// schema only, forced row-level security with the svc_ff_notifier policy).
func TestRegress_G_OPS_01_NotifierAsServiceRole(t *testing.T) {
	ctx := context.Background()
	dsn := servicetest.FreshDB(t)
	owner, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrations.Apply(ctx, owner); err != nil {
		t.Fatal(err)
	}
	owner.Close()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["role"] = "ff_notifier"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	throttleRestart(t, &notifier.PgRepo{DB: pool})
}
