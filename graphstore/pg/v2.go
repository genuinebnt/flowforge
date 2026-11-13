package pg

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/oam"
)

// events is the AsOf EventSource over graph.change_events inside a read
// transaction (served by the change_events_entity_seq / _edge_seq
// indexes of migration 0030).
type events struct {
	q      querier
	tenant graphstore.ID
}

func (s events) load(ctx context.Context, col string, p core.Prefix, ids []string, at int64) (map[string][]graphstore.ChangeEvent, error) {
	out := map[string][]graphstore.ChangeEvent{}
	if len(ids) == 0 {
		return out, nil
	}
	uuids, err := uuidsOf(p, ids)
	if err != nil {
		return nil, graphstore.Internal("ids", err)
	}
	rows, err := s.q.Query(ctx, `SELECT `+changeCols+` FROM graph.change_events c
		WHERE c.`+col+` = ANY($2::text[]::uuid[]) AND c.seq <= $3 AND c.tenant_id = $1 ORDER BY c.seq`,
		s.tenant.UUID.String(), uuids, at)
	if err != nil {
		return nil, graphstore.Internal("read events", err)
	}
	defer rows.Close()
	for rows.Next() {
		ev, err := scanChange(rows)
		if err != nil {
			return nil, graphstore.Internal("scan change", err)
		}
		id := ev.EntityID
		if p == core.PrefixEdge {
			id = ev.EdgeID
		}
		out[id] = append(out[id], ev)
	}
	return out, rows.Err()
}

func (s events) EntityEvents(ctx context.Context, ids []string, at int64) (map[string][]graphstore.ChangeEvent, error) {
	return s.load(ctx, "entity_id", core.PrefixEntity, ids, at)
}

func (s events) EdgeEvents(ctx context.Context, ids []string, at int64) (map[string][]graphstore.ChangeEvent, error) {
	return s.load(ctx, "edge_id", core.PrefixEdge, ids, at)
}

// GetEntityAsOf implements graphstore.Store.
func (s *Store) GetEntityAsOf(ctx context.Context, c graphstore.Caller, req graphstore.GetEntityAsOfRequest) (graphstore.Entity, error) {
	tenant, err := graphstore.TenantOf(c)
	if err != nil {
		return graphstore.Entity{}, err
	}
	if err := graphstore.ValidateAsOfSeq(req.Seq); err != nil {
		return graphstore.Entity{}, err
	}
	var out graphstore.Entity
	err = s.tx(ctx, tenant, txRead, func(tx pgx.Tx) error {
		cur, err := entityOf(ctx, tx, tenant, "entity_id", req.EntityID)
		if err != nil {
			return err
		}
		out, err = graphstore.GetAsOf(ctx, events{tx, tenant}, cur, req.Seq, req.IncludeTags)
		return err
	})
	return out, err
}

// NeighborsAsOf implements graphstore.Store.
func (s *Store) NeighborsAsOf(ctx context.Context, c graphstore.Caller, req graphstore.NeighborsAsOfRequest) (graphstore.Subgraph, error) {
	tenant, err := graphstore.TenantOf(c)
	if err != nil {
		return graphstore.Subgraph{}, err
	}
	if req, err = graphstore.ValidateNeighborsAsOf(req); err != nil {
		return graphstore.Subgraph{}, err
	}
	var out graphstore.Subgraph
	err = s.tx(ctx, tenant, txRead, func(tx pgx.Tx) error {
		cur, err := entityOf(ctx, tx, tenant, "entity_id", req.EntityID)
		if err != nil {
			return err
		}
		ev := events{tx, tenant}
		start, err := graphstore.GetAsOf(ctx, ev, cur, req.Seq, false)
		if err != nil {
			return err
		}
		r, err := s.readerFor(ctx, tx, tenant, cur)
		if err != nil {
			return err
		}
		out, err = graphstore.TraverseNeighbors(ctx, graphstore.AsOfReader{Cur: r, Ev: ev, At: req.Seq}, start, req.NeighborsRequest)
		return err
	})
	return out, err
}

