package pgstore

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/eventstore"
)

// Config configures a Store.
type Config struct {
	// DSN is the connection string. Open requires it; with New it is
	// optional and only used for the LISTEN connection that Subscribe uses
	// (without it Subscribe polls).
	DSN string
	// MaxConns bounds the built-in pool (Open only). Default 8.
	MaxConns int
	// PollInterval is how often a subscription polls when no notification
	// arrives (the fallback when LISTEN is unavailable or a notification
	// is lost). Default 1s.
	PollInterval time.Duration
	// WakeChannel is the NOTIFY channel for ready runs (spec 9.4). Default
	// "engine_wake"; the payload is the run id.
	WakeChannel string
	// EventsChannel is the NOTIFY channel for appends, which Subscribe
	// listens on. Default "engine_events"; the payload is the run id.
	EventsChannel string
}

func (c *Config) defaults() {
	if c.MaxConns <= 0 {
		c.MaxConns = 8
	}
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	if c.WakeChannel == "" {
		c.WakeChannel = "engine_wake"
	}
	if c.EventsChannel == "" {
		c.EventsChannel = "engine_events"
	}
}

// Store is eventstore.Store over the engine schema of the migrations
// package (engine.runs, engine.run_events, engine.snapshots; spec 10.4 and
// 23.4 implementation A). All lease and wake times come from the database
// clock (now()), the single time source for expiry, so worker clock skew
// cannot break ownership (spec 23.8).
//
// Fences map to the single bigint column engine.runs.fence: Fence{Term: 0,
// Counter: fence}. A fence with a non-zero term never matches.
type Store struct {
	db   DB
	own  *pool
	cfg  Config
	lis  *listener
	stop context.CancelFunc
}

var _ eventstore.Store = (*Store)(nil)

// Open connects to cfg.DSN with a built-in pool.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	cfg.defaults()
	pc, err := pgx.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("pgstore: parse DSN: %w", err)
	}
	p := newPool(pc, cfg.MaxConns)
	c, err := p.acquire(ctx)
	if err != nil {
		p.close()
		return nil, fmt.Errorf("pgstore: connect: %w", err)
	}
	p.release(c)
	s := newStore(p, cfg, pc)
	s.own = p
	return s, nil
}

// New wraps an existing pool (for example a *pgxpool.Pool). cfg.DSN, if
// set, is used for the LISTEN connection; otherwise Subscribe polls.
func New(db DB, cfg Config) (*Store, error) {
	cfg.defaults()
	var lc *pgx.ConnConfig
	if cfg.DSN != "" {
		var err error
		if lc, err = pgx.ParseConfig(cfg.DSN); err != nil {
			return nil, fmt.Errorf("pgstore: parse DSN: %w", err)
		}
	}
	return newStore(db, cfg, lc), nil
}

func newStore(db DB, cfg Config, listenCfg *pgx.ConnConfig) *Store {
	ctx, stop := context.WithCancel(context.Background())
	s := &Store{db: db, cfg: cfg, stop: stop}
	if listenCfg != nil {
		s.lis = newListener(ctx, listenCfg.Copy(), cfg.EventsChannel, cfg.PollInterval)
	}
	return s
}

// Close stops the listener and, for a store made by Open, closes its pool.
func (s *Store) Close() {
	s.stop()
	if s.lis != nil {
		s.lis.wait()
	}
	if s.own != nil {
		s.own.close()
	}
}

// ---- helpers ----

func pgUUID(u core.UUID) pgtype.UUID { return pgtype.UUID{Bytes: u, Valid: true} }

func pgOptUUID(u core.UUID) pgtype.UUID { return pgtype.UUID{Bytes: u, Valid: !u.IsZero()} }

func pgOptTime(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
}

// dbFence maps a Fence to the bigint column; a fence that cannot exist in
// this store maps to -1, which never matches.
func dbFence(f eventstore.Fence) int64 {
	if f.Term != 0 || f.Counter > math.MaxInt64 {
		return -1
	}
	return int64(f.Counter)
}

func micros(d time.Duration) int64 { return d.Microseconds() }

