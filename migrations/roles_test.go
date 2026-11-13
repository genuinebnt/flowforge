package migrations_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/genuinebnt/flowforge/migrations"
	"github.com/genuinebnt/flowforge/testkit"
)

const (
	tenantA = "00000000-0000-7000-8000-0000000000aa"
	tenantB = "00000000-0000-7000-8000-0000000000bb"
)

// roleFixtures inserts, as the superuser, one workflow, workflow version,
// run, run event, program and scope decision per tenant.
func roleFixtures(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	for i, tenant := range []string{tenantA, tenantB} {
		wf := fmt.Sprintf("dddddddd-0000-7000-8000-00000000000%d", i+1)
		wfv := fmt.Sprintf("dddddddd-0000-7000-8000-00000000001%d", i+1)
		run := fmt.Sprintf("dddddddd-0000-7000-8000-00000000002%d", i+1)
		prog := fmt.Sprintf("dddddddd-0000-7000-8000-00000000003%d", i+1)
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO engine.workflows (id, tenant_id, name) VALUES ($1, $2, 'wf')`, []any{wf, tenant}},
			{`INSERT INTO engine.workflow_versions (id, workflow_id, version, source, source_sha256, module,
				module_sha256, effects, inferred_class, created_by)
				VALUES ($1, $2, 1, '', '\x00', '\x00', '\x00', '{}', 'none', $3)`, []any{wfv, wf, tenant}},
			{`INSERT INTO engine.runs (id, tenant_id, workflow_version_id, status, created_by, idempotency_key, request_hash)
				VALUES ($1, $2, $3, 'pending', 'user:x', 'k', '\x00')`, []any{run, tenant, wfv}},
			{`INSERT INTO engine.run_events (run_id, seq, type, actor, payload) VALUES ($1, 1, 'log', 'engine', '{}')`, []any{run}},
			{`INSERT INTO scope.programs (id, tenant_id, name) VALUES ($1, $2, 'p')`, []any{prog, tenant}},
			{`INSERT INTO scope.scope_decisions (id, tenant_id, program_id, scope_version, target, class, decision, reason, decided_at)
				VALUES (gen_random_uuid(), $1, $2, 1, 'a.example', 'passive', 'allow', 'rule', '2099-01-15T00:00:00Z')`, []any{tenant, prog}},
		} {
			if _, err := conn.Exec(ctx, q.sql, q.args...); err != nil {
				t.Fatalf("fixture: %v\n%s", err, q.sql)
			}
		}
	}
}

// asRole runs fn in a transaction under SET LOCAL ROLE role, with
// app.tenant_id set when tenant is not empty. The transaction is rolled
// back.
func asRole(t *testing.T, ctx context.Context, conn *pgx.Conn, role, tenant string, fn func(pgx.Tx)) {
	t.Helper()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+pgx.Identifier{role}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if tenant != "" {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
			t.Fatal(err)
		}
	}
	fn(tx)
}

// probe runs sql in a savepoint and returns its error (nil, or the
// Postgres error), so one denied statement does not abort the
// transaction.
func probe(ctx context.Context, tx pgx.Tx, sql string, args ...any) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	_, err = sp.Exec(ctx, sql, args...)
	_ = sp.Rollback(ctx)
	return err
}

func denied(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "42501"
}

func count(t *testing.T, ctx context.Context, tx pgx.Tx, sql string) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(ctx, sql).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// TestRegress_G_OPS_01_ServiceRoleGrants pins the per-service roles of
// migration 0018 (spec 10, 17.2): each role reads and writes its own
// schema, is denied every other service schema, PUBLIC gets nothing, a
// stray grant is removed by the next sync, membership in cloudsqlsuperuser
// is revoked, and Deploy is idempotent.
func TestRegress_G_OPS_01_ServiceRoleGrants(t *testing.T) {
	cfg := freshDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn := connect(t, cfg)
	if _, err := migrations.Deploy(ctx, conn); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	roleFixtures(t, ctx, conn)

	// own: a statement that must succeed; other: one in another service's
	// schema that must be denied.
	cases := []struct {
		role, tenant, own, other string
	}{
		{"ff_gateway", "", `INSERT INTO identity.tenants (id, name, slug) VALUES (gen_random_uuid(), 'x', 'x')`, `SELECT 1 FROM engine.runs`},
		{"ff_engine", "", `UPDATE engine.runs SET priority = priority`, `SELECT 1 FROM identity.users`},
		{"ff_scope", "", `INSERT INTO scope.programs (id, tenant_id, name) VALUES (gen_random_uuid(), '` + tenantA + `', 'n')`, `SELECT 1 FROM graph.entities`},
		{"ff_graph", tenantA, `SELECT count(*) FROM graph.entities`, `SELECT 1 FROM scope.programs`},
		{"ff_notifier", "", `SELECT count(*) FROM notifier.channels`, `SELECT 1 FROM engine.workflows`},
		{"ff_similarity", "", `SELECT 1`, `SELECT 1 FROM graph.entities`},
		{"ff_maintenance", "", `SELECT (SELECT count(*) FROM engine.runs) + (SELECT count(*) FROM scope.programs)`, `SELECT 1 FROM public.schema_migrations`},
	}
	for _, c := range cases {
		t.Run(c.role, func(t *testing.T) {
			asRole(t, ctx, conn, c.role, c.tenant, func(tx pgx.Tx) {
				if err := probe(ctx, tx, c.own); err != nil {
					t.Errorf("own schema: %v\n%s", err, c.own)
				}
				if err := probe(ctx, tx, c.other); !denied(err) {
					t.Errorf("other schema: got %v, want permission denied\n%s", err, c.other)
				}
			})
		})
	}

	t.Run("PUBLIC", func(t *testing.T) {
		role := fmt.Sprintf("ff_nobody_%d", testkit.Seed(t)%1_000_000)
		if _, err := conn.Exec(ctx, `CREATE ROLE `+role); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = conn.Exec(ctx, `DROP ROLE IF EXISTS `+role) })
		asRole(t, ctx, conn, role, tenantA, func(tx pgx.Tx) {
			for _, q := range []string{`SELECT 1 FROM identity.users`, `SELECT 1 FROM engine.runs`,
				`SELECT 1 FROM graph.entities`, `SELECT 1 FROM public.schema_migrations`, `SELECT public.ff_sync_roles()`,
				`CREATE TABLE public.ff_squat (x int)`} {
				if err := probe(ctx, tx, q); !denied(err) {
					t.Errorf("%s: got %v, want permission denied", q, err)
				}
			}
		})
	})

	t.Run("stray grant removed by sync", func(t *testing.T) {
		if _, err := conn.Exec(ctx, `GRANT USAGE ON SCHEMA identity TO ff_engine; GRANT SELECT ON identity.users TO ff_engine`); err != nil {
			t.Fatal(err)
		}
		if err := migrations.SyncRoles(ctx, conn); err != nil {
			t.Fatal(err)
		}
		asRole(t, ctx, conn, "ff_engine", "", func(tx pgx.Tx) {
			if err := probe(ctx, tx, `SELECT 1 FROM identity.users`); !denied(err) {
				t.Errorf("after sync: %v, want permission denied", err)
			}
		})
	})

	t.Run("cloudsqlsuperuser membership", func(t *testing.T) {
		// Simulates Cloud SQL: the role exists cluster-wide and the API made
		// the service user a member.
		var existed bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cloudsqlsuperuser')`).Scan(&existed); err != nil {
			t.Fatal(err)
		}
		if !existed {
			if _, err := conn.Exec(ctx, `CREATE ROLE cloudsqlsuperuser NOLOGIN`); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = conn.Exec(context.Background(), `DROP ROLE IF EXISTS cloudsqlsuperuser`) })
		}
		if _, err := conn.Exec(ctx, `GRANT cloudsqlsuperuser TO ff_similarity`); err != nil {
			t.Fatal(err)
		}
		problems, err := migrations.RoleProblems(ctx, conn)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(problems, "ff_similarity: is a member of cloudsqlsuperuser") {
			t.Fatalf("problems before sync = %q", problems)
		}
		if _, err := migrations.Deploy(ctx, conn); err != nil {
			t.Fatalf("deploy: %v", err)
		}
		if problems, err = migrations.RoleProblems(ctx, conn); err != nil || len(problems) != 0 {
			t.Fatalf("problems after deploy = %q, %v", problems, err)
		}
	})

	t.Run("deploy is idempotent", func(t *testing.T) {
		res, err := migrations.Deploy(ctx, conn)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res {
			if r.Changed {
				t.Errorf("%s changed on a re-run", r.Migration.File())
			}
		}
	})
}

