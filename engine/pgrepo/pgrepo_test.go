package pgrepo_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/engine/enginetest"
	"github.com/genuinebnt/flowforge/engine/pgrepo"
	"github.com/genuinebnt/flowforge/eventstore"
	"github.com/genuinebnt/flowforge/eventstore/pgstore"
	"github.com/genuinebnt/flowforge/migrations"
	"github.com/genuinebnt/flowforge/testkit"
)

// The integration tests run against FF_TEST_PG_DSN in a private database
// with the migrations and pgrepo.SchemaSQL applied.

var testDSN string

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	dsn := os.Getenv("FF_TEST_PG_DSN")
	if dsn == "" {
		return m.Run()
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgrepo tests: connect: %v\n", err)
		return 1
	}
	defer func() { _ = admin.Close(ctx) }()
	name := fmt.Sprintf("ff_pgrepo_test_%d", os.Getpid())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		fmt.Fprintf(os.Stderr, "pgrepo tests: %v\n", err)
		return 1
	}
	defer func() { _, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") }()
	testDSN = withDatabase(dsn, name)
	conn, err := pgx.Connect(ctx, testDSN)
	if err == nil {
		err = migrations.Apply(ctx, conn)
		if err == nil {
			_, err = conn.Exec(ctx, pgrepo.SchemaSQL)
		}
		_ = conn.Close(ctx)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgrepo tests: setup: %v\n", err)
		return 1
	}
	return m.Run()
}

func withDatabase(dsn, db string) string {
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		u.Path = "/" + db
		return u.String()
	}
	return dsn + " dbname=" + db
}

// toolsAdapter gives pgrepo.Tools the harness's Register signature.
type toolsAdapter struct {
	*pgrepo.Tools
	t testing.TB
}

func (a toolsAdapter) Register(s engine.ToolSpec) engine.ToolSpec {
	got, err := a.Tools.Register(context.Background(), s)
	if err != nil {
		a.t.Fatal(err)
	}
	return got
}

type pgEnv struct {
	pool  *pgxpool.Pool
	store *pgstore.Store
}

