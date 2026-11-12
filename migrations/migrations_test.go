package migrations_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/genuinebnt/flowforge/migrations"
	"github.com/genuinebnt/flowforge/testkit"
)

func TestMigrate_EmbeddedFiles(t *testing.T) {
	all, err := migrations.All()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"0001_extensions.sql", "0002_identity.sql", "0003_scope.sql", "0004_engine.sql",
		"0005_graph.sql", "0006_notifier.sql", "0007_rls.sql",
	}
	for i, w := range want {
		if got := all[i].File(); got != w {
			t.Errorf("migration %d = %s, want %s", i+1, got, w)
		}
	}
	var vector int
	for _, m := range all {
		if m.RequiresExtension == "vector" {
			vector++
			if !strings.Contains(m.SQL, "graph.embeddings") {
				t.Errorf("%s requires vector but does not define graph.embeddings", m.File())
			}
		} else if m.RequiresExtension != "" {
			t.Errorf("%s requires unexpected extension %q", m.File(), m.RequiresExtension)
		}
		if strings.Contains(m.SQL, "CREATE EXTENSION IF NOT EXISTS vector") && m.RequiresExtension != "vector" {
			t.Errorf("%s creates the vector extension without declaring it", m.File())
		}
	}
	if vector != 1 {
		t.Errorf("want exactly one migration requiring vector, got %d", vector)
	}
}

// freshDB creates an empty database on the FF_TEST_PG_DSN server and
// returns a config for it; the database is dropped when the test ends.
func freshDB(t *testing.T) *pgx.ConnConfig {
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
	name := fmt.Sprintf("ff_migrate_%d", testkit.Seed(t)%1_000_000_000)
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = admin.Close(ctx)
	})
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = name
	return cfg
}

func connect(t *testing.T, cfg *pgx.ConnConfig) *pgx.Conn {
	t.Helper()
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func TestMigrate_ApplyConcurrentAndTwice(t *testing.T) {
	cfg := freshDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Four migrators race on an empty database.
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		conn := connect(t, cfg)
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = migrations.Apply(ctx, conn)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("migrator %d: %v", i, err)
		}
	}

	conn := connect(t, cfg)
	res, err := migrations.ApplyReport(ctx, conn)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	var vectorAvailable bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'vector')`).
		Scan(&vectorAvailable); err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.Changed {
			t.Errorf("%s changed on the idempotent re-run", r.Migration.File())
		}
		want := migrations.StatusApplied
		if r.Migration.RequiresExtension == "vector" && !vectorAvailable {
			want = migrations.StatusSkipped
		}
		if r.Status != want {
			t.Errorf("%s: status %s, want %s", r.Migration.File(), r.Status, want)
		}
	}
	var rows int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM public.schema_migrations`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != len(res) {
		t.Errorf("schema_migrations has %d rows, want %d", rows, len(res))
	}

	for _, table := range []string{
		"identity.tenants", "identity.outbox", "identity.http_idempotency", "scope.scope_decisions",
		"scope.outbox", "engine.runs", "engine.run_events", "engine.processed_events", "engine.workers",
		"graph.entities", "graph.snapshots", "graph.change_events", "notifier.processed_events",
	} {
		var ok bool
		if err := conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&ok); err != nil || !ok {
			t.Errorf("table %s missing (err %v)", table, err)
		}
	}
	var embeddings bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('graph.embeddings') IS NOT NULL`).Scan(&embeddings); err != nil {
		t.Fatal(err)
	}
	if embeddings != vectorAvailable {
		t.Errorf("graph.embeddings exists = %v, vector available = %v", embeddings, vectorAvailable)
	}
}

func TestMigrate_ChecksumMismatchRefused(t *testing.T) {
	cfg := freshDB(t)
	ctx := context.Background()
	conn := connect(t, cfg)
	if err := migrations.Apply(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE public.schema_migrations SET checksum = '\x00' WHERE version = 2`); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Apply(ctx, conn); !errors.Is(err, migrations.ErrChecksumMismatch) {
		t.Fatalf("Apply after edit: %v, want ErrChecksumMismatch", err)
	}
}

