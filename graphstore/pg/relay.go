package pg

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/graphstore"
)

// Relay publishes graph.change_events rows, which double as the graph
// outbox (spec 10.5, 23.7), and marks them published. Publish is called
// first and the rows are marked in the same transaction afterwards, so
// a crash between the two re-publishes them: delivery is at least once
// (duplicates possible, never loss); consumers deduplicate by event id.
//
// The relay spans tenants. Its scan of graph.change_events relies on the
// svc_ff_graph policy of migration 0018 (or a superuser); the entity
// snapshots it attaches are read with app.tenant_id set to each event's
// tenant, so ff_graph stays subject to row-level security on every other
// graph table (migrations/OPEN_QUESTIONS.md 12). Concurrent relays skip each other's rows
// (FOR UPDATE SKIP LOCKED).
type Relay struct {
	DB DB
	// Publish delivers one batch, in seq order; an error leaves the batch
	// unpublished.
	Publish func(ctx context.Context, events []graphstore.ChangeEvent) error
	// BatchSize defaults to 500.
	BatchSize int
	// Clock stamps published_at; nil means core.SystemClock.
	Clock core.Clock
}

// RunOnce publishes and marks at most one batch and returns its size
// (0 when nothing was pending).
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	n := r.BatchSize
	if n <= 0 {
		n = 500
	}
	clock := r.Clock
	if clock == nil {
		clock = &core.SystemClock{}
	}
	tx, err := r.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, graphstore.Internal("relay begin", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx, `SELECT `+changeCols+` FROM graph.change_events c
		WHERE c.published_at IS NULL ORDER BY c.seq LIMIT $1 FOR UPDATE SKIP LOCKED`, n)
	if err != nil {
		return 0, graphstore.Internal("relay read", err)
	}
	var evs []graphstore.ChangeEvent
	var ids []string
	for rows.Next() {
		ev, err := scanChange(rows)
		if err != nil {
			rows.Close()
			return 0, graphstore.Internal("relay scan", err)
		}
		u, _ := uuidOf(core.PrefixChange, ev.ID)
		evs, ids = append(evs, ev), append(ids, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, graphstore.Internal("relay read", err)
	}
	if len(evs) == 0 {
		return 0, nil
	}
	if err := r.snapshots(ctx, tx, evs); err != nil {
		return 0, err
	}
	if err := r.Publish(ctx, evs); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE graph.change_events SET published_at = $2 WHERE id = ANY($1::text[]::uuid[])`,
		ids, graphstore.StoreTime(clock.Now())); err != nil {
		return 0, graphstore.Internal("relay mark", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, graphstore.Internal("relay commit", err)
	}
	return len(evs), nil
}

// snapshots attaches the current entity snapshot to each entity event.
func (r *Relay) snapshots(ctx context.Context, tx pgx.Tx, evs []graphstore.ChangeEvent) error {
	byTenant := map[string][]string{}
	for _, ev := range evs {
		if ev.EntityID != "" {
			byTenant[ev.TenantID] = append(byTenant[ev.TenantID], ev.EntityID)
		}
	}
	snaps := map[string]graphstore.Entity{}
	for tenant, ids := range byTenant {
		tid, err := graphstore.ParseID(core.PrefixTenant, "tenant_id", tenant)
		if err != nil {
			return graphstore.Internal("relay tenant", err)
		}
		// set_config(..., true) is transaction-local and may change within
		// the transaction; the last tenant's setting ends with it.
		if _, err := tx.Exec(ctx, setTenant, tid.UUID.String()); err != nil {
			return graphstore.Internal("relay set tenant", err)
		}
		m, err := readEntities(ctx, tx, tid, nil, ids)
		if err != nil {
			return err
		}
		for id, e := range m {
			snaps[id] = e
		}
	}
	for i := range evs {
		if e, ok := snaps[evs[i].EntityID]; ok {
			evs[i].Entity = &e
		}
	}
	return nil
}