// pgHarness empties the engine tables and returns a harness on pgstore and
// pgrepo.
func pgHarness(t *testing.T, start time.Time) (*enginetest.Harness, pgEnv) {
	t.Helper()
	if testDSN == "" {
		t.Skip("FF_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE engine.run_requests, engine.run_ext, engine.approval_ext, engine.workflow_version_ext,
  engine.runs, engine.run_events, engine.snapshots, engine.effects, engine.timers, engine.approvals, engine.outbox,
  engine.workflow_versions, engine.workflows, engine.tools CASCADE`); err != nil {
		t.Fatal(err)
	}
	store, err := pgstore.Open(ctx, pgstore.Config{DSN: testDSN, MaxConns: 16, PollInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	h := enginetest.New(t, func(h *enginetest.Harness) {
		h.Clock = testkit.NewFakeClock(start)
		h.Store = store
		h.Repo = pgrepo.New(pool)
		h.Tools = toolsAdapter{Tools: pgrepo.NewTools(pool), t: t}
		h.Publisher = pgrepo.NewOutbox(pool)
	})
	return h, pgEnv{pool: pool, store: store}
}

func TestEng001_HappyPathPostgres(t *testing.T) {
	h, env := pgHarness(t, enginetest.Start)
	h.Exec.Handle("subfinder", enginetest.Output([]string{"api.acme.com", "evil.org", "www.acme.com"}))
	h.Exec.Handle("httpx", func(ctx context.Context, call engine.ToolCall) (engine.ToolResult, error) {
		host := string(call.Input[1 : len(call.Input)-1])
		out := fmt.Sprintf(`{"url":"https://%s/","status":200}`, host)
		obs := fmt.Sprintf(`{"entities":[{"ref":"u1","asset":{"type":"URL","url":"https://%s/"}}],"edges":[]}`, host)
		return engine.ToolResult{Output: []byte(out), Observations: []json.RawMessage{json.RawMessage(obs)}}, nil
	})
	c := enginetest.Analyst(enginetest.TenantA)
	h.Register(c, "recon", enginetest.Recon, enginetest.ProgramA)
	r := h.Submit(c, "recon", enginetest.ProgramA, map[string]any{"roots": []string{"acme.com"}}, "key-1")
	if again := h.Submit(c, "recon", enginetest.ProgramA, map[string]any{"roots": []string{"acme.com"}}, "key-1"); again.ID != r.ID {
		t.Fatal("idempotent submit created a second run")
	}
	if st := h.RunToEnd(h.Engine.NewWorker("w1"), r.ID); st != engine.StatusSucceeded {
		t.Fatalf("status %s", st)
	}
	got := enginetest.NormalJSON(enginetest.Normalize(h.Events(r.ID)))
	want := string(testkit.ReadFile(t, "engine", "eng_001_recon.golden.json"))
	if got != want {
		t.Fatalf("Postgres log differs from the golden log:\n%s", got)
	}
	run, err := h.Engine.GetRun(h.Ctx(), c, r.ID)
	if err != nil || string(run.Output) != `{"alerts": 1, "new_assets": 2}` && string(run.Output) != `{"alerts":1,"new_assets":2}` {
		t.Fatalf("run %+v %v", run.Output, err)
	}
	runs, _, err := h.Engine.ListRuns(h.Ctx(), c, engine.RunFilter{Status: engine.StatusSucceeded}, engine.Page{})
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns %d %v", len(runs), err)
	}
	if runs, _, _ := h.Engine.ListRuns(h.Ctx(), enginetest.Analyst(enginetest.TenantB), engine.RunFilter{}, engine.Page{}); len(runs) != 0 {
		t.Fatal("another tenant sees the run")
	}
	var outbox, effects int
	_ = env.pool.QueryRow(h.Ctx(), `SELECT count(*) FROM engine.outbox WHERE topic = 'run.events'`).Scan(&outbox)
	_ = env.pool.QueryRow(h.Ctx(), `SELECT count(*) FROM engine.effects WHERE run_id = $1 AND state = 'completed'`, r.ID).Scan(&effects)
	if uint64(outbox) != run.LastSeq || effects != 9 {
		t.Fatalf("outbox %d (log %d), completed effects %d", outbox, run.LastSeq, effects)
	}
}

func TestEng004_ApprovalsPostgres(t *testing.T) {
	h, _ := pgHarness(t, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	h.Exec.Handle("post", enginetest.Output("id-0"))
	h.Exec.Handle("block", enginetest.Echo())
	c := enginetest.Analyst(enginetest.TenantA)
	h.Register(c, "saga", enginetest.Saga(enginetest.SagaOptions{Approval: true, Steps: 1}), core.Nil)
	r := h.Submit(c, "saga", core.Nil, map[string]any{"amount": 5}, "k")
	w := h.Engine.NewWorker("w")
	if _, err := w.RunOnce(h.Ctx()); err != nil { // up to the approval
		t.Fatal(err)
	}
	approver := enginetest.Approver(enginetest.TenantA)
	as, _, err := h.Engine.ListApprovals(h.Ctx(), approver, engine.ApprovalFilter{Status: engine.ApprovalPending}, engine.Page{})
	if err != nil || len(as) != 1 {
		t.Fatalf("approvals %v %v", as, err)
	}
	self := c
	self.Roles = append(self.Roles, "finance_ap")
	if _, err := h.Engine.DecideApproval(h.Ctx(), self, as[0].ID, engine.DecisionApprove, ""); err == nil {
		t.Fatal("self approval accepted")
	}
	if _, err := h.Engine.DecideApproval(h.Ctx(), approver, as[0].ID, engine.DecisionApprove, "ok"); err != nil {
		t.Fatal(err)
	}
	if a, err := h.Engine.DecideApproval(h.Ctx(), approver, as[0].ID, engine.DecisionApprove, "ok"); err != nil || a.DecidedBy != approver.Actor {
		t.Fatalf("idempotent repeat %+v %v", a, err)
	}
	if st, _ := drive(t, h, w, r.ID, false); st != engine.StatusSucceeded {
		t.Fatalf("status %s", st)
	}
}

// drive advances the run with w until it is terminal, moving the fake
// clock to each wake time (the fake clock is in the database's past, so
// every parked run is due as far as the database is concerned).
func drive(t *testing.T, h *enginetest.Harness, w *engine.Worker, run core.UUID, stopOnErr bool) (engine.RunStatus, error) {
	t.Helper()
	for i := 0; i < 500; i++ {
		ok, err := w.RunOnce(h.Ctx())
		if err != nil {
			if stopOnErr {
				return "", err
			}
			t.Fatal(err)
		}
		info, err := h.Store.GetRun(h.Ctx(), run)
		if err != nil {
			if stopOnErr {
				return "", err
			}
			t.Fatal(err)
		}
		if engine.Terminal(info.Status) {
			return info.Status, nil
		}
		if !ok || info.NextWakeAt.After(h.Clock.Now()) {
			h.Clock.Advance(max(info.NextWakeAt.Sub(h.Clock.Now()), time.Millisecond))
		}
	}
	t.Fatal("run never finishes")
	return "", nil
}

func multiRun(t *testing.T, crashAt int) string {
	h, env := pgHarness(t, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	h.Exec.Handle("echo", enginetest.Echo())
	h.Exec.Handle("httpx", enginetest.Output(map[string]any{"url": "https://api.acme.com/", "status": 200}))
	c := enginetest.Analyst(enginetest.TenantA)
	h.Register(c, "multi", enginetest.Multi, enginetest.ProgramA)
	r := h.Submit(c, "multi", enginetest.ProgramA, map[string]any{"roots": []string{"api.acme.com"}}, "k")
	if crashAt >= 0 {
		cs := enginetest.NewCrashStore(h.Store, crashAt)
		_, _ = drive(t, h, h.NewEngine(cs).NewWorker("doomed"), r.ID, true)
		// Let the dead worker's lease expire on the database clock.
		if _, err := env.pool.Exec(h.Ctx(), `UPDATE engine.runs SET lease_expires_at = now() - interval '1 second'
WHERE lease_owner IS NOT NULL`); err != nil {
			t.Fatal(err)
		}
	}
	if st, _ := drive(t, h, h.Engine.NewWorker(eventstore.WorkerID("survivor")), r.ID, false); st != engine.StatusSucceeded {
		t.Fatalf("crash at %d: status %s", crashAt, st)
	}
	return enginetest.NormalJSON(enginetest.Normalize(h.Events(r.ID)))
}

func TestEng008_CrashRecoveryPostgres(t *testing.T) {
	if testDSN == "" {
		t.Skip("FF_TEST_PG_DSN not set")
	}
	want := multiRun(t, -1)
	for _, k := range []int{0, 2, 4, 7, 10, 13, 16, 19, 22, 25, 28} {
		if got := multiRun(t, k); got != want {
			t.Fatalf("crash after %d appends: log differs\n got: %s\nwant: %s", k, got, want)
		}
	}
}
