package pg

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/genuinebnt/flowforge/graphstore"
)

// DB is the subset of a connection pool the store uses; *pgxpool.Pool
// satisfies it.
type DB interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Config configures the Postgres store.
type Config struct {
	graphstore.Config
	// PollInterval is how often WatchChanges re-reads when it has seen no
	// local commit (writers in other processes); default 500ms.
	PollInterval time.Duration
	// MaxRetries bounds retries of a transaction that hit a serialization
	// failure or deadlock; default 3.
	MaxRetries int
}

// Store is the Postgres graph store. It is safe for concurrent use.
type Store struct {
	db     DB
	cfg    Config
	notify graphstore.Notifier
	close  func()
}

var _ graphstore.Store = (*Store)(nil)

// New returns a store over db, whose schema must be migrated
// (migrations.Apply).
func New(db DB, cfg Config) *Store {
	cfg.Config = cfg.Config.WithDefaults()
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 500 * time.Millisecond
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 3
	}
	return &Store{db: db, cfg: cfg, close: func() {}}
}

// Open connects a pgxpool to dsn and returns a store over it; Close
// releases the pool.
func Open(ctx context.Context, dsn string, cfg Config) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, graphstore.Internal("connect", err)
	}
	s := New(pool, cfg)
	s.close = pool.Close
	return s, nil
}

// Close releases the pool opened by Open (a no-op for New).
func (s *Store) Close() { s.close() }

// txMode selects the transaction kind.
type txMode int

const (
	txWrite txMode = iota
	// txRead is a read-only repeatable-read transaction (one snapshot).
	txRead
	// txWriteDeferTenant is txWrite where fn itself sends setTenant as
	// its first statement (saving a round trip).
	txWriteDeferTenant
)

// setTenant sets app.tenant_id for the transaction (spec 10.7 RLS). It
// also forces custom plans: the store's queries select a handful of ids
// with = ANY($n), and a generic plan cached while a table was small would
// keep scanning the (tenant, program) index as the graph grows.
const setTenant = `SELECT set_config('app.tenant_id', $1, true), set_config('plan_cache_mode', 'force_custom_plan', true)`

// tx runs fn in a transaction with app.tenant_id set to tenant (spec
// 10.7), retrying serialization failures and deadlocks.
func (s *Store) tx(ctx context.Context, tenant graphstore.ID, mode txMode, fn func(pgx.Tx) error) error {
	var err error
	for attempt := 0; attempt <= s.cfg.MaxRetries; attempt++ {
		if err = s.tryTx(ctx, tenant, mode, fn); !retryable(err) {
			return err
		}
	}
	return err
}

func (s *Store) tryTx(ctx context.Context, tenant graphstore.ID, mode txMode, fn func(pgx.Tx) error) (err error) {
	opts := pgx.TxOptions{}
	if mode == txRead {
		opts = pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	}
	tx, err := s.db.BeginTx(ctx, opts)
	if err != nil {
		return graphstore.Internal("begin", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if mode != txWriteDeferTenant {
		if _, err = tx.Exec(ctx, setTenant, tenant.UUID.String()); err != nil {
			return graphstore.Internal("set tenant", err)
		}
	}
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return graphstore.Internal("commit", err)
	}
	return nil
}

func retryable(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && (pe.Code == "40001" || pe.Code == "40P01")
}