// mapErr translates constraint and data errors to the sentinel errors.
func mapErr(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "23505": // unique_violation
			return fmt.Errorf("%w: %w", eventstore.ErrExists, err)
		case "23503", "23514", "23502", "22P02", "22P05", "22021", "22003", "54000":
			// foreign key, check, not null, invalid text, untranslatable
			// character, bad encoding, out of range, too large
			return fmt.Errorf("%w: %w", eventstore.ErrInvalid, err)
		}
	}
	return err
}

func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("pgstore: %s: %w", op, mapErr(err))
}

func (s *Store) inTx(ctx context.Context, fn func(pgx.Tx) error) (err error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// exists reports whether the run exists.
func exists(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, run eventstore.RunID) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM engine.runs WHERE id = $1)`, pgUUID(run)).Scan(&ok)
	return ok, err
}

func (s *Store) notFoundOr(ctx context.Context, run eventstore.RunID, otherwise error) error {
	ok, err := exists(ctx, s.db, run)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", eventstore.ErrNotFound, run)
	}
	return otherwise
}

const insertEvents = `INSERT INTO engine.run_events (run_id, seq, type, at, actor, payload)
SELECT $1::uuid, u.seq, u.type, COALESCE(u.at, now()), u.actor, u.payload::jsonb
FROM unnest($2::bigint[], $3::text[], $4::timestamptz[], $5::text[], $6::text[]) AS u(seq, type, at, actor, payload)`

func eventArgs(run eventstore.RunID, evs []eventstore.Event) []any {
	seqs := make([]int64, len(evs))
	types := make([]string, len(evs))
	ats := make([]pgtype.Timestamptz, len(evs))
	actors := make([]string, len(evs))
	payloads := make([]string, len(evs))
	for i, e := range evs {
		seqs[i], types[i], ats[i], actors[i], payloads[i] = int64(e.Seq), e.Type, pgOptTime(e.At), e.Actor, string(e.Payload)
	}
	return []any{pgUUID(run), seqs, types, ats, actors, payloads}
}

// ---- Store ----

// CreateRun implements eventstore.Store.
func (s *Store) CreateRun(ctx context.Context, spec eventstore.RunSpec, initial []eventstore.Event) error {
	spec, err := eventstore.PrepareRunSpec(spec)
	if err != nil {
		return err
	}
	evs, err := eventstore.PrepareEvents(spec.ID, 0, initial)
	if err != nil {
		return err
	}
	var scopeVersion pgtype.Int4
	if spec.ScopeVersion != 0 {
		scopeVersion = pgtype.Int4{Int32: spec.ScopeVersion, Valid: true}
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO engine.runs (id, tenant_id, program_id, workflow_version_id,
  parent_run_id, scope_version, status, input, created_by, idempotency_key, request_hash, priority,
  deadline_at, next_wake_at, last_seq)
VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7::text::jsonb, $8, $9, $10, $11, $12, COALESCE($13, now()), $14)`,
			pgUUID(spec.ID), pgUUID(spec.Tenant), pgOptUUID(spec.Program), pgUUID(spec.WorkflowVersion),
			pgOptUUID(spec.Parent), scopeVersion, string(spec.Input), spec.CreatedBy, spec.IdempotencyKey,
			spec.RequestHash, spec.Priority, pgOptTime(spec.Deadline), pgOptTime(spec.WakeAt), int64(len(evs))); err != nil {
			return err
		}
		if len(evs) > 0 {
			if _, err := tx.Exec(ctx, insertEvents, eventArgs(spec.ID, evs)...); err != nil {
				return err
			}
		}
		if spec.WakeAt.IsZero() {
			if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, s.cfg.WakeChannel, spec.ID.String()); err != nil {
				return err
			}
		}
		return nil
	})
	return wrap("create run", err)
}

// GetRun implements eventstore.Store.
func (s *Store) GetRun(ctx context.Context, run eventstore.RunID) (eventstore.RunInfo, error) {
	var (
		info    eventstore.RunInfo
		tenant  pgtype.UUID
		status  string
		head    int64
		fence   int64
		owner   pgtype.Text
		expires pgtype.Timestamptz
	)
	err := s.db.QueryRow(ctx, `SELECT tenant_id, status, last_seq, priority, next_wake_at, fence, lease_owner, lease_expires_at
FROM engine.runs WHERE id = $1`, pgUUID(run)).
		Scan(&tenant, &status, &head, &info.Priority, &info.NextWakeAt, &fence, &owner, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return info, fmt.Errorf("%w: %s", eventstore.ErrNotFound, run)
	}
	if err != nil {
		return info, wrap("get run", err)
	}
	info.ID, info.Tenant, info.Status, info.Head = run, tenant.Bytes, eventstore.RunStatus(status), uint64(head)
	info.NextWakeAt = info.NextWakeAt.UTC()
	info.Fence = eventstore.Fence{Counter: uint64(fence)}
	info.LeaseOwner = eventstore.WorkerID(owner.String)
	if expires.Valid {
		info.LeaseExpiresAt = expires.Time.UTC()
	}
	return info, nil
}

