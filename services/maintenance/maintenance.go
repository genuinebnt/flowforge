// Package maintenance runs the spec 10.7 retention rules and the
// partition job (G-OPS-03, G-ENG-04) against the Postgres database:
//
//	job                  rule (spec 10.7 unless noted)
//	decision_partitions  create scope_decisions partitions for this and
//	                     the next two months (0011's function)
//	decision_retention   drop monthly partitions older than 13 months and
//	                     delete old rows of the default partition
//	run_archive          run_events of runs finished more than 400 days
//	                     ago: archived as zstd JSON Lines to the blob store,
//	                     then the whole run is deleted
//	snapshots            snapshots of finished runs (the engine deletes
//	                     them at finish; this sweeps what a failure left)
//	observations         graph.observations older than 90 days (the raw
//	                     blobs follow the bucket lifecycle rule)
//	auth_audit           identity.auth_audit older than 12 months
//	outbox               published outbox rows older than 7 days
//	                     (engine, scope, identity; 23.7 needs them only
//	                     until published)
//	processed_events     inbox rows older than 30 days (longer than any
//	                     redelivery: Pub/Sub retains 7 days, dead letters
//	                     14)
//	bus                  acked bus deliveries (24 h) and dead ones (14
//	                     days) of the Postgres bus (0016), when present
//	alert_state          ended notifier throttle windows (0017)
//
// Every job is idempotent and bounded per invocation (Limits), so a run
// can be cut off at any point and the next continues; a pass is
// serialized across processes by an advisory lock. The same Runner runs
// in process (the engine, flowforge dev), from `migrate maintain`, and
// from POST /internal/schedules:tick on GCP (one Cloud Scheduler job).
//
// Privileges: the runner needs DELETE on the tables above, ownership of
// the scope_decisions partitions (DROP TABLE; the migration role owns
// them) and EXECUTE on scope.ensure_decision_partition; with the 0018
// roles that is the migrator acting as ff_maintenance. The run_events
// delete passes the append-only trigger only inside the archival
// transaction (0017).
package maintenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/services/internal/blob"
	"github.com/genuinebnt/flowforge/services/internal/bus"
	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// lockKey serializes maintenance passes ("ffmaint" in ASCII).
const lockKey int64 = 0x66666d61696e74

// Retention is the spec 10.7 retention, overridable for tests.
type Retention struct {
	RunEvents       time.Duration // 400 days
	Decisions       int           // months, 13
	Observations    time.Duration // 90 days
	AuthAudit       time.Duration // 12 months (365 days)
	Outbox          time.Duration // 7 days after publication
	ProcessedEvents time.Duration // 30 days
}

// DefaultRetention returns the spec 10.7 values.
func DefaultRetention() Retention {
	return Retention{RunEvents: 400 * 24 * time.Hour, Decisions: 13, Observations: 90 * 24 * time.Hour,
		AuthAudit: 365 * 24 * time.Hour, Outbox: 7 * 24 * time.Hour, ProcessedEvents: 30 * 24 * time.Hour}
}

// Limits bound one pass.
type Limits struct {
	Rows       int // rows deleted per table (default 5000)
	Runs       int // runs archived (default 20)
	Partitions int // partitions dropped (default 3)
}

// Runner runs the jobs.
type Runner struct {
	Pool *pgxpool.Pool
	// Blobs receives run archives. Archival is skipped (and runs are kept)
	// when it is nil or in memory: deleting events whose only copy is in
	// process memory would lose them at the next restart.
	Blobs     graphstore.BlobStore
	Retention Retention
	Limits    Limits
	Logger    *slog.Logger
}

// Report counts what one pass did, per job.
type Report map[string]int64

func (r *Runner) defaults() {
	z := Retention{}
	if r.Retention == z {
		r.Retention = DefaultRetention()
	}
	if r.Limits.Rows <= 0 {
		r.Limits.Rows = 5000
	}
	if r.Limits.Runs <= 0 {
		r.Limits.Runs = 20
	}
	if r.Limits.Partitions <= 0 {
		r.Limits.Partitions = 3
	}
	if r.Logger == nil {
		r.Logger = platform.DiscardLogger()
	}
}