func TestMigrate_Semantics(t *testing.T) {
	cfg := freshDB(t)
	ctx := context.Background()
	conn := connect(t, cfg)
	if err := migrations.Apply(ctx, conn); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	tenant := "11111111-1111-7111-8111-111111111111"
	other := "22222222-2222-7222-8222-222222222222"

	t.Run("run_events append-only", func(t *testing.T) {
		exec(`INSERT INTO engine.workflows (id, tenant_id, name) VALUES ($1, $2, 'wf')`,
			"aaaaaaaa-0000-7000-8000-000000000001", tenant)
		exec(`INSERT INTO engine.workflow_versions (id, workflow_id, version, source, source_sha256, module,
			module_sha256, effects, inferred_class, created_by)
			VALUES ($1, $2, 1, '', '\x00', '\x00', '\x00', '{}', 'none', $3)`,
			"aaaaaaaa-0000-7000-8000-000000000002", "aaaaaaaa-0000-7000-8000-000000000001", tenant)
		exec(`INSERT INTO engine.runs (id, tenant_id, workflow_version_id, status, created_by, idempotency_key, request_hash)
			VALUES ($1, $2, $3, 'pending', 'user:x', 'k1', '\x00')`,
			"aaaaaaaa-0000-7000-8000-000000000003", tenant, "aaaaaaaa-0000-7000-8000-000000000002")
		exec(`INSERT INTO engine.run_events (run_id, seq, type, actor, payload) VALUES ($1, 1, 'log', 'engine', '{}')`,
			"aaaaaaaa-0000-7000-8000-000000000003")
		if _, err := conn.Exec(ctx, `UPDATE engine.run_events SET type = 'x'`); err == nil {
			t.Error("UPDATE on run_events succeeded")
		}
		if _, err := conn.Exec(ctx, `DELETE FROM engine.run_events`); err == nil {
			t.Error("DELETE on run_events succeeded")
		}
	})

	t.Run("scope decision partitions", func(t *testing.T) {
		// Far-future month: lands in the default partition, then moves.
		exec(`INSERT INTO scope.scope_decisions (id, tenant_id, program_id, scope_version, target, class, decision, reason, decided_at)
			VALUES ($1, $2, $2, 1, 'a.example', 'passive', 'allow', 'rule', '2099-03-15T12:00:00Z')`,
			"bbbbbbbb-0000-7000-8000-000000000001", tenant)
		var part string
		if err := conn.QueryRow(ctx, `SELECT tableoid::regclass::text FROM scope.scope_decisions`).Scan(&part); err != nil {
			t.Fatal(err)
		}
		if part != "scope.scope_decisions_default" {
			t.Fatalf("row in %s, want the default partition", part)
		}
		for range 2 { // idempotent
			if err := conn.QueryRow(ctx, `SELECT scope.ensure_decision_partition('2099-03-01T00:00:00Z')`).Scan(&part); err != nil {
				t.Fatal(err)
			}
		}
		if part != "scope.scope_decisions_y2099m03" {
			t.Fatalf("partition %s", part)
		}
		if err := conn.QueryRow(ctx, `SELECT tableoid::regclass::text FROM scope.scope_decisions`).Scan(&part); err != nil {
			t.Fatal(err)
		}
		if part != "scope.scope_decisions_y2099m03" {
			t.Fatalf("row in %s after ensure_decision_partition", part)
		}
		var cur int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_inherits WHERE inhparent = 'scope.scope_decisions'::regclass`).
			Scan(&cur); err != nil {
			t.Fatal(err)
		}
		if cur != 4 { // default, this month, next month, 2099-03
			t.Errorf("%d partitions, want 4", cur)
		}
	})

	t.Run("row level security", func(t *testing.T) {
		role := fmt.Sprintf("ff_app_%d", testkit.Seed(t)%1_000_000)
		exec(`CREATE ROLE ` + role)
		t.Cleanup(func() {
			_, _ = conn.Exec(ctx, `RESET ROLE`)
			_, _ = conn.Exec(ctx, `DROP OWNED BY `+role)
			_, _ = conn.Exec(ctx, `DROP ROLE `+role)
		})
		exec(`GRANT USAGE ON SCHEMA scope TO ` + role)
		exec(`GRANT SELECT, INSERT ON scope.programs TO ` + role)
		exec(`INSERT INTO scope.programs (id, tenant_id, name) VALUES ($1, $2, 'mine'), ($3, $4, 'theirs')`,
			"cccccccc-0000-7000-8000-000000000001", tenant, "cccccccc-0000-7000-8000-000000000002", other)

		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+role); err != nil {
			t.Fatal(err)
		}
		count := func() int {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM scope.programs`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		if n := count(); n != 0 {
			t.Errorf("without app.tenant_id the app role sees %d programs, want 0 (fail closed)", n)
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
			t.Fatal(err)
		}
		if n := count(); n != 1 {
			t.Errorf("with app.tenant_id the app role sees %d programs, want 1", n)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO scope.programs (id, tenant_id, name) VALUES ($1, $2, 'sneaky')`,
			"cccccccc-0000-7000-8000-000000000003", other); err == nil {
			t.Error("insert for another tenant passed the RLS check")
		}
	})
}