// Append implements eventstore.Store: one transaction that locks the run
// row (SELECT ... FOR UPDATE), checks fence and head, inserts the events
// and advances last_seq (spec 23.4 implementation A).
func (s *Store) Append(ctx context.Context, run eventstore.RunID, expectedSeq uint64, fence eventstore.Fence, events []eventstore.Event) (uint64, error) {
	if err := eventstore.CheckSeq("expected seq", expectedSeq); err != nil {
		return 0, err
	}
	evs, err := eventstore.PrepareEvents(run, expectedSeq, events)
	if err != nil {
		return 0, err
	}
	newHead := expectedSeq + uint64(len(evs))
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var (
			head, cur int64
			held      bool
		)
		err := tx.QueryRow(ctx, `SELECT last_seq, fence, lease_owner IS NOT NULL FROM engine.runs WHERE id = $1 FOR UPDATE`,
			pgUUID(run)).Scan(&head, &cur, &held)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", eventstore.ErrNotFound, run)
		}
		if err != nil {
			return err
		}
		if !held || dbFence(fence) != cur {
			return fmt.Errorf("%w: run %s fence %s, current %d (held: %t)", eventstore.ErrFenced, run, fence, cur, held)
		}
		if uint64(head) != expectedSeq {
			return fmt.Errorf("%w: run %s expected seq %d, head %d", eventstore.ErrSeqConflict, run, expectedSeq, head)
		}
		if len(evs) == 0 {
			return nil
		}
		args := append(eventArgs(run, evs), int64(newHead), s.cfg.EventsChannel)
		_, err = tx.Exec(ctx, `WITH ins AS (`+insertEvents+`),
upd AS (UPDATE engine.runs SET last_seq = $7 WHERE id = $1)
SELECT pg_notify($8, $1::uuid::text)`, args...)
		return err
	})
	if err != nil {
		return 0, wrap("append", err)
	}
	return newHead, nil
}

// Read implements eventstore.Store.
func (s *Store) Read(ctx context.Context, run eventstore.RunID, fromSeq uint64, limit int) ([]eventstore.Event, error) {
	from := int64(min(max(fromSeq, 1), eventstore.MaxSeq))
	var lim pgtype.Int8
	if limit > 0 {
		lim = pgtype.Int8{Int64: int64(limit), Valid: true}
	}
	rows, err := s.db.Query(ctx, `SELECT seq, type, at, actor, payload::text FROM engine.run_events
WHERE run_id = $1 AND seq >= $2 ORDER BY seq LIMIT $3`, pgUUID(run), from, lim)
	if err != nil {
		return nil, wrap("read", err)
	}
	defer rows.Close()
	out := []eventstore.Event{}
	for rows.Next() {
		var (
			ev      eventstore.Event
			seq     int64
			payload string
		)
		if err := rows.Scan(&seq, &ev.Type, &ev.At, &ev.Actor, &payload); err != nil {
			return nil, wrap("read", err)
		}
		// jsonb normalizes whitespace and key order its own way; return the
		// same canonical form MemStore stores.
		p, err := eventstore.CanonicalPayload([]byte(payload))
		if err != nil {
			return nil, fmt.Errorf("pgstore: read: run %s seq %d: %w", run, seq, err)
		}
		ev.RunID, ev.Seq, ev.At, ev.Payload = run, uint64(seq), ev.At.UTC(), p
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("read", err)
	}
	rows.Close()
	if len(out) == 0 {
		if err := s.notFoundOr(ctx, run, nil); err != nil {
			return nil, wrap("read", err)
		}
	}
	return out, nil
}

