package pg

import (
	"context"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/graphstore/internal/kernel"
	"github.com/genuinebnt/flowforge/oam"
)

// effectCoveringRun marks graph.observations rows that record a completed
// covering run (SetStale idempotency) rather than an observation.
const effectCoveringRun = "ff:covering_run"

// SetStale implements graphstore.Store: it applies a covering run to every
// entity (of the requested asset types) and edge the tool has reported,
// in one transaction.
func (s *Store) SetStale(ctx context.Context, c graphstore.Caller, req graphstore.SetStaleRequest) (graphstore.SetStaleResult, error) {
	tenant, err := graphstore.TenantOf(c)
	if err != nil {
		return graphstore.SetStaleResult{}, err
	}
	program, run, tool, err := graphstore.ValidateSetStale(req)
	if err != nil {
		return graphstore.SetStaleResult{}, err
	}
	n := req.StaleAfterRuns
	if n <= 0 {
		n = s.cfg.StaleAfterRuns
	}
	at := s.cfg.Now()
	var out graphstore.SetStaleResult
	err = s.tx(ctx, tenant, txWrite, func(tx pgx.Tx) error {
		out = graphstore.SetStaleResult{}
		t, p := tenant.UUID.String(), program.UUID.String()
		tag, err := tx.Exec(ctx, insertObservation, graphstore.CoveringRunUUID(tenant.Text, program.Text, tool, run.Text).String(),
			t, p, run.UUID.String(), effectCoveringRun, tool, "", at, float32(0), nil, 0, 0, at)
		if err != nil {
			return graphstore.Internal("record covering run", err)
		}
		if tag.RowsAffected() == 0 {
			out.Duplicate = true
			return nil
		}
		st, err := loadRunStates(ctx, tx, tenant, program, tool, req.AssetTypes)
		if err != nil {
			return err
		}
		ne, nd, evs := kernel.CoverRun(st, tenant.Text, program.Text, tool, run.Text, nil, n, at)
		if err := writeRunStates(ctx, tx, tenant, tool, st); err != nil {
			return err
		}
		seqs := map[string]int64{}
		if len(evs) > 0 {
			b := &pgx.Batch{}
			b.Queue(lockProgram, lockKey(tenant, program))
			if err := queueEvents(b, t, p, evs); err != nil {
				return err
			}
			br := tx.SendBatch(ctx, b)
			_, err := br.Exec()
			if err == nil {
				err = eachRow(br, func(rows pgx.Rows) error {
					var (
						id  string
						seq int64
					)
					if err := rows.Scan(&id, &seq); err != nil {
						return err
					}
					pid, err := publicOf(core.PrefixChange, &id)
					seqs[pid] = seq
					return err
				})
			}
			if cerr := br.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return graphstore.Internal("insert change events", err)
			}
		}
		var ids []string
		for _, ev := range evs {
			if ev.EntityID != "" {
				ids = append(ids, ev.EntityID)
			}
		}
		snaps, err := readEntities(ctx, tx, tenant, &program, ids)
		if err != nil {
			return err
		}
		out = graphstore.SetStaleResult{EntitiesMarked: ne, EdgesMarked: nd}
		for _, ev := range evs {
			c := graphstore.ChangeEvent{ID: ev.ID, Seq: seqs[ev.ID], TenantID: tenant.Text, ProgramID: program.Text, Type: ev.Type,
				EntityID: ev.EntityID, EdgeID: ev.EdgeID, RunID: ev.RunID, At: ev.At}
			if e, ok := snaps[ev.EntityID]; ok {
				c.Entity = &e
			}
			out.Changes = append(out.Changes, c)
		}
		return nil
	})
	if err != nil {
		return graphstore.SetStaleResult{}, err
	}
	if len(out.Changes) > 0 {
		s.notify.Broadcast()
	}
	return out, nil
}