// ErrBusy is returned when another process holds the maintenance lock.
var ErrBusy = errors.New("maintenance: another pass is running")

// RunOnce runs every job once. Job errors are collected, not fatal: one
// failing job never blocks the others.
func (r *Runner) RunOnce(ctx context.Context) (Report, error) {
	r.defaults()
	conn, err := r.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockKey).Scan(&got); err != nil {
		return nil, err
	}
	if !got {
		return nil, ErrBusy
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, lockKey) }()

	rep := Report{}
	var errs []error
	for _, j := range []struct {
		name string
		run  func(context.Context) (int64, error)
	}{
		{"decision_partitions", r.ensurePartitions},
		{"decision_retention", r.decisionRetention},
		{"run_archive", r.archiveRuns},
		{"snapshots", r.finishedSnapshots},
		{"observations", r.deleteOlder("graph.observations", "ingested_at", r.Retention.Observations, "")},
		{"auth_audit", r.deleteOlder("identity.auth_audit", "at", r.Retention.AuthAudit, "")},
		{"outbox", r.outbox},
		{"processed_events", r.processed},
		{"bus", r.bus},
		{"alert_state", r.deleteOlder("notifier.throttle_windows", "until", 0, "")},
	} {
		n, err := j.run(ctx)
		rep[j.name] = n
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", j.name, err))
			r.Logger.WarnContext(ctx, "maintenance.job_failed", "job", j.name, "error", err.Error())
		}
	}
	r.Logger.InfoContext(ctx, "maintenance.pass", "report", fmt.Sprint(map[string]int64(rep)))
	return rep, errors.Join(errs...)
}