// subscribeBatch bounds how many events Subscribe reads per query.
const subscribeBatch = 500

// Subscribe implements eventstore.Store. It reads in batches, then waits
// for a NOTIFY on the events channel or the poll interval, whichever comes
// first. Missed notifications only delay delivery: the poll catches up.
func (s *Store) Subscribe(ctx context.Context, run eventstore.RunID, fromSeq uint64) iter.Seq2[eventstore.Event, error] {
	return func(yield func(eventstore.Event, error) bool) {
		var sig <-chan struct{}
		if s.lis != nil {
			ch, cancel := s.lis.register(run)
			defer cancel()
			sig = ch
		}
		ticker := time.NewTicker(s.cfg.PollInterval)
		defer ticker.Stop()
		next := max(fromSeq, 1)
		for {
			if err := ctx.Err(); err != nil {
				yield(eventstore.Event{}, err)
				return
			}
			evs, err := s.Read(ctx, run, next, subscribeBatch)
			if err != nil {
				if cerr := ctx.Err(); cerr != nil {
					err = cerr
				}
				yield(eventstore.Event{}, err)
				return
			}
			for _, ev := range evs {
				if !yield(ev, nil) {
					return
				}
				next = ev.Seq + 1
			}
			if len(evs) == subscribeBatch {
				continue
			}
			select {
			case <-sig:
			case <-ticker.C:
			case <-ctx.Done():
				yield(eventstore.Event{}, ctx.Err())
				return
			}
		}
	}
}

// SaveSnapshot implements eventstore.Store; it keeps the newest
// eventstore.SnapshotsKept snapshots per run.
func (s *Store) SaveSnapshot(ctx context.Context, run eventstore.RunID, snap eventstore.Snapshot) error {
	if err := eventstore.CheckSeq("snapshot seq", snap.AtSeq); err != nil {
		return err
	}
	mod, state := snap.ModuleSHA256, snap.State
	if mod == nil {
		mod = []byte{}
	}
	if state == nil {
		state = []byte{}
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var head int64
		err := tx.QueryRow(ctx, `SELECT last_seq FROM engine.runs WHERE id = $1 FOR UPDATE`, pgUUID(run)).Scan(&head)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", eventstore.ErrNotFound, run)
		}
		if err != nil {
			return err
		}
		if snap.AtSeq > uint64(head) {
			return fmt.Errorf("%w: snapshot at seq %d beyond head %d", eventstore.ErrInvalid, snap.AtSeq, head)
		}
		_, err = tx.Exec(ctx, `WITH ins AS (
  INSERT INTO engine.snapshots (run_id, at_seq, module_sha256, state) VALUES ($1, $2, $3, $4)
  ON CONFLICT (run_id, at_seq) DO NOTHING RETURNING at_seq)
DELETE FROM engine.snapshots WHERE run_id = $1 AND at_seq NOT IN (
  SELECT at_seq FROM (
    SELECT at_seq FROM engine.snapshots WHERE run_id = $1
    UNION SELECT at_seq FROM ins) k
  ORDER BY at_seq DESC LIMIT $5)`, pgUUID(run), int64(snap.AtSeq), mod, state, eventstore.SnapshotsKept)
		return err
	})
	return wrap("save snapshot", err)
}

// LoadSnapshot implements eventstore.Store.
func (s *Store) LoadSnapshot(ctx context.Context, run eventstore.RunID) (eventstore.Snapshot, bool, error) {
	var (
		snap eventstore.Snapshot
		at   int64
	)
	err := s.db.QueryRow(ctx, `SELECT at_seq, module_sha256, state FROM engine.snapshots
WHERE run_id = $1 ORDER BY at_seq DESC LIMIT 1`, pgUUID(run)).Scan(&at, &snap.ModuleSHA256, &snap.State)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := s.notFoundOr(ctx, run, nil); err != nil {
			return snap, false, wrap("load snapshot", err)
		}
		return snap, false, nil
	}
	if err != nil {
		return snap, false, wrap("load snapshot", err)
	}
	snap.AtSeq = uint64(at)
	return snap, true, nil
}