// Commonness implements graphstore.Store.
func (s *Store) Commonness(ctx context.Context, c graphstore.Caller, req graphstore.CommonnessRequest) (graphstore.Commonness, error) {
	tenant, err := graphstore.TenantOf(c)
	if err != nil {
		return graphstore.Commonness{}, err
	}
	var out graphstore.Commonness
	err = s.tx(ctx, tenant, txRead, func(tx pgx.Tx) error {
		e, err := entityOf(ctx, tx, tenant, "entity_id", req.EntityID)
		if err != nil {
			return err
		}
		r, err := s.readerFor(ctx, tx, tenant, e)
		if err != nil {
			return err
		}
		out, err = graphstore.ComputeCommonness(ctx, r, e)
		return err
	})
	return out, err
}

// RecordContentChange implements graphstore.Store: the event is inserted
// under the program lock (seq order, as every writer) with ON CONFLICT DO
// NOTHING on its derived id, and a repeat returns the stored event.
func (s *Store) RecordContentChange(ctx context.Context, c graphstore.Caller, req graphstore.ContentChange) (graphstore.ChangeEvent, error) {
	tenant, err := graphstore.TenantOf(c)
	if err != nil {
		return graphstore.ChangeEvent{}, err
	}
	run, err := graphstore.ParseOptionalID(core.PrefixRun, "run_id", req.RunID)
	if err != nil {
		return graphstore.ChangeEvent{}, err
	}
	patch, err := req.Patch.Normalize()
	if err != nil {
		return graphstore.ChangeEvent{}, graphstore.Classify(err)
	}
	pb, err := json.Marshal(patch)
	if err != nil {
		return graphstore.ChangeEvent{}, graphstore.Internal("encode patch", err)
	}
	at := graphstore.StoreTime(req.At)
	if req.At.IsZero() {
		at = s.cfg.Now()
	}
	var out graphstore.ChangeEvent
	inserted := false
	err = s.tx(ctx, tenant, txWrite, func(tx pgx.Tx) error {
		inserted = false
		e, err := entityOf(ctx, tx, tenant, "entity_id", req.EntityID)
		if err != nil {
			return err
		}
		prog, err := graphstore.ParseID(core.PrefixProgram, "program_id", e.ProgramID)
		if err != nil {
			return graphstore.Internal("program id", err)
		}
		id := oam.ContentChangeUUID(tenant.Text, prog.Text, e.ID, patch)
		eu, _ := uuidOf(core.PrefixEntity, e.ID)
		var runU *string
		if !run.IsZero() {
			u := run.UUID.String()
			runU = &u
		}
		cause := nullText(patch.Cause)
		if patch.Cause == oam.CauseTarget {
			cause = nil
		}
		if _, err := tx.Exec(ctx, lockProgram, lockKey(tenant, prog)); err != nil {
			return graphstore.Internal("lock program", err)
		}
		tag, err := tx.Exec(ctx, `INSERT INTO graph.change_events (id, tenant_id, program_id, type, entity_id, run_id, patch, at, cause, baseline)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5::uuid, $6::uuid, $7::jsonb, $8, $9, $10) ON CONFLICT (id) DO NOTHING`,
			id.String(), tenant.UUID.String(), prog.UUID.String(), string(oam.ChangeContentChanged), eu, runU, string(pb), at, cause, patch.Baseline)
		if err != nil {
			return graphstore.Internal("insert content change", err)
		}
		inserted = tag.RowsAffected() == 1
		evs, err := queryChanges(ctx, tx, tenant, `SELECT `+changeCols+` FROM graph.change_events c WHERE c.id = $1::uuid AND c.tenant_id = $2`,
			id.String(), tenant.UUID.String())
		if err != nil {
			return err
		}
		if len(evs) != 1 {
			return graphstore.Internal("content change", errString("event missing after insert"))
		}
		out = evs[0]
		return nil
	})
	if err == nil && inserted {
		s.notify.Broadcast()
	}
	return out, err
}