// Loop runs a pass every interval until ctx is done. The first pass waits
// min(interval, 5 min): a process starting up (migrations, schema
// bootstrap, the first requests) should not race the partition DDL, and a
// short-lived process has `migrate maintain` and the tick instead.
func (r *Runner) Loop(ctx context.Context, interval time.Duration) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(min(interval, 5*time.Minute)):
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := r.RunOnce(ctx); err != nil && !errors.Is(err, ErrBusy) && ctx.Err() == nil {
			r.Logger.WarnContext(ctx, "maintenance.pass_failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func secs(d time.Duration) float64 { return d.Seconds() }

// tableExists lets a job skip a schema that is not migrated here (the
// bus schema only exists from 0016 on).
func (r *Runner) tableExists(ctx context.Context, name string) (bool, error) {
	var ok bool
	err := r.Pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&ok)
	return ok, err
}

// deleteOlder deletes up to Limits.Rows rows of table whose column is
// older than keep (extra is an additional AND condition).
func (r *Runner) deleteOlder(table, column string, keep time.Duration, extra string) func(context.Context) (int64, error) {
	return func(ctx context.Context) (int64, error) {
		if ok, err := r.tableExists(ctx, table); err != nil || !ok {
			return 0, err
		}
		cond := column + ` < now() - make_interval(secs => $1)`
		if extra != "" {
			cond += " AND " + extra
		}
		tag, err := r.Pool.Exec(ctx, `DELETE FROM `+table+` WHERE ctid IN (SELECT ctid FROM `+table+` WHERE `+cond+` LIMIT $2)`,
			secs(keep), r.Limits.Rows)
		return tag.RowsAffected(), err
	}
}

func (r *Runner) ensurePartitions(ctx context.Context) (int64, error) {
	var n int64
	for k := range 3 {
		if _, err := r.Pool.Exec(ctx, `SELECT scope.ensure_decision_partition(now() + make_interval(months => $1))`, k); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

var partitionName = regexp.MustCompile(`^scope_decisions_y([0-9]{4})m([0-9]{2})$`)

// decisionRetention drops monthly partitions whose month ended more than
// Retention.Decisions months ago (whole months, so a partition is dropped
// only when all its rows are past retention), then trims the default
// partition, which holds rows no partition covered.
func (r *Runner) decisionRetention(ctx context.Context) (int64, error) {
	var cutoff time.Time
	if err := r.Pool.QueryRow(ctx, `SELECT date_trunc('month', (now() AT TIME ZONE 'UTC') - make_interval(months => $1)) AT TIME ZONE 'UTC'`,
		r.Retention.Decisions).Scan(&cutoff); err != nil {
		return 0, err
	}
	rows, err := r.Pool.Query(ctx, `SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
JOIN pg_namespace n ON n.oid = c.relnamespace WHERE i.inhparent = 'scope.scope_decisions'::regclass AND n.nspname = 'scope' ORDER BY c.relname`)
	if err != nil {
		return 0, err
	}
	var old []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return 0, err
		}
		m := partitionName.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		end := time.Date(y, time.Month(mo)+1, 1, 0, 0, 0, 0, time.UTC)
		if !end.After(cutoff) {
			old = append(old, name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var n int64
	for _, name := range old[:min(len(old), r.Limits.Partitions)] {
		if _, err := r.Pool.Exec(ctx, `DROP TABLE IF EXISTS scope.`+pgx.Identifier{name}.Sanitize()); err != nil {
			return n, err
		}
		r.Logger.InfoContext(ctx, "maintenance.partition_dropped", "partition", "scope."+name)
		n++
	}
	tag, err := r.Pool.Exec(ctx, `DELETE FROM scope.scope_decisions_default WHERE ctid IN (
  SELECT ctid FROM scope.scope_decisions_default WHERE decided_at < $1 LIMIT $2)`, cutoff, r.Limits.Rows)
	return n + tag.RowsAffected(), err
}

const terminal = `('succeeded','failed','cancelled')`

func (r *Runner) finishedSnapshots(ctx context.Context) (int64, error) {
	tag, err := r.Pool.Exec(ctx, `DELETE FROM engine.snapshots WHERE ctid IN (
  SELECT s.ctid FROM engine.snapshots s JOIN engine.runs x ON x.id = s.run_id WHERE x.status IN `+terminal+` LIMIT $1)`, r.Limits.Rows)
	return tag.RowsAffected(), err
}

func (r *Runner) outbox(ctx context.Context) (int64, error) {
	var total int64
	for _, t := range []string{"engine.outbox", "scope.outbox", "identity.outbox"} {
		n, err := r.deleteOlder(t, "published_at", r.Retention.Outbox, "published_at IS NOT NULL")(ctx)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (r *Runner) processed(ctx context.Context) (int64, error) {
	var total int64
	for _, t := range []string{"engine.processed_events", "notifier.processed_events"} {
		n, err := r.deleteOlder(t, "processed_at", r.Retention.ProcessedEvents, "")(ctx)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (r *Runner) bus(ctx context.Context) (int64, error) {
	if ok, err := r.tableExists(ctx, "bus.deliveries"); err != nil || !ok {
		return 0, err
	}
	return (&bus.PgBus{Pool: r.Pool}).Cleanup(ctx, bus.Retention{Limit: r.Limits.Rows})
}

// ArchiveName is the blob name of a run's archive.
func ArchiveName(tenant, run core.UUID) string {
	return "archives/run_events/" + core.PublicID(core.PrefixTenant, tenant) + "/" + core.PublicID(core.PrefixRun, run) + ".jsonl.zst"
}

// ArchiveLine is one JSON Lines record of an archive: one run_events row.
type ArchiveLine struct {
	RunID   string          `json:"run_id"`
	Seq     int64           `json:"seq"`
	Type    string          `json:"type"`
	At      string          `json:"at"`
	Actor   string          `json:"actor"`
	Payload json.RawMessage `json:"payload"`
}

// archiveRuns archives and deletes up to Limits.Runs whole runs that
// finished before the retention cutoff. A run is eligible only when every
// event reached its outbox (run_ext.published_seq = last_seq) and no
// child run still references it.
func (r *Runner) archiveRuns(ctx context.Context) (int64, error) {
	if r.Blobs == nil {
		return 0, nil
	}
	if _, mem := r.Blobs.(*graphstore.MemBlobs); mem {
		return 0, nil
	}
	rows, err := r.Pool.Query(ctx, `SELECT x.id::text, x.tenant_id::text FROM engine.runs x
LEFT JOIN engine.run_ext e ON e.run_id = x.id
WHERE x.status IN `+terminal+` AND x.finished_at < now() - make_interval(secs => $1)
  AND COALESCE(e.published_seq, x.last_seq) >= x.last_seq
  AND NOT EXISTS (SELECT 1 FROM engine.runs c WHERE c.parent_run_id = x.id)
ORDER BY x.finished_at LIMIT $2`, secs(r.Retention.RunEvents), r.Limits.Runs)
	if err != nil {
		return 0, err
	}
	type cand struct{ run, tenant core.UUID }
	var cs []cand
	for rows.Next() {
		var id, tenant string
		if err := rows.Scan(&id, &tenant); err != nil {
			rows.Close()
			return 0, err
		}
		cs = append(cs, cand{core.MustParseUUID(id), core.MustParseUUID(tenant)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var n int64
	for _, c := range cs {
		if err := r.archiveRun(ctx, c.tenant, c.run); err != nil {
			return n, fmt.Errorf("run %s: %w", c.run, err)
		}
		n++
	}
	return n, nil
}

func (r *Runner) archiveRun(ctx context.Context, tenant, run core.UUID) error {
	rows, err := r.Pool.Query(ctx, `SELECT seq, type, at, actor, payload::text FROM engine.run_events WHERE run_id = $1 ORDER BY seq`, run.String())
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	var count int64
	enc := json.NewEncoder(&buf)
	for rows.Next() {
		var l ArchiveLine
		var at time.Time
		var payload string
		if err := rows.Scan(&l.Seq, &l.Type, &at, &l.Actor, &payload); err != nil {
			rows.Close()
			return err
		}
		l.RunID, l.At, l.Payload = core.PublicID(core.PrefixRun, run), core.FormatTime(at), json.RawMessage(payload)
		if err := enc.Encode(l); err != nil {
			rows.Close()
			return err
		}
		count++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	frame := blob.ZstdStored(buf.Bytes())
	// The archive is written (and overwritten on a retry) before anything
	// is deleted: a crash in between leaves the run in place and the next
	// pass writes the same object again.
	uri, err := r.Blobs.Put(ctx, ArchiveName(tenant, run), frame)
	if err != nil {
		return err
	}
	sum := sha256Sum(frame)
	return pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO engine.run_archives (run_id, tenant_id, uri, events, sha256) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (run_id) DO UPDATE SET uri = EXCLUDED.uri, events = EXCLUDED.events, sha256 = EXCLUDED.sha256, archived_at = now()`,
			run.String(), tenant.String(), uri, count, sum[:]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('ff.archive_run', $1, true)`, run.String()); err != nil {
			return err
		}
		for _, q := range []string{
			`DELETE FROM engine.run_events WHERE run_id = $1`,
			`DELETE FROM engine.effects WHERE run_id = $1`,
			`DELETE FROM engine.timers WHERE run_id = $1`,
			`DELETE FROM engine.snapshots WHERE run_id = $1`,
			`DELETE FROM engine.approval_ext WHERE approval_id IN (SELECT id FROM engine.approvals WHERE run_id = $1)`,
			`DELETE FROM engine.approvals WHERE run_id = $1`,
			`DELETE FROM engine.run_requests WHERE run_id = $1`,
			`DELETE FROM engine.run_ext WHERE run_id = $1`,
			`DELETE FROM engine.runs WHERE id = $1`,
		} {
			if _, err := tx.Exec(ctx, q, run.String()); err != nil {
				return err
			}
		}
		r.Logger.InfoContext(ctx, "maintenance.run_archived", "run_id", core.PublicID(core.PrefixRun, run), "events", count, "uri", uri)
		return nil
	})
}

func sha256Sum(b []byte) [sha256.Size]byte { return sha256.Sum256(b) }
