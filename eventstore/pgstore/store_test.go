package pgstore_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/eventstore"
	"github.com/genuinebnt/flowforge/eventstore/pgstore"
	"github.com/genuinebnt/flowforge/eventstore/storetest"
	"github.com/genuinebnt/flowforge/migrations"
)

// The integration tests run against FF_TEST_PG_DSN. TestMain creates a
// private database on that server, applies the migrations, inserts the
// workflow fixtures engine.runs needs (foreign keys) and drops the
// database at the end.

var (
	testDSN       string // DSN of the private test database ("" = skip)
	fixtureWFV    = core.MustParseUUID("0190a1b2-0000-7000-8000-00000000f1f0")
	fixtureWF     = core.MustParseUUID("0190a1b2-0000-7000-8000-00000000f1f1")
	fixtureTenant = core.MustParseUUID("00000000-0000-7000-8000-0000000000a1")
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	dsn := os.Getenv("FF_TEST_PG_DSN")
	if dsn == "" {
		return m.Run()
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgstore tests: connect: %v\n", err)
		return 1
	}
	defer func() { _ = admin.Close(ctx) }()
	name := fmt.Sprintf("ff_pgstore_test_%d", os.Getpid())
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name); err != nil {
		fmt.Fprintf(os.Stderr, "pgstore tests: %v\n", err)
		return 1
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		fmt.Fprintf(os.Stderr, "pgstore tests: %v\n", err)
		return 1
	}
	defer func() { _, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") }()

	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgstore tests: %v\n", err)
		return 1
	}
	cfg.Database = name
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgstore tests: %v\n", err)
		return 1
	}
	err = migrations.Apply(ctx, conn)
	if err == nil {
		_, err = conn.Exec(ctx, `INSERT INTO engine.workflows (id, tenant_id, name) VALUES ($1, $2, 'storetest')`,
			fixtureWF.String(), fixtureTenant.String())
	}
	if err == nil {
		_, err = conn.Exec(ctx, `INSERT INTO engine.workflow_versions (id, workflow_id, version, source, source_sha256,
  module, module_sha256, effects, inferred_class, created_by)
VALUES ($1, $2, 1, 'workflow storetest {}', '\x00', '\x00', '\x00', '{}', 'none', $3)`,
			fixtureWFV.String(), fixtureWF.String(), fixtureTenant.String())
	}
	_ = conn.Close(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgstore tests: setup: %v\n", err)
		return 1
	}
	testDSN = withDatabase(dsn, name)
	return m.Run()
}

// withDatabase points a DSN (URL or keyword/value form) at database db.
func withDatabase(dsn, db string) string {
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		u.Path = "/" + db
		return u.String()
	}
	return dsn + " dbname=" + db
}

// pgHarness empties the engine tables and opens a Store on the test
// database. Elapse shifts every stored lease and wake timestamp back by d,
// which is equivalent to advancing the database clock for every
// comparison the store makes against now().
func pgHarness(t testing.TB) *storetest.Harness {
	t.Helper()
	if testDSN == "" {
		t.Skip("FF_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(ctx) })
	if _, err := admin.Exec(ctx, `TRUNCATE engine.runs, engine.run_events, engine.snapshots, engine.effects,
  engine.timers, engine.approvals CASCADE`); err != nil {
		t.Fatal(err)
	}
	open := func(t testing.TB) eventstore.Store {
		s, err := pgstore.Open(ctx, pgstore.Config{DSN: testDSN, MaxConns: 64, PollInterval: 50 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		return s
	}
	var mu sync.Mutex // admin is one connection; Elapse may be called concurrently
	cases := 200
	if os.Getenv("FF_PROPTEST_CASES") != "" {
		cases = 0 // testkit.Cases()
	}
	return &storetest.Harness{
		Store: open(t),
		Now: func(ctx context.Context) (time.Time, error) {
			mu.Lock()
			defer mu.Unlock()
			var now time.Time
			err := admin.QueryRow(ctx, `SELECT now()`).Scan(&now)
			return now, err
		},
		Elapse: func(ctx context.Context, d time.Duration) error {
			mu.Lock()
			defer mu.Unlock()
			_, err := admin.Exec(ctx, `UPDATE engine.runs SET
  next_wake_at      = next_wake_at      - $1::bigint * interval '1 microsecond',
  lease_expires_at  = lease_expires_at  - $1::bigint * interval '1 microsecond',
  lease_acquired_at = lease_acquired_at - $1::bigint * interval '1 microsecond',
  woken_at          = woken_at          - $1::bigint * interval '1 microsecond'`, d.Microseconds())
			return err
		},
		WorkflowVersion: fixtureWFV,
		Restart:         open,
		PropCases:       cases,
	}
}

func newPG(t *testing.T) *storetest.Harness { return pgHarness(t) }

// TestES007_AllImplementations runs the whole conformance suite against
// Postgres (eventstore runs it against MemStore).
func TestES007_AllImplementations(t *testing.T) { storetest.Run(t, newPG) }

// TestPGStore_NotifyWake checks that ready runs are announced on the
// engine_wake channel (spec 9.4) so workers need not poll quickly.
func TestPGStore_NotifyWake(t *testing.T) {
	h := pgHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lc, err := pgx.Connect(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lc.Close(context.Background()) }()
	if _, err := lc.Exec(ctx, `LISTEN engine_wake`); err != nil {
		t.Fatal(err)
	}
	run := core.MustParseUUID("0190a1b2-0000-7000-8000-0000000000e1")
	spec := eventstore.RunSpec{ID: run, Tenant: fixtureTenant, WorkflowVersion: fixtureWFV,
		CreatedBy: "user:t", IdempotencyKey: "wake-test", WakeAt: time.Now().Add(time.Hour)}
	if err := h.Store.CreateRun(ctx, spec, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.Store.Wake(ctx, run); err != nil {
		t.Fatal(err)
	}
	n, err := lc.WaitForNotification(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n.Channel != "engine_wake" || n.Payload != run.String() {
		t.Fatalf("notification %s %q, want engine_wake %q", n.Channel, n.Payload, run)
	}
}

// TestPGStore_UnknownWorkflowVersion pins the mapping of a foreign key
// violation to ErrInvalid.
func TestPGStore_UnknownWorkflowVersion(t *testing.T) {
	h := pgHarness(t)
	spec := eventstore.RunSpec{ID: core.MustParseUUID("0190a1b2-0000-7000-8000-0000000000e2"), Tenant: fixtureTenant,
		WorkflowVersion: core.MustParseUUID("0190a1b2-0000-7000-8000-0000000000e3"), CreatedBy: "user:t", IdempotencyKey: "k"}
	if err := h.Store.CreateRun(context.Background(), spec, nil); !errors.Is(err, eventstore.ErrInvalid) {
		t.Fatalf("CreateRun with unknown workflow version: %v, want ErrInvalid", err)
	}
}
