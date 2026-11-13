package bus

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// NotifyChannel is the LISTEN/NOTIFY channel PgBus wakes subscribers on;
// the payload is the topic.
const NotifyChannel = "ff_bus"

// PgBus is the Postgres Bus (migration 0016) for running the services as
// separate local processes against one database (G-SVC-01). It gives the
// same guarantees as Pub/Sub with ordering enabled and a dead-letter
// policy (spec 11.7, 17):
//
//   - Subscriptions are durable rows: a consumer that is down keeps
//     receiving its backlog when it comes back.
//   - At least once: a delivery is acked only after the handler returns
//     nil; a crashed consumer's lease expires (Lease, the ack deadline) and
//     another process claims the row.
//   - Redelivery with exponential backoff (Backoff doubling up to
//     MaxBackoff); after MaxAttempts the delivery is marked dead and the
//     envelope is published to <topic>.dead-letter.
//   - Ordering per key (OrderingKey): a delivery is claimable only while no
//     earlier delivery of the same subscription and key is pending, so a
//     failing message holds back the rest of its key, as Pub/Sub does.
//     Messages without a key are unordered.
//   - Wakeups by LISTEN/NOTIFY on NotifyChannel, with polling (Poll) as the
//     fallback for lost notifications, retries and lease expiry.
//
// Several processes may consume the same subscription: claims use
// FOR UPDATE SKIP LOCKED, so they share the backlog. All times come from
// the database clock, so processes with skewed clocks agree on leases.
//
// PORT: the listener holds one pool connection hijacked out of the pool for
// the bus's lifetime (pgx cancels a WaitForNotification by closing the
// connection, so it must never return to the pool).
type PgBus struct {
	Pool        *pgxpool.Pool
	MaxAttempts int           // default 5 (the Terraform max_delivery_attempts)
	Backoff     time.Duration // first retry delay, default 100 ms
	MaxBackoff  time.Duration // default 10 s
	Lease       time.Duration // ack deadline, default 60 s (spec 17)
	Poll        time.Duration // fallback poll interval, default 1 s
	Batch       int           // deliveries claimed per round, default 16
	Logger      *slog.Logger
	Metrics     *platform.Metrics

	mu       sync.Mutex
	waiters  map[string]map[chan struct{}]struct{}
	active   int
	stopLsn  context.CancelFunc
	lsnDone  chan struct{}
	initOnce sync.Once
}

// NewPgBus returns a Postgres bus on pool with the defaults above.
func NewPgBus(pool *pgxpool.Pool, log *slog.Logger, m *platform.Metrics) *PgBus {
	b := &PgBus{Pool: pool, Logger: log, Metrics: m}
	b.init()
	return b
}

func (b *PgBus) init() {
	b.initOnce.Do(func() {
		if b.MaxAttempts <= 0 {
			b.MaxAttempts = 5
		}
		if b.Backoff <= 0 {
			b.Backoff = 100 * time.Millisecond
		}
		if b.MaxBackoff <= 0 {
			b.MaxBackoff = 10 * time.Second
		}
		if b.Lease <= 0 {
			b.Lease = 60 * time.Second
		}
		if b.Poll <= 0 {
			b.Poll = time.Second
		}
		if b.Batch <= 0 {
			b.Batch = 16
		}
		if b.Logger == nil {
			b.Logger = platform.DiscardLogger()
		}
		if b.Metrics == nil {
			b.Metrics = platform.NewMetrics(nil)
		}
		b.waiters = map[string]map[chan struct{}]struct{}{}
	})
}

// Publish implements Bus: the message and one delivery per existing
// subscription of topic are written in one transaction, which also sends
// the NOTIFY (delivered at commit, so a woken consumer always sees the rows).
func (b *PgBus) Publish(ctx context.Context, topic string, env *pb.EventEnvelope) error {
	b.init()
	data, err := proto.Marshal(env)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, b.Pool, func(tx pgx.Tx) error {
		return publishTx(ctx, tx, topic, env.GetEventId(), OrderingKey(topic, env), data)
	})
}

