package maintenance_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/migrations"
	"github.com/genuinebnt/flowforge/services/internal/blob"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
	"github.com/genuinebnt/flowforge/services/maintenance"
)

func pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), servicetest.FreshDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	if err := migrations.Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

func exec(t *testing.T, p *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := p.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func count(t *testing.T, p *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := p.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

const (
	tenant = "0190a1b2-0000-7000-8000-00000000a001"
	wf     = "0190a1b2-0000-7000-8000-0000000000f1"
	wfv    = "0190a1b2-0000-7000-8000-0000000000f2"
	oldRun = "0190a1b2-0000-7000-8000-0000000000b1"
	newRun = "0190a1b2-0000-7000-8000-0000000000b2"
	live   = "0190a1b2-0000-7000-8000-0000000000b3"
)

func fixture(t *testing.T, p *pgxpool.Pool) {
	exec(t, p, `INSERT INTO engine.workflows (id, tenant_id, name) VALUES ($1, $2, 'wf')`, wf, tenant)
	exec(t, p, `INSERT INTO engine.workflow_versions (id, workflow_id, version, source, source_sha256, module, module_sha256, effects,
  inferred_class, created_by) VALUES ($1, $2, 1, '', '\x00', '\x00', '\x00', '{}', 'none', $3)`, wfv, wf, tenant)
	for _, r := range []struct {
		id, status string
		finished   string
	}{{oldRun, "succeeded", "now() - interval '401 days'"}, {newRun, "failed", "now() - interval '10 days'"}, {live, "running", "NULL"}} {
		exec(t, p, `INSERT INTO engine.runs (id, tenant_id, workflow_version_id, status, created_by, idempotency_key, request_hash, finished_at, last_seq)
VALUES ($1::uuid, $2, $3, $4, 'user:x', $1::text, '\x00', `+r.finished+`, 3)`, r.id, tenant, wfv, r.status)
		for seq := 1; seq <= 3; seq++ {
			exec(t, p, `INSERT INTO engine.run_events (run_id, seq, type, actor, payload) VALUES ($1, $2, 'step', 'engine', '{"n": 1}')`, r.id, seq)
		}
		exec(t, p, `INSERT INTO engine.snapshots (run_id, at_seq, module_sha256, state) VALUES ($1, 2, '\x00', '\x00')`, r.id)
		exec(t, p, `INSERT INTO engine.run_ext (run_id, status, published_seq) VALUES ($1, $2, 3)`, r.id, r.status)
	}
	// Old and recent rows for the row-retention jobs.
	exec(t, p, `INSERT INTO identity.auth_audit (at, event) VALUES (now() - interval '13 months', 'login_succeeded'), (now(), 'login_succeeded')`)
	exec(t, p, `INSERT INTO graph.observations (id, tenant_id, program_id, run_id, effect_path, tool_name, tool_version, observed_at, confidence,
  entity_count, edge_count, ingested_at) VALUES
  (gen_random_uuid(), $1, $1, $1, '1', 't', '1', now(), 1, 0, 0, now() - interval '91 days'),
  (gen_random_uuid(), $1, $1, $1, '1', 't', '1', now(), 1, 0, 0, now())`, tenant)
	exec(t, p, `INSERT INTO engine.outbox (event_id, topic, payload, published_at) VALUES
  (gen_random_uuid(), 'run.events', '{}', now() - interval '8 days'), (gen_random_uuid(), 'run.events', '{}', NULL)`)
	exec(t, p, `INSERT INTO notifier.processed_events (consumer, event_id, processed_at) VALUES ('notifier', gen_random_uuid(), now() - interval '31 days')`)
	// A scope_decisions partition 15 months back, with a row.
	exec(t, p, `SELECT scope.ensure_decision_partition(now() - interval '15 months')`)
	exec(t, p, `INSERT INTO scope.scope_decisions (id, tenant_id, program_id, scope_version, target, class, decision, reason, decided_at)
VALUES (gen_random_uuid(), $1, $1, 1, 'a.test', 'passive', 'allow', 'r', now() - interval '15 months')`, tenant)
}

// TestRegress_GOPS03_RetentionAndArchival runs one pass against old and
// recent rows of every table spec 10.7 sets a retention for, then a second
// pass that must change nothing (idempotent).
func TestRegress_GOPS03_RetentionAndArchival(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	fixture(t, p)

	// run_events stays append-only outside the archival path.
	if _, err := p.Exec(ctx, `DELETE FROM engine.run_events WHERE run_id = $1`, oldRun); err == nil {
		t.Fatal("a plain DELETE on run_events succeeded")
	}

	blobs := graphstore.DirBlobs{Root: t.TempDir()}
	r := &maintenance.Runner{Pool: p, Blobs: blobs}
	rep, err := r.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep["run_archive"] != 1 || rep["auth_audit"] != 1 || rep["observations"] != 1 || rep["outbox"] != 1 || rep["processed_events"] != 1 {
		t.Fatalf("report %v", rep)
	}
	// The old run is gone entirely; its archive has its three events.
	if n := count(t, p, `SELECT count(*) FROM engine.runs WHERE id = $1`, oldRun); n != 0 {
		t.Fatal("archived run still present")
	}
	if n := count(t, p, `SELECT count(*) FROM engine.run_events WHERE run_id = $1`, newRun); n != 3 {
		t.Fatalf("recent run lost events: %d", n)
	}
	var uri string
	var events int
	if err := p.QueryRow(ctx, `SELECT uri, events FROM engine.run_archives WHERE run_id = $1`, oldRun).Scan(&uri, &events); err != nil || events != 3 {
		t.Fatalf("archive row %q %d %v", uri, events, err)
	}
	frame, err := blobs.Get(ctx, uri)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(uri, maintenance.ArchiveName(core.MustParseUUID(tenant), core.MustParseUUID(oldRun))) {
		t.Fatalf("archive uri %s", uri)
	}
	plain, err := blob.ZstdDecodeStored(frame)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(bytes.NewReader(plain))
	seq := int64(0)
	for sc.Scan() {
		var l maintenance.ArchiveLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil || l.Seq != seq+1 || l.Type != "step" {
			t.Fatalf("archive line %s: %v", sc.Text(), err)
		}
		seq = l.Seq
	}
	if seq != 3 {
		t.Fatalf("archive has %d lines", seq)
	}
	// G-ENG-04: finished runs keep no snapshots; the live run keeps its own.
	if n := count(t, p, `SELECT count(*) FROM engine.snapshots`); n != 1 {
		t.Fatalf("%d snapshots left, want only the live run's", n)
	}
	if n := count(t, p, `SELECT count(*) FROM engine.snapshots WHERE run_id = $1`, live); n != 1 {
		t.Fatal("live run's snapshot was deleted")
	}
	// Decision partitions: the 15-month-old one is dropped, the next two
	// months exist.
	if n := count(t, p, `SELECT count(*) FROM scope.scope_decisions WHERE decided_at < now() - interval '14 months'`); n != 0 {
		t.Fatal("old decisions kept")
	}
	if n := count(t, p, `SELECT count(*) FROM pg_class WHERE relname = 'scope_decisions_y' || to_char(now() + interval '2 months', 'YYYY') || 'm' || to_char(now() + interval '2 months', 'MM')`); n != 1 {
		t.Fatal("partition two months ahead missing")
	}

	rep, err = r.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for job, n := range rep {
		if job != "decision_partitions" && n != 0 {
			t.Fatalf("second pass changed %s (%d): not idempotent", job, n)
		}
	}
}

// TestRegress_GOPS03_NoArchiveWithoutDurableBlobs: with the in-memory blob
// store (or none) runs are never deleted, since their only copy would be
// in process memory.
func TestRegress_GOPS03_NoArchiveWithoutDurableBlobs(t *testing.T) {
	p := pool(t)
	fixture(t, p)
	for _, b := range []graphstore.BlobStore{nil, &graphstore.MemBlobs{}} {
		rep, err := (&maintenance.Runner{Pool: p, Blobs: b}).RunOnce(context.Background())
		if err != nil || rep["run_archive"] != 0 {
			t.Fatalf("archived %d (%v)", rep["run_archive"], err)
		}
	}
	if n := count(t, p, `SELECT count(*) FROM engine.run_events WHERE run_id = $1`, oldRun); n != 3 {
		t.Fatal("events deleted without an archive")
	}
}

// TestRegress_GOPS03_SerializedPasses: a pass while another holds the
// lock returns ErrBusy instead of running the jobs twice at once.
func TestRegress_GOPS03_SerializedPasses(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	conn, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(0x66666d61696e74)); err != nil {
		t.Fatal(err)
	}
	if _, err := (&maintenance.Runner{Pool: p}).RunOnce(ctx); !errors.Is(err, maintenance.ErrBusy) {
		t.Fatalf("concurrent pass: %v", err)
	}
	_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, int64(0x66666d61696e74))
}
