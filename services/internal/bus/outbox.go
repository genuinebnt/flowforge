package bus

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/internal/convert"
	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// DB is the subset of a pgx pool the Postgres adapters use.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// OutboxRow is one outbox message (spec 23.7): payload is convert.OutboxJSON.
type OutboxRow struct {
	ID          int64
	EventID     string
	Topic       string
	OrderingKey string
	Payload     []byte
	CreatedAt   time.Time
}

// OutboxSource is an outbox table. Claim hands up to limit unpublished rows
// to publish, in id order, and marks them published only if publish
// succeeds: a crash in between republishes them (at least once, never lost).
type OutboxSource interface {
	Claim(ctx context.Context, limit int, publish func(context.Context, []OutboxRow) error) (int, error)
}

// MemOutbox is the in-memory outbox (dev stack and tests). Append ignores
// event ids it already holds.
type MemOutbox struct {
	mu        sync.Mutex
	rows      []OutboxRow
	published map[int64]bool
	ids       map[string]bool
	claimMu   sync.Mutex
}

// NewMemOutbox returns an empty outbox.
func NewMemOutbox() *MemOutbox {
	return &MemOutbox{published: map[int64]bool{}, ids: map[string]bool{}}
}

// Append adds rows (ids and CreatedAt are assigned).
func (o *MemOutbox) Append(rows ...OutboxRow) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, r := range rows {
		if o.ids[r.EventID] {
			continue
		}
		o.ids[r.EventID] = true
		r.ID = int64(len(o.rows) + 1)
		if r.CreatedAt.IsZero() {
			r.CreatedAt = time.Now()
		}
		o.rows = append(o.rows, r)
	}
}

// Pending returns the number of unpublished rows.
func (o *MemOutbox) Pending() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.rows) - len(o.published)
}

// Claim implements OutboxSource.
func (o *MemOutbox) Claim(ctx context.Context, limit int, publish func(context.Context, []OutboxRow) error) (int, error) {
	o.claimMu.Lock()
	defer o.claimMu.Unlock()
	o.mu.Lock()
	var batch []OutboxRow
	for _, r := range o.rows {
		if !o.published[r.ID] {
			batch = append(batch, r)
			if len(batch) == limit {
				break
			}
		}
	}
	o.mu.Unlock()
	if len(batch) == 0 {
		return 0, nil
	}
	if err := publish(ctx, batch); err != nil {
		return 0, err
	}
	o.mu.Lock()
	for _, r := range batch {
		o.published[r.ID] = true
	}
	o.mu.Unlock()
	return len(batch), nil
}

// PgOutbox is an outbox table with the shape of migrations 0004/0008
// (id, event_id, topic, ordering_key, payload, created_at, published_at),
// for example engine.outbox or scope.outbox. One relay claims a table at a
// time (a transaction-scoped advisory lock on the table name; the others
// claim nothing until it commits), so rows are published in id order and
// two events of one ordering key never overtake each other (spec 11.7;
// GAPS G-SVC-25). Table is interpolated into SQL and must be a trusted
// identifier.
type PgOutbox struct {
	DB    DB
	Table string
}