// TestRegress_G_OPS_02_ForcedRowLevelSecurity pins migration 0018's RLS
// (spec 10.7): FORCE subjects the table owner, child tables without a
// tenant_id follow their parent, partitions queried by name are covered
// (including ones the partition helper adds later), and the role-scoped
// cross-tenant policies exist only for their roles.
func TestRegress_G_OPS_02_ForcedRowLevelSecurity(t *testing.T) {
	cfg := freshDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn := connect(t, cfg)
	if _, err := migrations.Deploy(ctx, conn); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	roleFixtures(t, ctx, conn)

	t.Run("every tenant-scoped table is forced", func(t *testing.T) {
		rows, err := conn.Query(ctx, `SELECT c.oid::regclass::text FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'tenant_id'
			WHERE n.nspname IN ('identity','scope','engine','graph','notifier') AND c.relkind IN ('r','p')
			  AND NOT (c.relrowsecurity AND c.relforcerowsecurity
			           AND EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid = c.oid AND p.polname = 'tenant_isolation'))`)
		if err != nil {
			t.Fatal(err)
		}
		bad, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		if len(bad) > 0 {
			t.Errorf("tenant tables without forced RLS: %v", bad)
		}
	})

	owner := fmt.Sprintf("ff_owner_%d", testkit.Seed(t)%1_000_000)
	reader := fmt.Sprintf("ff_reader_%d", testkit.Seed(t)%1_000_000)
	for _, q := range []string{
		`CREATE ROLE ` + owner, `CREATE ROLE ` + reader,
		`GRANT USAGE ON SCHEMA scope, engine TO ` + owner + `, ` + reader,
		`GRANT SELECT, INSERT ON ALL TABLES IN SCHEMA engine TO ` + reader,
		`GRANT SELECT ON ALL TABLES IN SCHEMA scope TO ` + reader,
		`ALTER TABLE scope.programs OWNER TO ` + owner,
		`ALTER TABLE engine.runs OWNER TO ` + owner,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = conn.Exec(bg, `ALTER TABLE scope.programs OWNER TO CURRENT_USER`)
		_, _ = conn.Exec(bg, `ALTER TABLE engine.runs OWNER TO CURRENT_USER`)
		for _, r := range []string{owner, reader} {
			_, _ = conn.Exec(bg, `DROP OWNED BY `+r)
			_, _ = conn.Exec(bg, `DROP ROLE IF EXISTS `+r)
		}
	})

	t.Run("table owner is subject to the policy", func(t *testing.T) {
		asRole(t, ctx, conn, owner, tenantA, func(tx pgx.Tx) {
			if n := count(t, ctx, tx, `SELECT count(*) FROM scope.programs`); n != 1 {
				t.Errorf("owner with tenant A sees %d programs, want 1", n)
			}
			if n := count(t, ctx, tx, `SELECT count(*) FROM engine.runs WHERE tenant_id = '`+tenantB+`'`); n != 0 {
				t.Errorf("owner with tenant A sees %d runs of tenant B", n)
			}
		})
		asRole(t, ctx, conn, owner, "", func(tx pgx.Tx) {
			if n := count(t, ctx, tx, `SELECT count(*) FROM scope.programs`); n != 0 {
				t.Errorf("owner without a tenant sees %d programs, want 0 (fail closed)", n)
			}
		})
	})

	t.Run("child tables follow their parent", func(t *testing.T) {
		asRole(t, ctx, conn, reader, tenantA, func(tx pgx.Tx) {
			if n := count(t, ctx, tx, `SELECT count(*) FROM engine.run_events`); n != 1 {
				t.Errorf("tenant A sees %d run events, want 1", n)
			}
			if n := count(t, ctx, tx, `SELECT count(*) FROM engine.workflow_versions`); n != 1 {
				t.Errorf("tenant A sees %d workflow versions, want 1", n)
			}
			err := probe(ctx, tx, `INSERT INTO engine.run_events (run_id, seq, type, actor, payload)
				VALUES ('dddddddd-0000-7000-8000-000000000022', 9, 'log', 'engine', '{}')`)
			var pe *pgconn.PgError
			if !errors.As(err, &pe) || pe.Code != "42501" {
				t.Errorf("insert into tenant B's run: %v, want an RLS violation", err)
			}
		})
	})

	t.Run("partitions queried by name", func(t *testing.T) {
		if _, err := conn.Exec(ctx, `SELECT scope.ensure_decision_partition('2099-01-01T00:00:00Z')`); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, `GRANT SELECT ON ALL TABLES IN SCHEMA scope TO `+reader); err != nil {
			t.Fatal(err)
		}
		asRole(t, ctx, conn, reader, tenantA, func(tx pgx.Tx) {
			if n := count(t, ctx, tx, `SELECT count(*) FROM scope.scope_decisions_y2099m01`); n != 1 {
				t.Errorf("tenant A sees %d rows of the new partition, want 1", n)
			}
			if n := count(t, ctx, tx, `SELECT count(*) FROM scope.scope_decisions`); n != 1 {
				t.Errorf("tenant A sees %d decisions, want 1", n)
			}
		})
	})

	t.Run("cross-tenant policies are role-scoped", func(t *testing.T) {
		asRole(t, ctx, conn, "ff_engine", "", func(tx pgx.Tx) {
			// The scheduler's ready scan (eventstore/pgstore ListReady)
			// spans tenants.
			if n := count(t, ctx, tx, `SELECT count(*) FROM engine.runs WHERE status IN ('pending','running','waiting')`); n != 2 {
				t.Errorf("ff_engine sees %d ready runs, want both tenants' 2", n)
			}
		})
		asRole(t, ctx, conn, "ff_graph", "", func(tx pgx.Tx) {
			if n := count(t, ctx, tx, `SELECT count(*) FROM graph.entities`); n != 0 {
				t.Errorf("ff_graph without a tenant sees %d entities", n)
			}
		})
		var n int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_policies WHERE policyname LIKE 'svc_%'
			AND NOT (roles = ARRAY[substr(policyname, 5)]::name[])`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%d svc_ policies apply to roles other than their own", n)
		}
	})
}
