package pg

import (
	"context"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/graphstore/internal/kernel"
	"github.com/genuinebnt/flowforge/oam"
)

const changeCols = `c.id::text, c.seq, c.tenant_id::text, c.program_id::text, c.type, c.entity_id::text, c.edge_id::text,
	c.run_id::text, c.patch, c.at, c.tag_type, c.tag_name, c.base, c.cause, c.baseline`

func scanChange(rows scanner) (graphstore.ChangeEvent, error) {
	var (
		ev                       graphstore.ChangeEvent
		id, tenant, program, typ string
		entity, edge, run        *string
		patch, base              []byte
		at                       time.Time
		tagType, tagName, cause  *string
	)
	if err := rows.Scan(&id, &ev.Seq, &tenant, &program, &typ, &entity, &edge, &run, &patch, &at,
		&tagType, &tagName, &base, &cause, &ev.Baseline); err != nil {
		return ev, err
	}
	if tagType != nil {
		ev.TagType = *tagType
	}
	if tagName != nil {
		ev.TagName = *tagName
	}
	if cause != nil {
		ev.Cause = *cause
	}
	if len(base) > 0 {
		b, err := kernel.DecodeObject(base)
		if err != nil {
			return ev, err
		}
		ev.Base = b
	}
	var err error
	for _, f := range []struct {
		dst *string
		p   core.Prefix
		src *string
	}{
		{&ev.ID, core.PrefixChange, &id}, {&ev.TenantID, core.PrefixTenant, &tenant}, {&ev.ProgramID, core.PrefixProgram, &program},
		{&ev.EntityID, core.PrefixEntity, entity}, {&ev.EdgeID, core.PrefixEdge, edge}, {&ev.RunID, core.PrefixRun, run},
	} {
		if *f.dst, err = publicOf(f.p, f.src); err != nil {
			return ev, err
		}
	}
	ev.Type, ev.At = oam.ChangeType(typ), at.UTC()
	if ev.Type == oam.ChangeContentChanged {
		ev.ContentChange, err = kernel.DecodeContentChange(patch)
		return ev, err
	}
	ev.Patch, err = kernel.DecodePatch(patch)
	return ev, err
}

// queryChanges runs a change-event query and attaches entity snapshots.
func queryChanges(ctx context.Context, q querier, tenant graphstore.ID, sql string, args ...any) ([]graphstore.ChangeEvent, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, graphstore.Internal("query changes", err)
	}
	var out []graphstore.ChangeEvent
	for rows.Next() {
		ev, err := scanChange(rows)
		if err != nil {
			rows.Close()
			return nil, graphstore.Internal("scan change", err)
		}
		out = append(out, ev)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, graphstore.Internal("query changes", err)
	}
	var ids []string
	for _, ev := range out {
		if ev.EntityID != "" {
			ids = append(ids, ev.EntityID)
		}
	}
	snaps, err := readEntities(ctx, q, tenant, nil, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if e, ok := snaps[out[i].EntityID]; ok {
			out[i].Entity = &e
		}
	}
	return out, nil
}

// ReadChanges implements graphstore.Store.
func (s *Store) ReadChanges(ctx context.Context, c graphstore.Caller, req graphstore.ChangesRequest) ([]graphstore.ChangeEvent, error) {
	tenant, err := graphstore.TenantOf(c)
	if err != nil {
		return nil, err
	}
	prog, limit, err := graphstore.ValidateChanges(req)
	if err != nil {
		return nil, err
	}
	types := make([]string, len(req.Types))
	for i, t := range req.Types {
		types[i] = string(t)
	}
	var out []graphstore.ChangeEvent
	err = s.tx(ctx, tenant, txRead, func(tx pgx.Tx) error {
		out, err = queryChanges(ctx, tx, tenant, `SELECT `+changeCols+` FROM graph.change_events c
			WHERE c.tenant_id = $1 AND c.program_id = $2 AND c.seq > $3 AND ($4::text[] IS NULL OR c.type = ANY($4))
			ORDER BY c.seq LIMIT $5`, tenant.UUID.String(), prog.UUID.String(), req.AfterSeq, nilIfEmpty(types), limit)
		return err
	})
	return out, err
}

// WatchChanges implements graphstore.Store. It wakes on commits made
// through this Store and polls every PollInterval for other writers.
func (s *Store) WatchChanges(ctx context.Context, c graphstore.Caller, req graphstore.WatchRequest, fn func(graphstore.ChangeEvent) error) error {
	cr := graphstore.ChangesRequest{ProgramID: req.ProgramID, AfterSeq: req.AfterSeq, Types: req.Types, Limit: graphstore.MaxPageSize}
	if _, _, err := graphstore.ValidateChanges(cr); err != nil {
		return err
	}
	if _, err := graphstore.TenantOf(c); err != nil {
		return err
	}
	read := func(ctx context.Context, after int64) ([]graphstore.ChangeEvent, error) {
		cr.AfterSeq = after
		return s.ReadChanges(ctx, c, cr)
	}
	return graphstore.WatchLoop(ctx, req.AfterSeq, read, &s.notify, s.cfg.PollInterval, fn)
}

// Diff implements graphstore.Store.
func (s *Store) Diff(ctx context.Context, c graphstore.Caller, req graphstore.DiffRequest) (graphstore.DiffResult, error) {
	tenant, err := graphstore.TenantOf(c)
	if err != nil {
		return graphstore.DiffResult{}, err
	}
	q, err := graphstore.ValidateDiff(req)
	if err != nil {
		return graphstore.DiffResult{}, err
	}
	t, p := tenant.UUID.String(), q.Program.UUID.String()
	var res graphstore.DiffResult
	err = s.tx(ctx, tenant, txRead, func(tx pgx.Tx) error {
		lastOf := func(run graphstore.ID) (int64, error) {
			var seq *int64
			if err := tx.QueryRow(ctx, `SELECT max(seq) FROM graph.change_events WHERE tenant_id = $1 AND program_id = $2 AND run_id = $3::uuid`,
				t, p, run.UUID.String()).Scan(&seq); err != nil {
				return 0, graphstore.Internal("diff run", err)
			}
			if seq == nil {
				return 0, graphstore.NotFound("run", run.Text)
			}
			return *seq, nil
		}
		var evs []graphstore.ChangeEvent
		var err error
		if q.RunA.IsZero() {
			evs, err = queryChanges(ctx, tx, tenant, `SELECT `+changeCols+` FROM graph.change_events c
				WHERE c.tenant_id = $1 AND c.program_id = $2 AND c.at >= $3 AND c.seq > $4 ORDER BY c.seq LIMIT $5`,
				t, p, q.Since, q.After, q.Size+1)
		} else {
			lo, err := lastOf(q.RunA)
			if err != nil {
				return err
			}
			hi := int64(math.MaxInt64)
			if !q.RunB.IsZero() {
				if hi, err = lastOf(q.RunB); err != nil {
					return err
				}
			}
			evs, err = queryChanges(ctx, tx, tenant, `SELECT `+changeCols+` FROM graph.change_events c
				WHERE c.tenant_id = $1 AND c.program_id = $2 AND c.seq > $3 AND c.seq <= $4 ORDER BY c.seq LIMIT $5`,
				t, p, max(lo, q.After), hi, q.Size+1)
			if err != nil {
				return err
			}
		}
		if err != nil {
			return err
		}
		if len(evs) > q.Size {
			evs = evs[:q.Size]
			res.NextPageToken = graphstore.EncodeSeqToken(evs[q.Size-1].Seq)
		}
		res.Changes = evs
		return nil
	})
	return res, err
}
