package pgstore

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/genuinebnt/flowforge/eventstore"
)

// DB is the subset of a connection pool the store uses. *pgxpool.Pool
// satisfies it; Open uses a small built-in pool instead because pgxpool's
// dependency (puddle) is not part of this module.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// pool is a minimal bounded connection pool over pgx.Conn. A connection
// that is closed or left inside a transaction is discarded on release.
type pool struct {
	cfg *pgx.ConnConfig
	sem chan struct{}

	mu     sync.Mutex
	idle   []*pgx.Conn
	closed bool
}

func newPool(cfg *pgx.ConnConfig, maxConns int) *pool {
	return &pool{cfg: cfg, sem: make(chan struct{}, maxConns)}
}

func (p *pool) acquire(ctx context.Context) (*pgx.Conn, error) {
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		<-p.sem
		return nil, eventstore.ErrClosed
	}
	if n := len(p.idle); n > 0 {
		c := p.idle[n-1]
		p.idle = p.idle[:n-1]
		p.mu.Unlock()
		return c, nil
	}
	p.mu.Unlock()
	c, err := pgx.ConnectConfig(ctx, p.cfg)
	if err != nil {
		<-p.sem
		return nil, err
	}
	return c, nil
}

func (p *pool) release(c *pgx.Conn) {
	defer func() { <-p.sem }()
	if c.IsClosed() || c.PgConn().TxStatus() != 'I' || c.PgConn().IsBusy() {
		_ = c.Close(context.Background())
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = c.Close(context.Background())
		return
	}
	p.idle = append(p.idle, c)
	p.mu.Unlock()
}

func (p *pool) close() {
	p.mu.Lock()
	idle := p.idle
	p.idle, p.closed = nil, true
	p.mu.Unlock()
	for _, c := range idle {
		_ = c.Close(context.Background())
	}
}

// Begin implements DB.
func (p *pool) Begin(ctx context.Context) (pgx.Tx, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := c.Begin(ctx)
	if err != nil {
		p.release(c)
		return nil, err
	}
	return &poolTx{Tx: tx, release: func() { p.release(c) }}, nil
}

// Exec implements DB.
func (p *pool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	defer p.release(c)
	return c.Exec(ctx, sql, args...)
}

// Query implements DB. The connection returns to the pool when the rows
// are closed.
func (p *pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := c.Query(ctx, sql, args...)
	if err != nil {
		p.release(c)
		return nil, err
	}
	return &poolRows{Rows: rows, release: func() { p.release(c) }}, nil
}

// QueryRow implements DB.
func (p *pool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	rows, err := p.Query(ctx, sql, args...)
	return &poolRow{rows: rows, err: err}
}

type poolTx struct {
	pgx.Tx
	once    sync.Once
	release func()
}

func (t *poolTx) Commit(ctx context.Context) error {
	err := t.Tx.Commit(ctx)
	t.once.Do(t.release)
	return err
}

func (t *poolTx) Rollback(ctx context.Context) error {
	err := t.Tx.Rollback(ctx)
	t.once.Do(t.release)
	return err
}

type poolRows struct {
	pgx.Rows
	once    sync.Once
	release func()
}

func (r *poolRows) Close() {
	r.Rows.Close()
	r.once.Do(r.release)
}

type poolRow struct {
	rows pgx.Rows
	err  error
}

// Scan mirrors pgx's Row semantics: pgx.ErrNoRows when the query returned
// nothing, and the rows are always closed.
func (r *poolRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	defer r.rows.Close()
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return err
		}
		return pgx.ErrNoRows
	}
	if err := r.rows.Scan(dest...); err != nil {
		return err
	}
	r.rows.Close()
	return r.rows.Err()
}