// loadRunStates loads (and locks) the state and run tags of every record
// the tool has reported: the only parts of a record a covering run reads.
func loadRunStates(ctx context.Context, tx pgx.Tx, tenant, program graphstore.ID, tool string, assetTypes []string) (*kernel.State, error) {
	st := kernel.NewState()
	t, p := tenant.UUID.String(), program.UUID.String()
	var types []string
	if len(assetTypes) > 0 {
		types = assetTypes
	}
	b := &pgx.Batch{}
	b.Queue(`SELECT e.id::text, e.asset_type, e.state FROM graph.entities e
		WHERE e.tenant_id = $1 AND e.program_id = $2 AND ($4::text[] IS NULL OR e.asset_type = ANY($4))
		AND EXISTS (SELECT 1 FROM graph.entity_tags t WHERE t.entity_id = e.id AND t.ptype = '`+kernel.PTypeRun+`' AND t.name = $3)
		ORDER BY e.id FOR UPDATE OF e`, t, p, tool, types)
	b.Queue(`SELECT e.id::text, e.state FROM graph.edges e
		WHERE e.tenant_id = $1 AND e.program_id = $2
		AND EXISTS (SELECT 1 FROM graph.edge_tags t WHERE t.edge_id = e.id AND t.ptype = '`+kernel.PTypeRun+`' AND t.name = $3)
		ORDER BY e.id FOR UPDATE OF e`, t, p, tool)
	br := tx.SendBatch(ctx, b)
	err := eachRow(br, func(rows pgx.Rows) error {
		var id, typ, state string
		if err := rows.Scan(&id, &typ, &state); err != nil {
			return err
		}
		pid, err := publicOf(core.PrefixEntity, &id)
		e := &kernel.Entity{AssetType: typ}
		e.ID, e.Tenant, e.Program, e.State, e.Runs = pid, tenant.Text, program.Text, oam.State(state), map[string]*kernel.RunState{}
		st.Entities[pid] = e
		return err
	})
	if err == nil {
		err = eachRow(br, func(rows pgx.Rows) error {
			var id, state string
			if err := rows.Scan(&id, &state); err != nil {
				return err
			}
			pid, err := publicOf(core.PrefixEdge, &id)
			e := &kernel.Edge{}
			e.ID, e.Tenant, e.Program, e.State, e.Runs = pid, tenant.Text, program.Text, oam.State(state), map[string]*kernel.RunState{}
			st.Edges[pid] = e
			return err
		})
	}
	if cerr := br.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, graphstore.Internal("load covered records", err)
	}
	if err := loadRuns(ctx, tx, tenant, graphstore.TargetEntity, slices.Sorted(maps.Keys(st.Entities)), func(id string) *kernel.Record {
		return &st.Entities[id].Record
	}); err != nil {
		return nil, err
	}
	if err := loadRuns(ctx, tx, tenant, graphstore.TargetEdge, slices.Sorted(maps.Keys(st.Edges)), func(id string) *kernel.Record {
		return &st.Edges[id].Record
	}); err != nil {
		return nil, err
	}
	return st, nil
}

// loadRuns loads the run rows and the liveness row of the records (the
// liveness row's raw content is kept in liveRaw for writeRunStates).
func loadRuns(ctx context.Context, tx pgx.Tx, tenant graphstore.ID, kind graphstore.TargetKind, ids []string, recOf func(string) *kernel.Record) error {
	if len(ids) == 0 {
		return nil
	}
	prefix := core.PrefixEntity
	if kind == graphstore.TargetEdge {
		prefix = core.PrefixEdge
	}
	uuids, err := uuidsOf(prefix, ids)
	if err != nil {
		return graphstore.Internal("ids", err)
	}
	table, owner := tagTable(kind)
	rows, err := tx.Query(ctx, `SELECT `+owner+`::text, ptype, name, content FROM `+table+`
		WHERE tenant_id = $1 AND ptype IN ('`+kernel.PTypeRun+`', '`+kernel.PTypeLive+`') AND `+owner+` = ANY($2::text[]::uuid[])`, tenant.UUID.String(), uuids)
	if err != nil {
		return graphstore.Internal("load run states", err)
	}
	defer rows.Close()
	hasLive := map[string]bool{}
	for rows.Next() {
		var oid string
		var t kernel.StoredTag
		if err := rows.Scan(&oid, &t.PType, &t.Name, &t.Content); err != nil {
			return graphstore.Internal("load run states", err)
		}
		pid, err := publicOf(prefix, &oid)
		if err != nil {
			return graphstore.Internal("ids", err)
		}
		r := recOf(pid)
		if t.PType == kernel.PTypeLive {
			if err := r.DecodeLive(t); err != nil {
				return graphstore.Internal("decode live state", err)
			}
			r.LiveRaw, hasLive[pid] = t.Content, true
			continue
		}
		rs, err := kernel.DecodeRunTag(t)
		if err != nil {
			return graphstore.Internal("decode run state", err)
		}
		r.Runs[t.Name] = &rs
	}
	if err := rows.Err(); err != nil {
		return graphstore.Internal("load run states", err)
	}
	for _, id := range ids {
		if r := recOf(id); !hasLive[id] {
			r.ClassicStale = r.State == oam.StateStale // stored before 0030
		}
	}
	return nil
}