// Claim implements OutboxSource.
func (o PgOutbox) Claim(ctx context.Context, limit int, publish func(context.Context, []OutboxRow) error) (int, error) {
	tx, err := o.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// Replicas each run a relay. With row locks alone (SKIP LOCKED) a
	// second relay took the rows after the first one's batch and could
	// publish them first, reordering a key across the two batches.
	var mine bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))`, "ff_outbox:"+o.Table).Scan(&mine); err != nil {
		return 0, err
	}
	if !mine {
		return 0, nil
	}
	rows, err := tx.Query(ctx, `SELECT id, event_id::text, topic, coalesce(ordering_key, ''), payload::text, created_at FROM `+o.Table+
		` WHERE published_at IS NULL ORDER BY id LIMIT $1 FOR UPDATE`, limit)
	if err != nil {
		return 0, err
	}
	var batch []OutboxRow
	for rows.Next() {
		var r OutboxRow
		var payload string
		if err := rows.Scan(&r.ID, &r.EventID, &r.Topic, &r.OrderingKey, &payload, &r.CreatedAt); err != nil {
			rows.Close()
			return 0, err
		}
		r.Payload = []byte(payload)
		batch = append(batch, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(batch) == 0 {
		return 0, nil
	}
	if err := publish(ctx, batch); err != nil {
		return 0, err
	}
	ids := make([]int64, len(batch))
	for i, r := range batch {
		ids[i] = r.ID
	}
	if _, err := tx.Exec(ctx, `UPDATE `+o.Table+` SET published_at = now() WHERE id = ANY($1)`, ids); err != nil {
		return 0, err
	}
	return len(batch), tx.Commit(ctx)
}

// Relay publishes an outbox to a bus (spec 23.7).
type Relay struct {
	Source   OutboxSource
	Bus      Bus
	Interval time.Duration // poll interval, default 200 ms
	Logger   *slog.Logger
	Metrics  *platform.Metrics
	// Decode turns a row into an envelope; default convert.EnvelopeFromOutbox.
	Decode func(OutboxRow) (*pb.EventEnvelope, error)
}

// RunOnce publishes at most one batch.
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	decode := r.Decode
	if decode == nil {
		decode = func(row OutboxRow) (*pb.EventEnvelope, error) {
			return convert.EnvelopeFromOutbox(row.Topic, row.Payload, "")
		}
	}
	return r.Source.Claim(ctx, 500, func(ctx context.Context, rows []OutboxRow) error {
		for _, row := range rows {
			env, err := decode(row)
			if err != nil {
				// A row that can never be decoded is logged and skipped
				// rather than blocking the outbox forever.
				r.Logger.ErrorContext(ctx, "outbox.undecodable", "topic", row.Topic, "event_id", row.EventID, "error", err.Error())
				continue
			}
			if err := r.Bus.Publish(ctx, row.Topic, env); err != nil {
				return err
			}
			if r.Metrics != nil && !row.CreatedAt.IsZero() {
				r.Metrics.OutboxLag.Since(row.CreatedAt, row.Topic)
			}
		}
		return nil
	})
}

// Run publishes until ctx is done: batches back to back while there are
// rows, then one poll every Interval (also after an error, which is
// logged).
func (r *Relay) Run(ctx context.Context) error {
	if r.Logger == nil {
		r.Logger = platform.DiscardLogger()
	}
	iv := r.Interval
	if iv <= 0 {
		iv = 200 * time.Millisecond
	}
	for {
		n, err := r.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			r.Logger.WarnContext(ctx, "outbox.relay_failed", "error", err.Error())
		}
		if ctx.Err() != nil {
			return nil
		}
		if n > 0 && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(iv):
		}
	}
}

// Inbox records processed events per consumer (processed_events, spec
// 23.7): MarkProcessed reports false for a duplicate.
type Inbox interface {
	MarkProcessed(ctx context.Context, consumer, eventID string) (bool, error)
	// Processed reports whether the event was already recorded.
	Processed(ctx context.Context, consumer, eventID string) (bool, error)
}

// MemInbox is the in-memory inbox.
type MemInbox struct {
	mu   sync.Mutex
	seen map[[2]string]bool
}

// NewMemInbox returns an empty inbox.
func NewMemInbox() *MemInbox { return &MemInbox{seen: map[[2]string]bool{}} }

// MarkProcessed implements Inbox.
func (i *MemInbox) MarkProcessed(_ context.Context, consumer, eventID string) (bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	k := [2]string{consumer, eventID}
	if i.seen[k] {
		return false, nil
	}
	i.seen[k] = true
	return true, nil
}

// Processed implements Inbox.
func (i *MemInbox) Processed(_ context.Context, consumer, eventID string) (bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.seen[[2]string{consumer, eventID}], nil
}

// Querier is what an inbox insert needs (a pool or a transaction).
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// PgInbox is a processed_events table (engine.processed_events,
// notifier.processed_events).
type PgInbox struct {
	DB    DB
	Table string
}

// Processed implements Inbox.
func (i PgInbox) Processed(ctx context.Context, consumer, eventID string) (bool, error) {
	id, err := convert.EventUUID(eventID)
	if err != nil {
		return false, err
	}
	var ok bool
	err = i.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+i.Table+` WHERE consumer = $1 AND event_id = $2)`, consumer, id.String()).Scan(&ok)
	return ok, err
}

// MarkProcessed implements Inbox.
func (i PgInbox) MarkProcessed(ctx context.Context, consumer, eventID string) (bool, error) {
	return MarkProcessedIn(ctx, i.DB, i.Table, consumer, eventID)
}

// MarkProcessedIn inserts into a processed_events table through q (use a
// transaction to make the mark atomic with the effect).
func MarkProcessedIn(ctx context.Context, q Querier, table, consumer, eventID string) (bool, error) {
	id, err := convert.EventUUID(eventID)
	if err != nil {
		return false, errors.New("bus: event id is not a UUID: " + eventID)
	}
	tag, err := q.Exec(ctx, `INSERT INTO `+table+` (consumer, event_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, consumer, id.String())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