func publishTx(ctx context.Context, tx pgx.Tx, topic, eventID, key string, data []byte) error {
	if _, err := tx.Exec(ctx, `WITH m AS (
  INSERT INTO bus.messages (topic, event_id, ordering_key, envelope) VALUES ($1, $2, $3, $4) RETURNING id)
INSERT INTO bus.deliveries (topic, consumer, message_id, ordering_key)
SELECT s.topic, s.consumer, m.id, $3 FROM bus.subscriptions s, m WHERE s.topic = $1`, topic, eventID, key, data); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, NotifyChannel, topic)
	return err
}

// Subscribe implements Bus. The subscription row is created before it
// returns, so every later Publish fans out to it; it survives the process.
func (b *PgBus) Subscribe(ctx context.Context, topic, consumer string, h Handler) error {
	b.init()
	if _, err := b.Pool.Exec(ctx, `INSERT INTO bus.subscriptions (topic, consumer) VALUES ($1, $2) ON CONFLICT DO NOTHING`, topic, consumer); err != nil {
		return err
	}
	sig := make(chan struct{}, 1)
	b.mu.Lock()
	if b.waiters[topic] == nil {
		b.waiters[topic] = map[chan struct{}]struct{}{}
	}
	b.waiters[topic][sig] = struct{}{}
	b.active++
	if b.active == 1 {
		lctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		b.stopLsn, b.lsnDone = cancel, make(chan struct{})
		go b.listen(lctx, b.lsnDone)
	}
	b.mu.Unlock()
	go func() {
		defer b.unsubscribe(topic, sig)
		b.loop(ctx, topic, consumer, h, sig)
	}()
	return nil
}

func (b *PgBus) unsubscribe(topic string, sig chan struct{}) {
	b.mu.Lock()
	delete(b.waiters[topic], sig)
	b.active--
	var stop context.CancelFunc
	var done chan struct{}
	if b.active == 0 {
		stop, done = b.stopLsn, b.lsnDone
		b.stopLsn, b.lsnDone = nil, nil
	}
	b.mu.Unlock()
	if stop != nil {
		stop()
		<-done
	}
}

// listen forwards notifications to the subscribers of their topic until
// ctx is done, reconnecting with backoff; polling covers the gaps.
func (b *PgBus) listen(ctx context.Context, done chan struct{}) {
	defer close(done)
	backoff := 100 * time.Millisecond
	for ctx.Err() == nil {
		err := b.listenOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		b.Logger.WarnContext(ctx, "bus.listen_failed", "error", errString(err))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 10*time.Second)
	}
}

func (b *PgBus) listenOnce(ctx context.Context) error {
	pc, err := b.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	conn := pc.Hijack()
	defer conn.Close(context.WithoutCancel(ctx))
	if _, err := conn.Exec(ctx, "LISTEN "+NotifyChannel); err != nil {
		return err
	}
	b.wakeAll() // anything published while we were not listening
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		b.wake(n.Payload)
	}
}