// AcquireLease implements eventstore.Store with the conditional UPDATE of
// spec 23.4: the database clock decides expiry and fence + 1 makes fences
// strictly increasing, durably.
func (s *Store) AcquireLease(ctx context.Context, run eventstore.RunID, owner eventstore.WorkerID, ttl time.Duration) (eventstore.Lease, error) {
	if err := eventstore.CheckWorker(owner); err != nil {
		return eventstore.Lease{}, err
	}
	if err := eventstore.CheckTTL(ttl); err != nil {
		return eventstore.Lease{}, err
	}
	var (
		fence   int64
		expires time.Time
	)
	err := s.db.QueryRow(ctx, `UPDATE engine.runs SET fence = fence + 1, lease_owner = $2,
  lease_expires_at = now() + $3::bigint * interval '1 microsecond', lease_acquired_at = now()
WHERE id = $1 AND (lease_expires_at IS NULL OR lease_expires_at < now() OR lease_owner = $2)
RETURNING fence, lease_expires_at`, pgUUID(run), string(owner), micros(ttl)).Scan(&fence, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		var holder pgtype.Text
		err = s.db.QueryRow(ctx, `SELECT lease_owner FROM engine.runs WHERE id = $1`, pgUUID(run)).Scan(&holder)
		if errors.Is(err, pgx.ErrNoRows) {
			return eventstore.Lease{}, fmt.Errorf("%w: %s", eventstore.ErrNotFound, run)
		}
		if err != nil {
			return eventstore.Lease{}, wrap("acquire lease", err)
		}
		return eventstore.Lease{}, fmt.Errorf("%w: run %s is leased to %s", eventstore.ErrLeaseHeld, run, holder.String)
	}
	if err != nil {
		return eventstore.Lease{}, wrap("acquire lease", err)
	}
	return eventstore.Lease{Run: run, Owner: owner, Fence: eventstore.Fence{Counter: uint64(fence)}, ExpiresAt: expires.UTC()}, nil
}

func (s *Store) fencedOrNotFound(ctx context.Context, lease eventstore.Lease) error {
	return s.notFoundOr(ctx, lease.Run, fmt.Errorf("%w: run %s lease %s of %s is not current",
		eventstore.ErrFenced, lease.Run, lease.Fence, lease.Owner))
}

// RenewLease implements eventstore.Store.
func (s *Store) RenewLease(ctx context.Context, lease eventstore.Lease, ttl time.Duration) (eventstore.Lease, error) {
	if err := eventstore.CheckTTL(ttl); err != nil {
		return eventstore.Lease{}, err
	}
	var expires time.Time
	err := s.db.QueryRow(ctx, `UPDATE engine.runs SET lease_expires_at = now() + $4::bigint * interval '1 microsecond'
WHERE id = $1 AND fence = $2 AND lease_owner = $3 RETURNING lease_expires_at`,
		pgUUID(lease.Run), dbFence(lease.Fence), string(lease.Owner), micros(ttl)).Scan(&expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return eventstore.Lease{}, wrap("renew lease", s.fencedOrNotFound(ctx, lease))
	}
	if err != nil {
		return eventstore.Lease{}, wrap("renew lease", err)
	}
	lease.ExpiresAt = expires.UTC()
	return lease, nil
}

// schedulable is the SQL form of eventstore.RunStatus.Schedulable.
const schedulable = `status IN ('pending','running','waiting')`

// ReleaseLease implements eventstore.Store. A wake that arrived while the
// lease was held (woken_at >= lease_acquired_at) makes the run due now.
func (s *Store) ReleaseLease(ctx context.Context, lease eventstore.Lease, next eventstore.Next) error {
	if err := eventstore.CheckNext(next); err != nil {
		return err
	}
	var status pgtype.Text
	if next.Status != "" {
		status = pgtype.Text{String: string(next.Status), Valid: true}
	}
	var wake pgtype.Timestamptz
	if !next.WakeAt.IsZero() {
		wake = pgtype.Timestamptz{Time: next.WakeAt.UTC().Truncate(time.Microsecond), Valid: true}
	}
	tag, err := s.db.Exec(ctx, `WITH u AS (
  UPDATE engine.runs SET lease_owner = NULL, lease_expires_at = NULL,
    status = COALESCE($4, status),
    next_wake_at = CASE WHEN woken_at >= lease_acquired_at THEN now() ELSE COALESCE($5, now()) END
  WHERE id = $1 AND fence = $2 AND lease_owner = $3
  RETURNING id, status, next_wake_at)
SELECT CASE WHEN next_wake_at <= now() AND `+schedulable+` THEN pg_notify($6, id::text) IS NULL END FROM u`,
		pgUUID(lease.Run), dbFence(lease.Fence), string(lease.Owner), status, wake, s.cfg.WakeChannel)
	if err != nil {
		return wrap("release lease", err)
	}
	if tag.RowsAffected() == 0 {
		return wrap("release lease", s.fencedOrNotFound(ctx, lease))
	}
	return nil
}

