package pg_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/genuinebnt/flowforge/contentstore"
	"github.com/genuinebnt/flowforge/contentstore/pg"
	"github.com/genuinebnt/flowforge/contentstore/storetest"
	"github.com/genuinebnt/flowforge/migrations"
)

var (
	tmplOnce sync.Once
	tmplName string
	tmplErr  error
	dbSeq    atomic.Int64
)

func dsn(t testing.TB) string {
	d := os.Getenv("FF_TEST_PG_DSN")
	if d == "" {
		t.Skip("FF_TEST_PG_DSN not set")
	}
	return d
}

func TestMain(m *testing.M) {
	code := m.Run()
	if tmplName != "" {
		ctx := context.Background()
		if c, err := pgx.Connect(ctx, os.Getenv("FF_TEST_PG_DSN")); err == nil {
			_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+tmplName+" WITH (FORCE)")
			_ = c.Close(ctx)
		}
	}
	os.Exit(code)
}

// template is a database with every migration applied and the roles
// synced, cloned per test.
func template(t testing.TB) string {
	t.Helper()
	base := dsn(t)
	tmplOnce.Do(func() {
		ctx := context.Background()
		admin, err := pgx.Connect(ctx, base)
		if err != nil {
			tmplErr = err
			return
		}
		defer admin.Close(ctx)
		name := fmt.Sprintf("ff_content_tmpl_%d", os.Getpid())
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
			tmplErr = err
			return
		}
		cfg, _ := pgx.ParseConfig(base)
		cfg.Database = name
		c, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			tmplErr = err
			return
		}
		_, tmplErr = migrations.Deploy(ctx, c)
		_ = c.Close(ctx)
		tmplName = name
	})
	if tmplErr != nil {
		t.Fatalf("template database: %v", tmplErr)
	}
	return tmplName
}

// freshPoolAs returns a pool on a new clone of the template whose
// connections run as role (the "role" startup parameter), so row-level
// security applies as in production.
func freshPoolAs(t testing.TB, role string) *pgxpool.Pool {
	t.Helper()
	tmpl := template(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("ff_content_%d_%d", os.Getpid(), dbSeq.Add(1))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name+" TEMPLATE "+tmpl); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	if role != "" {
		cfg.ConnConfig.RuntimeParams["role"] = role
	}
	cfg.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = admin.Close(ctx)
	})
	return pool
}

// TestCStoreContract_Postgres runs the Repo contract on Postgres as the
// service role ff_content: every tenant read and write goes through RLS
// with app.tenant_id set per transaction.
func TestCStoreContract_Postgres(t *testing.T) {
	storetest.Run(t, func(t *testing.T) contentstore.Repo { return pg.New(freshPoolAs(t, "ff_content")) })
}

// TestCStoreRegress_RLSWithoutTenant: a transaction of ff_content that
// skips app.tenant_id sees no content rows at all (fail closed), and the
// role cannot read another service's schema.
func TestCStoreRegress_RLSWithoutTenant(t *testing.T) {
	pool := freshPoolAs(t, "ff_content")
	ctx := context.Background()
	r := pg.New(pool)
	s := storetest.SeedSnapshot(t, r)
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM content.snapshots`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("ff_content without a tenant sees %d snapshots (%v), want 0", n, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM content.snapshots WHERE id = $1`, s.String()).Scan(&n); err != nil || n != 0 {
		t.Fatalf("seeded snapshot visible without a tenant: %d %v", n, err)
	}
	if _, err := pool.Exec(ctx, `SELECT 1 FROM graph.entities`); err == nil {
		t.Fatal("ff_content reads the graph schema")
	}
	// Tenants() is the one cross-tenant read (content.programs policy).
	ts, err := r.Tenants(ctx)
	if err != nil || len(ts) != 1 {
		t.Fatalf("tenants %v %v", ts, err)
	}
}