func (b *PgBus) wake(topic string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for c := range b.waiters[topic] {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

func (b *PgBus) wakeAll() {
	b.mu.Lock()
	topics := make([]string, 0, len(b.waiters))
	for t := range b.waiters {
		topics = append(topics, t)
	}
	b.mu.Unlock()
	for _, t := range topics {
		b.wake(t)
	}
}

func (b *PgBus) loop(ctx context.Context, topic, consumer string, h Handler, sig chan struct{}) {
	for ctx.Err() == nil {
		n, retry, err := b.drain(ctx, topic, consumer, h)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			b.Logger.WarnContext(ctx, "bus.claim_failed", "topic", topic, "consumer", consumer, "error", err.Error())
		} else if n > 0 {
			continue
		}
		wait := b.Poll
		if retry > 0 && retry < wait {
			wait = retry
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-sig:
		case <-t.C:
		}
		t.Stop()
	}
}

type claimed struct {
	msgID    int64
	attempts int
	env      *pb.EventEnvelope
	raw      []byte
}

// claimSQL leases up to $3 deliveries of one subscription. The NOT EXISTS
// guard is what keeps ordering per key: a row whose key has an earlier
// pending row (leased, backing off or simply not yet claimed) waits. Rows
// skipped by SKIP LOCKED are still pending, so the guard also sees rows
// another process is claiming in the same instant.
const claimSQL = `WITH c AS (
  SELECT d.message_id FROM bus.deliveries d
  WHERE d.topic = $1 AND d.consumer = $2 AND d.status = 'pending' AND d.available_at <= now()
    AND (d.lease_until IS NULL OR d.lease_until <= now())
    AND (d.ordering_key = '' OR NOT EXISTS (
      SELECT 1 FROM bus.deliveries e
      WHERE e.topic = d.topic AND e.consumer = d.consumer AND e.ordering_key = d.ordering_key
        AND e.status = 'pending' AND e.ordering_key <> '' AND e.message_id < d.message_id))
  ORDER BY d.message_id LIMIT $3
  FOR UPDATE OF d SKIP LOCKED)
UPDATE bus.deliveries d SET lease_until = now() + make_interval(secs => $4), attempts = d.attempts + 1
FROM c, bus.messages m
WHERE d.topic = $1 AND d.consumer = $2 AND d.message_id = c.message_id AND m.id = d.message_id
RETURNING d.message_id, d.attempts, m.envelope`

// drain claims one batch and handles it in publication order. It returns
// how many deliveries it handled and the shortest retry delay it set.
func (b *PgBus) drain(ctx context.Context, topic, consumer string, h Handler) (int, time.Duration, error) {
	rows, err := b.Pool.Query(ctx, claimSQL, topic, consumer, b.Batch, b.Lease.Seconds())
	if err != nil {
		return 0, 0, err
	}
	var batch []claimed
	for rows.Next() {
		var c claimed
		if err := rows.Scan(&c.msgID, &c.attempts, &c.raw); err != nil {
			rows.Close()
			return 0, 0, err
		}
		batch = append(batch, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	// RETURNING does not preserve the CTE's ORDER BY.
	sortClaimed(batch)
	var retry time.Duration
	for _, c := range batch {
		env := &pb.EventEnvelope{}
		if err := proto.Unmarshal(c.raw, env); err != nil {
			// An undecodable message can never succeed: dead-letter it now
			// rather than burn its attempts.
			c.attempts = b.MaxAttempts
			_, _ = b.nack(ctx, topic, consumer, c, nil, err)
			continue
		}
		c.env = env
		hctx := ctx
		if t, ok := platform.ParseTraceparent(env.GetTraceparent()); ok {
			hctx = platform.WithTrace(ctx, t.Child())
		}
		herr := safeCall(hctx, h, env)
		if ctx.Err() != nil {
			// Shutting down: leave the lease to expire so another process
			// (or this one after restart) redelivers.
			return len(batch), 0, nil
		}
		if herr == nil {
			if _, err := b.Pool.Exec(ctx, `UPDATE bus.deliveries SET status = 'acked', done_at = now(), lease_until = NULL
WHERE topic = $1 AND consumer = $2 AND message_id = $3 AND attempts = $4 AND status = 'pending'`, topic, consumer, c.msgID, c.attempts); err != nil {
				return len(batch), 0, err
			}
			continue
		}
		d, err := b.nack(ctx, topic, consumer, c, env, herr)
		if err != nil {
			return len(batch), 0, err
		}
		if d > 0 && (retry == 0 || d < retry) {
			retry = d
		}
	}
	return len(batch), retry, nil
}

func sortClaimed(cs []claimed) {
	for i := 1; i < len(cs); i++ {
		for j := i; j > 0 && cs[j].msgID < cs[j-1].msgID; j-- {
			cs[j], cs[j-1] = cs[j-1], cs[j]
		}
	}
}

// backoff is the delay before attempt+1: Backoff * 2^(attempt-1), capped.
func (b *PgBus) backoff(attempt int) time.Duration {
	d := b.Backoff
	for i := 1; i < attempt && d < b.MaxBackoff; i++ {
		d *= 2
	}
	return min(d, b.MaxBackoff)
}

// nack reschedules a failed delivery, or marks it dead and publishes it to
// the dead-letter topic in the same transaction once attempts are used up.
func (b *PgBus) nack(ctx context.Context, topic, consumer string, c claimed, env *pb.EventEnvelope, herr error) (time.Duration, error) {
	reason := truncate(errString(herr), 500)
	if c.attempts < b.MaxAttempts {
		d := b.backoff(c.attempts)
		b.Logger.WarnContext(ctx, "bus.redeliver", "topic", topic, "consumer", consumer, "event_id", env.GetEventId(), "attempt", c.attempts, "error", reason)
		_, err := b.Pool.Exec(ctx, `UPDATE bus.deliveries SET lease_until = NULL, last_error = $5,
  available_at = now() + make_interval(secs => $6)
WHERE topic = $1 AND consumer = $2 AND message_id = $3 AND attempts = $4 AND status = 'pending'`,
			topic, consumer, c.msgID, c.attempts, reason, d.Seconds())
		return d, err
	}
	err := pgx.BeginFunc(ctx, b.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE bus.deliveries SET status = 'dead', done_at = now(), lease_until = NULL, last_error = $5
WHERE topic = $1 AND consumer = $2 AND message_id = $3 AND attempts = $4 AND status = 'pending'`, topic, consumer, c.msgID, c.attempts, reason)
		if err != nil || tag.RowsAffected() == 0 || strings.HasSuffix(topic, DeadLetterSuffix) {
			return err
		}
		return publishTx(ctx, tx, topic+DeadLetterSuffix, env.GetEventId(), "", c.raw)
	})
	if err != nil {
		return 0, err
	}
	b.Metrics.BusDeadLetters.Inc(topic)
	b.Logger.ErrorContext(ctx, "bus.dead_letter", "topic", topic, "consumer", consumer, "event_id", env.GetEventId(), "error", reason)
	return 0, nil
}

// DeadLetters returns the dead deliveries of topic (all topics when empty),
// oldest first, for tests and operators.
func (b *PgBus) DeadLetters(ctx context.Context, topic string) ([]DeadLetter, error) {
	rows, err := b.Pool.Query(ctx, `SELECT d.topic, d.consumer, d.last_error, m.envelope FROM bus.deliveries d
JOIN bus.messages m ON m.id = d.message_id WHERE d.status = 'dead' AND ($1 = '' OR d.topic = $1) ORDER BY d.done_at, d.message_id`, topic)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeadLetter
	for rows.Next() {
		var dl DeadLetter
		var raw []byte
		if err := rows.Scan(&dl.Topic, &dl.Consumer, &dl.Reason, &raw); err != nil {
			return nil, err
		}
		dl.Envelope = &pb.EventEnvelope{}
		_ = proto.Unmarshal(raw, dl.Envelope)
		out = append(out, dl)
	}
	return out, rows.Err()
}

// Retention bounds PgBus.Cleanup.
type Retention struct {
	Acked time.Duration // keep acked deliveries this long (default 24 h)
	Dead  time.Duration // keep dead deliveries this long (default 14 days, spec 11.7)
	Limit int           // rows deleted per table per call (default 5000)
}

// Cleanup deletes acked and dead deliveries past their retention, then
// messages that no delivery references any more, at most Limit rows each.
// Pending deliveries are never deleted: an unconsumed message is kept
// until its consumer takes it (G-OPS-03). It is idempotent.
func (b *PgBus) Cleanup(ctx context.Context, r Retention) (int64, error) {
	if r.Acked <= 0 {
		r.Acked = 24 * time.Hour
	}
	if r.Dead <= 0 {
		r.Dead = 14 * 24 * time.Hour
	}
	if r.Limit <= 0 {
		r.Limit = 5000
	}
	var total int64
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM bus.deliveries WHERE ctid IN (SELECT ctid FROM bus.deliveries
WHERE status = 'acked' AND done_at < now() - make_interval(secs => $1) LIMIT $2)`, []any{r.Acked.Seconds(), r.Limit}},
		{`DELETE FROM bus.deliveries WHERE ctid IN (SELECT ctid FROM bus.deliveries
WHERE status = 'dead' AND done_at < now() - make_interval(secs => $1) LIMIT $2)`, []any{r.Dead.Seconds(), r.Limit}},
		{`DELETE FROM bus.messages WHERE id IN (SELECT m.id FROM bus.messages m
WHERE m.published_at < now() - make_interval(secs => $1)
  AND NOT EXISTS (SELECT 1 FROM bus.deliveries d WHERE d.message_id = m.id) LIMIT $2)`, []any{r.Acked.Seconds(), r.Limit}},
	} {
		tag, err := b.Pool.Exec(ctx, q.sql, q.args...)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
	}
	return total, nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

var _ Bus = (*PgBus)(nil)