const readyWhere = schedulable + ` AND next_wake_at <= now()
  AND (lease_expires_at IS NULL OR lease_expires_at < now())
  AND ($1::bigint = 0 OR (('x' || right(replace(id::text, '-', ''), 8))::bit(32)::bigint % $1::bigint) = $2::bigint)`

// ListReady implements eventstore.Store (FOR UPDATE SKIP LOCKED: rows
// locked by a concurrent claim or append are skipped, not waited for).
func (s *Store) ListReady(ctx context.Context, shard eventstore.Shard, limit int) ([]eventstore.RunID, error) {
	if err := eventstore.CheckShard(shard); err != nil {
		return nil, err
	}
	out := []eventstore.RunID{}
	if limit <= 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `SELECT id FROM engine.runs WHERE `+readyWhere+`
ORDER BY priority DESC, next_wake_at, id LIMIT $3 FOR UPDATE SKIP LOCKED`,
		int64(shard.Count), int64(shard.Index), int64(limit))
	if err != nil {
		return nil, wrap("list ready", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id pgtype.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, wrap("list ready", err)
		}
		out = append(out, id.Bytes)
	}
	return out, wrap("list ready", rows.Err())
}

// ClaimReady implements eventstore.Store: one statement selects ready rows
// with FOR UPDATE SKIP LOCKED and leases them.
func (s *Store) ClaimReady(ctx context.Context, shard eventstore.Shard, owner eventstore.WorkerID, ttl time.Duration, limit int) ([]eventstore.Lease, error) {
	if err := eventstore.CheckShard(shard); err != nil {
		return nil, err
	}
	if err := eventstore.CheckWorker(owner); err != nil {
		return nil, err
	}
	if err := eventstore.CheckTTL(ttl); err != nil {
		return nil, err
	}
	out := []eventstore.Lease{}
	if limit <= 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `WITH c AS (
  SELECT id FROM engine.runs WHERE `+readyWhere+`
  ORDER BY priority DESC, next_wake_at, id LIMIT $3 FOR UPDATE SKIP LOCKED),
u AS (
  UPDATE engine.runs r SET fence = r.fence + 1, lease_owner = $4,
    lease_expires_at = now() + $5::bigint * interval '1 microsecond', lease_acquired_at = now()
  FROM c WHERE r.id = c.id
  RETURNING r.id, r.fence, r.lease_expires_at, r.priority, r.next_wake_at)
SELECT id, fence, lease_expires_at FROM u ORDER BY priority DESC, next_wake_at, id`,
		int64(shard.Count), int64(shard.Index), int64(limit), string(owner), micros(ttl))
	if err != nil {
		return nil, wrap("claim ready", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id      pgtype.UUID
			fence   int64
			expires time.Time
		)
		if err := rows.Scan(&id, &fence, &expires); err != nil {
			return nil, wrap("claim ready", err)
		}
		out = append(out, eventstore.Lease{Run: id.Bytes, Owner: owner, Fence: eventstore.Fence{Counter: uint64(fence)}, ExpiresAt: expires.UTC()})
	}
	return out, wrap("claim ready", rows.Err())
}

// Wake implements eventstore.Store: next_wake_at = now(), woken_at = now(),
// and a NOTIFY on the wake channel (delivered at commit).
func (s *Store) Wake(ctx context.Context, run eventstore.RunID) error {
	tag, err := s.db.Exec(ctx, `WITH u AS (
  UPDATE engine.runs SET next_wake_at = now(), woken_at = now() WHERE id = $1 RETURNING id)
SELECT pg_notify($2, id::text) FROM u`, pgUUID(run), s.cfg.WakeChannel)
	if err != nil {
		return wrap("wake", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", eventstore.ErrNotFound, run)
	}
	return nil
}