// writeRunStates writes the tool's run tag and the state of every record
// the covering run touched.
func writeRunStates(ctx context.Context, tx pgx.Tx, tenant graphstore.ID, tool string, st *kernel.State) error {
	t := tenant.UUID.String()
	b := &pgx.Batch{}
	type acc struct {
		ids, contents, states []string
		missed                []int32
		live                  tagArrays
	}
	var ents, edges acc
	add := func(a *acc, prefix core.Prefix, id string, r *kernel.Record) error {
		tag, ok, err := r.RunTag(tool)
		if err != nil || !ok {
			return err
		}
		u, err := uuidOf(prefix, id)
		if err != nil {
			return err
		}
		a.ids, a.contents, a.states = append(a.ids, u), append(a.contents, string(tag.Content)), append(a.states, string(r.State))
		a.missed = append(a.missed, int32(r.MinMissed()))
		live, err := r.MergedLiveTag()
		if err != nil {
			return err
		}
		kind := graphstore.TargetEntity
		if prefix == core.PrefixEdge {
			kind = graphstore.TargetEdge
		}
		a.live.add(tenant.Text, kind, id, u, live)
		return nil
	}
	for _, id := range slices.Sorted(maps.Keys(st.Entities)) {
		if err := add(&ents, core.PrefixEntity, id, &st.Entities[id].Record); err != nil {
			return graphstore.Internal("encode run state", err)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(st.Edges)) {
		if err := add(&edges, core.PrefixEdge, id, &st.Edges[id].Record); err != nil {
			return graphstore.Internal("encode run state", err)
		}
	}
	for _, x := range []struct {
		kind graphstore.TargetKind
		a    acc
	}{{graphstore.TargetEntity, ents}, {graphstore.TargetEdge, edges}} {
		if len(x.a.ids) == 0 {
			continue
		}
		x.a.live.queue(b, t, x.kind)
		table, owner := tagTable(x.kind)
		b.Queue(`UPDATE `+table+` t SET content = u.content::jsonb
			FROM unnest($2::text[], $3::text[]) AS u(owner, content)
			WHERE t.tenant_id = $1 AND t.`+owner+` = u.owner::uuid AND t.ptype = '`+kernel.PTypeRun+`' AND t.name = $4`,
			t, x.a.ids, x.a.contents, tool)
		if x.kind == graphstore.TargetEntity {
			b.Queue(`UPDATE graph.entities e SET state = u.state, missed_runs = u.missed
				FROM unnest($2::text[], $3::text[], $4::int[]) AS u(id, state, missed)
				WHERE e.tenant_id = $1 AND e.id = u.id::uuid`, t, x.a.ids, x.a.states, x.a.missed)
		} else {
			b.Queue(`UPDATE graph.edges e SET state = u.state
				FROM unnest($2::text[], $3::text[]) AS u(id, state)
				WHERE e.tenant_id = $1 AND e.id = u.id::uuid`, t, x.a.ids, x.a.states)
		}
	}
	if b.Len() == 0 {
		return nil
	}
	if err := tx.SendBatch(ctx, b).Close(); err != nil {
		return graphstore.Internal("write run states", err)
	}
	return nil
}
