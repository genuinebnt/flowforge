package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/graphstore/internal/kernel"
	"github.com/genuinebnt/flowforge/oam"
)

// Locking protocol. Every writer takes, in this order:
//
//  1. record locks: one transaction-scoped advisory lock per entity and
//     edge id it may create or change, entity ids then edge ids, each
//     sorted, so concurrent writers of the same records (rows that may not
//     exist yet) serialize and never deadlock;
//  2. row locks (SELECT ... FOR UPDATE, entities then edges, by id);
//  3. the program lock, only just before inserting its change events and
//     held until commit, so the change-event seqs of one program commit
//     in seq order and a snapshot's watermark never skips an event that
//     was still uncommitted (spec 21.4).
//
// Writers of different records of a program therefore run concurrently
// except for their short commit tail.
const (
	lockProgram = `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`
	lockRecords = `SELECT count(pg_advisory_xact_lock(hashtextextended(k, 0))) FROM unnest($1::text[]) WITH ORDINALITY AS u(k, n)`
)

func lockKey(tenant, program graphstore.ID) string {
	return "graph:" + tenant.UUID.String() + ":" + program.UUID.String()
}

// recordKeys returns the record-lock keys of the ids, in locking order.
func recordKeys(entityIDs, edgeIDs []string) []string {
	keys := make([]string, 0, len(entityIDs)+len(edgeIDs))
	for _, id := range entityIDs {
		keys = append(keys, "graph:rec:"+id)
	}
	for _, id := range edgeIDs {
		keys = append(keys, "graph:rec:"+id)
	}
	return keys
}

const insertObservation = `INSERT INTO graph.observations
	(id, tenant_id, program_id, run_id, effect_path, tool_name, tool_version, observed_at, confidence, raw_ref,
	 entity_count, edge_count, ingested_at)
	VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	ON CONFLICT (id) DO NOTHING`

// Pending references (G-CORE-03). An observation that names ent_/edg_ ids
// its tenant and program do not have yet is stored in
// graph.pending_observations (its graph.observations dedup row is removed
// again) instead of failing. A writer that creates entities or edges marks
// the pending rows naming them ready, in its own transaction while it
// holds the record locks; after its commit it re-ingests the ready rows of
// the program, each in one transaction that deletes the row, so a crash
// re-processes it later rather than losing it. Locks: the pending row
// before the record locks in every writer (a re-sent observation deletes
// its own row first), and the ready marking skips rows locked by someone
// already re-ingesting them, so the two orders never deadlock. Ready rows
// left by a crash are picked up by the next release in that program.
const (
	deletePendingResent = `DELETE FROM graph.pending_observations WHERE tenant_id = $1::uuid AND observation_id = $2::uuid`
	deletePendingReady  = `DELETE FROM graph.pending_observations WHERE tenant_id = $1::uuid AND observation_id = $2::uuid AND ready`
	markPendingReady    = `UPDATE graph.pending_observations SET ready = true
	WHERE (tenant_id, observation_id) IN (SELECT tenant_id, observation_id FROM graph.pending_observations
	  WHERE tenant_id = $1::uuid AND program_id = $2::uuid AND NOT ready AND missing && $3::text[]
	  FOR UPDATE SKIP LOCKED)`
)

// IngestObservation implements graphstore.Store.
func (s *Store) IngestObservation(ctx context.Context, req graphstore.IngestRequest) (graphstore.IngestResult, error) {
	out, ready, err := s.ingest(ctx, req, false)
	if err != nil {
		return graphstore.IngestResult{}, err
	}
	if ready {
		s.releasePending(ctx, req, &out)
	}
	if len(out.Changes) > 0 {
		s.notify.Broadcast()
	}
	return out, nil
}

// ingest applies or buffers one observation. fromPending re-ingests a ready
// pending row (and is a no-op when another writer took it). ready reports
// that the transaction marked pending rows of the program ready.
func (s *Store) ingest(ctx context.Context, req graphstore.IngestRequest, fromPending bool) (graphstore.IngestResult, bool, error) {
	pi, err := graphstore.PrepareIngest(req)
	if err != nil {
		return graphstore.IngestResult{}, false, err
	}
	p, err := kernel.Prepare(pi.Observation)
	if err != nil {
		return graphstore.IngestResult{}, false, graphstore.Classify(err)
	}
	o := pi.Observation
	var out graphstore.IngestResult
	var ready bool
	obsUUID := graphstore.ObservationUUID(pi.Tenant.Text, o.ObservationID).String()
	err = s.tx(ctx, pi.Tenant, txWriteDeferTenant, func(tx pgx.Tx) error {
		out, ready = graphstore.IngestResult{}, false
		entIDs, edgeIDs := p.EntityIDs(), p.EdgeIDs()
		// An authoritative capture may withdraw members of its source's
		// units: read the heads first (no lock yet) so those records are
		// locked and loaded with the rest; Apply reports
		// ErrMissingRecords (and the transaction is retried) if another
		// writer changed the heads in between.
		if moreEdges, moreEnts, err := preloadHeads(ctx, tx, pi.Tenant, p); err != nil {
			return err
		} else if len(moreEdges)+len(moreEnts) > 0 {
			entIDs = sortedUnion(entIDs, moreEnts)
			edgeIDs = sortedUnion(edgeIDs, moreEdges)
		}
		var rawRef *string
		if o.RawRef != "" {
			rawRef = &o.RawRef
		}
		runUUID := core.Nil.String()
		if !pi.Run.IsZero() {
			runUUID = pi.Run.UUID.String()
		}
		delPending := deletePendingResent
		if fromPending {
			delPending = deletePendingReady
		}
		b := &pgx.Batch{}
		b.Queue(setTenant, pi.Tenant.UUID.String())
		b.Queue(delPending, pi.Tenant.UUID.String(), obsUUID)
		b.Queue(lockRecords, recordKeys(entIDs, edgeIDs))
		b.Queue(insertObservation, obsUUID,
			pi.Tenant.UUID.String(), pi.Program.UUID.String(), runUUID, pi.EffectPath, o.Tool.Name, pi.ToolVersion,
			o.ObservedAt, float32(o.Confidence), rawRef, len(o.Entities), len(o.Edges), s.cfg.Now())
		br := tx.SendBatch(ctx, b)
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return graphstore.Internal("set tenant", err)
		}
		del, err := br.Exec()
		if err != nil {
			_ = br.Close()
			return graphstore.Internal("delete pending", err)
		}
		if fromPending && del.RowsAffected() == 0 {
			_ = br.Close()
			out = graphstore.IngestResult{Duplicate: true, RefToEntityID: map[string]string{}}
			return nil // another writer re-ingested it
		}
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return graphstore.Internal("lock records", err)
		}
		tag, err := br.Exec()
		if cerr := br.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return graphstore.Internal("insert observation", err)
		}
		if tag.RowsAffected() == 0 {
			out = graphstore.IngestResult{Duplicate: true, RefToEntityID: map[string]string{}}
			return nil
		}
		st, err := loadState(ctx, tx, pi.Tenant, pi.Program, entIDs, edgeIDs)
		if err != nil {
			return err
		}
		if missing := kernel.Missing(st, p); len(missing) > 0 {
			out, err = s.buffer(ctx, tx, pi, obsUUID, missing)
			return err
		}
		res, err := kernel.Apply(st, p, s.cfg.Scope)
		if errors.Is(err, kernel.ErrMissingRecords) {
			return errRetry
		}
		if err != nil {
			return graphstore.Classify(err)
		}
		seqs, err := writeState(ctx, tx, pi.Tenant, pi.Program, st, res.Events)
		if err != nil {
			return err
		}
		var created []string
		for _, ev := range res.Events {
			switch ev.Type {
			case oam.ChangeEntityAdded:
				created = append(created, ev.EntityID)
			case oam.ChangeEdgeAdded:
				created = append(created, ev.EdgeID)
			}
		}
		if len(created) > 0 {
			tag, err := tx.Exec(ctx, markPendingReady, pi.Tenant.UUID.String(), pi.Program.UUID.String(), created)
			if err != nil {
				return graphstore.Internal("mark pending ready", err)
			}
			ready = tag.RowsAffected() > 0
		}
		out = graphstore.IngestResult{
			EntitiesCreated: res.EntitiesCreated, EntitiesUpdated: res.EntitiesUpdated,
			EdgesCreated: res.EdgesCreated, EdgesUpdated: res.EdgesUpdated,
			RefToEntityID: res.RefToEntityID, Changes: changesOf(pi.Tenant, pi.Program, st, res.Events, seqs),
		}
		return nil
	})
	if err != nil {
		return graphstore.IngestResult{}, false, err
	}
	return out, ready, nil
}

// buffer stores an observation with missing references, within the ingest
// transaction (which holds the record locks of the missing ids, so a writer
// creating one of them either committed before loadState saw it absent or
// will see this row when it marks pending rows ready).
func (s *Store) buffer(ctx context.Context, tx pgx.Tx, pi graphstore.PreparedIngest, obsUUID string, missing []string) (graphstore.IngestResult, error) {
	if s.cfg.MaxPending < 0 {
		return graphstore.IngestResult{}, graphstore.Classify(&oam.ValidationError{Path: "edges",
			Err: fmt.Errorf("%w: %s", oam.ErrUnknownEntity, missing[0])})
	}
	t := pi.Tenant.UUID.String()
	expired, err := tx.Exec(ctx, `DELETE FROM graph.pending_observations WHERE tenant_id = $1::uuid AND observed_at < $2`,
		t, s.cfg.Now().Add(-s.cfg.PendingTTL))
	if err != nil {
		return graphstore.IngestResult{}, graphstore.Internal("expire pending", err)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM graph.pending_observations WHERE tenant_id = $1::uuid`, t).Scan(&n); err != nil {
		return graphstore.IngestResult{}, graphstore.Internal("count pending", err)
	}
	if n >= s.cfg.MaxPending {
		return graphstore.IngestResult{}, graphstore.Classify(fmt.Errorf("%w: %d buffered", oam.ErrPendingLimit, n))
	}
	env, err := oam.EncodeObservation(pi.Observation)
	if err != nil {
		return graphstore.IngestResult{}, graphstore.Classify(err)
	}
	b := &pgx.Batch{}
	// The dedup row goes again: the observation is not applied yet, and a
	// re-send must reach the pending path (it deletes and re-buffers).
	b.Queue(`DELETE FROM graph.observations WHERE id = $1::uuid`, obsUUID)
	b.Queue(`INSERT INTO graph.pending_observations
	(tenant_id, program_id, observation_id, effect_path, observation, missing, observed_at)
	VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7)`, t, pi.Program.UUID.String(), obsUUID, pi.EffectPath,
		env, missing, pi.Observation.ObservedAt)
	if err := tx.SendBatch(ctx, b).Close(); err != nil {
		return graphstore.IngestResult{}, graphstore.Internal("buffer pending", err)
	}
	return graphstore.IngestResult{Pending: true, Missing: missing, RefToEntityID: map[string]string{},
		Expired: int(expired.RowsAffected())}, nil
}

// releasePending re-ingests the ready pending rows of req's program and
// adds the applied ones to out (Released, Changes). An observation that
// fails validation now is dropped (it would have been rejected in the
// other order); any other error leaves the rows ready for the next
// release. Best effort: the caller's own ingest has committed.
func (s *Store) releasePending(ctx context.Context, req graphstore.IngestRequest, out *graphstore.IngestResult) {
	pi, err := graphstore.PrepareIngest(req)
	if err != nil {
		return
	}
	for range 1000 { // a cascade releases more rows; bounded against a loop
		type row struct {
			id, effectPath string
			env            []byte
		}
		var rows []row
		err := s.tx(ctx, pi.Tenant, txRead, func(tx pgx.Tx) error {
			rs, err := tx.Query(ctx, `SELECT observation_id::text, effect_path, observation FROM graph.pending_observations
	WHERE tenant_id = $1::uuid AND program_id = $2::uuid AND ready ORDER BY observation_id LIMIT 100`,
				pi.Tenant.UUID.String(), pi.Program.UUID.String())
			if err != nil {
				return err
			}
			rows, err = pgx.CollectRows(rs, func(r pgx.CollectableRow) (row, error) {
				var x row
				return x, r.Scan(&x.id, &x.effectPath, &x.env)
			})
			return err
		})
		if err != nil || len(rows) == 0 {
			return
		}
		for _, r := range rows {
			o, err := oam.DecodeObservation(r.env, oam.Strict)
			if err != nil {
				s.dropPending(ctx, pi.Tenant, r.id)
				continue
			}
			res, _, err := s.ingest(ctx, graphstore.IngestRequest{TenantID: pi.Tenant.Text, ProgramID: pi.Program.Text,
				EffectPath: r.effectPath, Observation: o}, true)
			switch {
			case core.KindOf(err) == core.KindInvalidArgument:
				s.dropPending(ctx, pi.Tenant, r.id)
			case err != nil:
				return
			case !res.Duplicate && !res.Pending:
				out.Released = append(out.Released, o.ObservationID)
				out.Changes = append(out.Changes, res.Changes...)
			}
		}
	}
}

// dropPending deletes one pending row (an observation that can never be
// applied).
func (s *Store) dropPending(ctx context.Context, tenant graphstore.ID, obsUUID string) {
	_ = s.tx(ctx, tenant, txWrite, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, deletePendingResent, tenant.UUID.String(), obsUUID)
		return err
	})
}

// changesOf converts kernel events with their seqs and the entity
// snapshots of st.
func changesOf(tenant, program graphstore.ID, st *kernel.State, evs []kernel.Event, seqs map[string]int64) []graphstore.ChangeEvent {
	out := make([]graphstore.ChangeEvent, 0, len(evs))
	for _, ev := range evs {
		c := graphstore.ChangeEvent{
			ID: ev.ID, Seq: seqs[ev.ID], TenantID: tenant.Text, ProgramID: program.Text, Type: ev.Type,
			EntityID: ev.EntityID, EdgeID: ev.EdgeID, RunID: ev.RunID, Patch: ev.Patch, At: ev.At,
			TagType: ev.TagType, TagName: ev.TagName, Base: ev.Base, Cause: ev.Cause, Baseline: ev.Baseline,
		}
		if e, ok := st.Entities[ev.EntityID]; ok && ev.EntityID != "" {
			v := graphstore.EntityFromOAM(e.View(), false)
			c.Entity = &v
		}
		out = append(out, c)
	}
	return out
}

// byIDs selects rows of one tenant and program by id ($3, UUID texts).
// The tenant and program are compared as text on purpose: that keeps the
// planner on the primary key, even on a freshly created table whose
// statistics would otherwise tie it with the (tenant, program, ...)
// indexes and scan the whole program.
const byIDs = `e.id = ANY($3::text[]::uuid[]) AND e.tenant_id::text = $1 AND e.program_id::text = $2`

// loadState loads and locks the existing records among the ids.
func loadState(ctx context.Context, tx pgx.Tx, tenant, program graphstore.ID, entIDs, edgeIDs []string) (*kernel.State, error) {
	st := kernel.NewState()
	eu, err := uuidsOf(core.PrefixEntity, entIDs)
	if err != nil {
		return nil, graphstore.Internal("entity ids", err)
	}
	du, err := uuidsOf(core.PrefixEdge, edgeIDs)
	if err != nil {
		return nil, graphstore.Internal("edge ids", err)
	}
	t, p := tenant.UUID.String(), program.UUID.String()
	b := &pgx.Batch{}
	b.Queue(`SELECT `+entityCols+` FROM graph.entities e WHERE `+byIDs+` ORDER BY e.id FOR UPDATE`, t, p, eu)
	b.Queue(`SELECT entity_id::text, ptype, name, content, created_at, last_seen FROM graph.entity_tags
		WHERE tenant_id = $1 AND entity_id = ANY($2::text[]::uuid[])`, t, eu)
	b.Queue(`SELECT `+edgeCols+` FROM graph.edges e WHERE `+byIDs+` ORDER BY e.id FOR UPDATE`, t, p, du)
	b.Queue(`SELECT edge_id::text, ptype, name, content, created_at, last_seen FROM graph.edge_tags
		WHERE tenant_id = $1 AND edge_id = ANY($2::text[]::uuid[])`, t, du)
	br := tx.SendBatch(ctx, b)
	defer br.Close()
	var erows []kernel.EntityRow
	if err := eachRow(br, func(rows pgx.Rows) error {
		r, _, err := scanEntityRow(rows)
		erows = append(erows, r)
		return err
	}); err != nil {
		return nil, graphstore.Internal("load entities", err)
	}
	etags := map[string][]kernel.StoredTag{}
	if err := eachRow(br, scanTagInto(core.PrefixEntity, etags)); err != nil {
		return nil, graphstore.Internal("load entity tags", err)
	}
	var drows []kernel.EdgeRow
	if err := eachRow(br, func(rows pgx.Rows) error {
		r, _, err := scanEdgeRow(rows)
		drows = append(drows, r)
		return err
	}); err != nil {
		return nil, graphstore.Internal("load edges", err)
	}
	dtags := map[string][]kernel.StoredTag{}
	if err := eachRow(br, scanTagInto(core.PrefixEdge, dtags)); err != nil {
		return nil, graphstore.Internal("load edge tags", err)
	}
	for _, r := range erows {
		e, err := kernel.LoadEntity(tenant.Text, program.Text, r, etags[r.ID])
		if err != nil {
			return nil, graphstore.Internal("decode entity", err)
		}
		st.Entities[r.ID] = e
	}
	for _, r := range drows {
		e, err := kernel.LoadEdge(tenant.Text, program.Text, r, dtags[r.ID])
		if err != nil {
			return nil, graphstore.Internal("decode edge", err)
		}
		st.Edges[r.ID] = e
	}
	return st, nil
}

func eachRow(br pgx.BatchResults, fn func(pgx.Rows) error) error {
	rows, err := br.Query()
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func scanTagInto(p core.Prefix, into map[string][]kernel.StoredTag) func(pgx.Rows) error {
	return func(rows pgx.Rows) error {
		var (
			oid string
			t   kernel.StoredTag
		)
		if err := rows.Scan(&oid, &t.PType, &t.Name, &t.Content, &t.CreatedAt, &t.LastSeen); err != nil {
			return err
		}
		t.CreatedAt, t.LastSeen = t.CreatedAt.UTC(), t.LastSeen.UTC()
		id, err := publicOf(p, &oid)
		if err != nil {
			return err
		}
		into[id] = append(into[id], t)
		return nil
	}
}

const upsertEntities = `INSERT INTO graph.entities AS e (id, tenant_id, program_id, asset_type, key, content, scope_status,
	confidence, state, created_at, last_seen, first_seen_run, last_seen_run, missed_runs, search_text)
SELECT u.id::uuid, $1::uuid, $2::uuid, u.asset_type, u.key, u.content::jsonb, u.scope, u.conf::real, u.state,
	u.created, u.last, u.first_run::uuid, u.last_run::uuid, u.missed, u.search
FROM unnest($3::text[], $4::text[], $5::text[], $6::text[], $7::text[], $8::float8[], $9::text[],
	$10::timestamptz[], $11::timestamptz[], $12::text[], $13::text[], $14::int[], $15::text[])
	AS u(id, asset_type, key, content, scope, conf, state, created, last, first_run, last_run, missed, search)
ON CONFLICT (tenant_id, program_id, asset_type, key) DO UPDATE SET
	content = EXCLUDED.content, scope_status = EXCLUDED.scope_status, confidence = EXCLUDED.confidence,
	state = EXCLUDED.state, created_at = LEAST(e.created_at, EXCLUDED.created_at),
	last_seen = GREATEST(e.last_seen, EXCLUDED.last_seen), first_seen_run = EXCLUDED.first_seen_run,
	last_seen_run = EXCLUDED.last_seen_run, missed_runs = EXCLUDED.missed_runs, search_text = EXCLUDED.search_text`

const upsertEdges = `INSERT INTO graph.edges AS e (id, tenant_id, program_id, rel_type, label, discriminator, content,
	from_entity_id, to_entity_id, confidence, state, created_at, last_seen)
SELECT u.id::uuid, $1::uuid, $2::uuid, u.rel_type, u.label, u.disc, u.content::jsonb, u.from_id::uuid, u.to_id::uuid,
	u.conf::real, u.state, u.created, u.last
FROM unnest($3::text[], $4::text[], $5::text[], $6::text[], $7::text[], $8::text[], $9::text[], $10::float8[],
	$11::text[], $12::timestamptz[], $13::timestamptz[])
	AS u(id, rel_type, label, disc, content, from_id, to_id, conf, state, created, last)
ON CONFLICT (tenant_id, program_id, from_entity_id, to_entity_id, rel_type, label, discriminator) DO UPDATE SET
	content = EXCLUDED.content, confidence = EXCLUDED.confidence, state = EXCLUDED.state,
	created_at = LEAST(e.created_at, EXCLUDED.created_at), last_seen = GREATEST(e.last_seen, EXCLUDED.last_seen)`

func upsertTags(kind graphstore.TargetKind) string {
	table, owner := tagTable(kind)
	return `INSERT INTO ` + table + ` AS t (id, tenant_id, ` + owner + `, ptype, name, content, created_at, last_seen)
SELECT u.id::uuid, $1::uuid, u.owner::uuid, u.ptype, u.name, u.content::jsonb, u.created, u.last
FROM unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::timestamptz[], $8::timestamptz[])
	AS u(id, owner, ptype, name, content, created, last)
ON CONFLICT (` + owner + `, ptype, name) DO UPDATE SET content = EXCLUDED.content,
	created_at = LEAST(t.created_at, EXCLUDED.created_at), last_seen = GREATEST(t.last_seen, EXCLUDED.last_seen)`
}

const insertEvents = `INSERT INTO graph.change_events (id, tenant_id, program_id, type, entity_id, edge_id, run_id, patch, at,
	tag_type, tag_name, base, cause, baseline)
SELECT u.id::uuid, $1::uuid, $2::uuid, u.type, u.entity_id::uuid, u.edge_id::uuid, u.run_id::uuid, u.patch::jsonb, u.at,
	u.tag_type, u.tag_name, u.base::jsonb, u.cause, u.baseline
FROM unnest($3::text[], $4::text[], $5::text[], $6::text[], $7::text[], $8::text[], $9::timestamptz[],
	$10::text[], $11::text[], $12::text[], $13::text[], $14::bool[])
	WITH ORDINALITY AS u(id, type, entity_id, edge_id, run_id, patch, at, tag_type, tag_name, base, cause, baseline, ord)
ORDER BY u.ord
RETURNING id::text, seq`

// tagArrays accumulates tag rows for one upsertTags statement.
type tagArrays struct {
	ids, owners, ptypes, names, contents []string
	created, last                        []time.Time
}

func (a *tagArrays) add(tenant string, kind graphstore.TargetKind, ownerID, ownerUUID string, t kernel.StoredTag) {
	a.ids = append(a.ids, graphstore.TagUUID(tenant, kind, ownerID, t.PType, t.Name).String())
	a.owners = append(a.owners, ownerUUID)
	a.ptypes = append(a.ptypes, t.PType)
	a.names = append(a.names, t.Name)
	a.contents = append(a.contents, string(t.Content))
	a.created = append(a.created, t.CreatedAt)
	a.last = append(a.last, t.LastSeen)
}

func (a *tagArrays) queue(b *pgx.Batch, tenant string, kind graphstore.TargetKind) {
	if len(a.ids) > 0 {
		b.Queue(upsertTags(kind), tenant, a.ids, a.owners, a.ptypes, a.names, a.contents, a.created, a.last)
	}
}

// writeState writes every dirty record of st with all its tags and
// inserts the events; it returns the seq of each event id.
func writeState(ctx context.Context, tx pgx.Tx, tenant, program graphstore.ID, st *kernel.State, evs []kernel.Event) (map[string]int64, error) {
	t, p := tenant.UUID.String(), program.UUID.String()
	b := &pgx.Batch{}
	var (
		eIDs, eTypes, eKeys, eContent, eScope, eState, eSearch []string
		eConf                                                  []float64
		eCreated, eLast                                        []time.Time
		eFirst, eLastRun                                       []*string
		eMissed                                                []int32
		etags                                                  tagArrays
	)
	for _, id := range slices.Sorted(maps.Keys(st.Entities)) {
		e := st.Entities[id]
		if !e.Dirty {
			continue
		}
		row, err := e.Row()
		if err != nil {
			return nil, graphstore.Internal("encode entity", err)
		}
		u, err := uuidOf(core.PrefixEntity, id)
		if err != nil {
			return nil, graphstore.Internal("entity id", err)
		}
		first, err := nullUUID(core.PrefixRun, row.FirstSeenRun)
		if err != nil {
			return nil, graphstore.Internal("run id", err)
		}
		last, err := nullUUID(core.PrefixRun, row.LastSeenRun)
		if err != nil {
			return nil, graphstore.Internal("run id", err)
		}
		eIDs, eTypes, eKeys = append(eIDs, u), append(eTypes, row.AssetType), append(eKeys, row.Key)
		eContent, eScope, eState = append(eContent, string(row.Content)), append(eScope, string(row.Scope)), append(eState, string(row.State))
		eConf, eCreated, eLast = append(eConf, row.Confidence), append(eCreated, row.CreatedAt), append(eLast, row.LastSeen)
		eFirst, eLastRun = append(eFirst, first), append(eLastRun, last)
		eMissed, eSearch = append(eMissed, int32(row.MissedRuns)), append(eSearch, row.SearchText)
		tags, err := e.StoredTags()
		if err != nil {
			return nil, graphstore.Internal("encode tags", err)
		}
		for _, tg := range tags {
			etags.add(tenant.Text, graphstore.TargetEntity, id, u, tg)
		}
	}
	if len(eIDs) > 0 {
		b.Queue(upsertEntities, t, p, eIDs, eTypes, eKeys, eContent, eScope, eConf, eState, eCreated, eLast, eFirst, eLastRun, eMissed, eSearch)
	}
	etags.queue(b, t, graphstore.TargetEntity)
	var (
		dIDs, dRel, dLabel, dDisc, dContent, dFrom, dTo, dState []string
		dConf                                                   []float64
		dCreated, dLast                                         []time.Time
		dtags                                                   tagArrays
	)
	for _, id := range slices.Sorted(maps.Keys(st.Edges)) {
		e := st.Edges[id]
		if !e.Dirty {
			continue
		}
		row, err := e.Row()
		if err != nil {
			return nil, graphstore.Internal("encode edge", err)
		}
		u, err := uuidOf(core.PrefixEdge, id)
		if err != nil {
			return nil, graphstore.Internal("edge id", err)
		}
		from, err1 := uuidOf(core.PrefixEntity, row.From)
		to, err2 := uuidOf(core.PrefixEntity, row.To)
		if err1 != nil || err2 != nil {
			return nil, graphstore.Internal("edge endpoints", core.ErrInvalidID)
		}
		dIDs, dRel, dLabel, dDisc = append(dIDs, u), append(dRel, row.RelType), append(dLabel, row.Label), append(dDisc, row.Disc)
		dContent, dFrom, dTo = append(dContent, string(row.Content)), append(dFrom, from), append(dTo, to)
		dConf, dState, dCreated, dLast = append(dConf, row.Confidence), append(dState, string(row.State)), append(dCreated, row.CreatedAt), append(dLast, row.LastSeen)
		tags, err := e.StoredTags()
		if err != nil {
			return nil, graphstore.Internal("encode tags", err)
		}
		for _, tg := range tags {
			dtags.add(tenant.Text, graphstore.TargetEdge, id, u, tg)
		}
	}
	if len(dIDs) > 0 {
		b.Queue(upsertEdges, t, p, dIDs, dRel, dLabel, dDisc, dContent, dFrom, dTo, dConf, dState, dCreated, dLast)
	}
	dtags.queue(b, t, graphstore.TargetEdge)
	if len(evs) > 0 {
		b.Queue(lockProgram, lockKey(tenant, program))
	}
	nExec := b.Len()
	if len(evs) > 0 {
		if err := queueEvents(b, t, p, evs); err != nil {
			return nil, err
		}
	}
	if b.Len() == 0 {
		return map[string]int64{}, nil
	}
	br := tx.SendBatch(ctx, b)
	defer br.Close()
	for range nExec {
		if _, err := br.Exec(); err != nil {
			return nil, graphstore.Internal("write records", err)
		}
	}
	seqs := map[string]int64{}
	if len(evs) > 0 {
		if err := eachRow(br, func(rows pgx.Rows) error {
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
		}); err != nil {
			return nil, graphstore.Internal("insert change events", err)
		}
	}
	return seqs, nil
}

// queueEvents queues the insert of evs (in order, so seqs follow
// emission order).
func queueEvents(b *pgx.Batch, tenant, program string, evs []kernel.Event) error {
	var (
		ids, types                         []string
		ents, edges, runs                  []*string
		patches, tagTypes, tagNames, bases []*string
		causes                             []*string
		baselines                          []bool
		ats                                []time.Time
	)
	for _, ev := range evs {
		id, err := uuidOf(core.PrefixChange, ev.ID)
		if err != nil {
			return graphstore.Internal("change id", err)
		}
		ent, err1 := nullUUID(core.PrefixEntity, ev.EntityID)
		edge, err2 := nullUUID(core.PrefixEdge, ev.EdgeID)
		run, err3 := nullUUID(core.PrefixRun, ev.RunID)
		if err1 != nil || err2 != nil || err3 != nil {
			return graphstore.Internal("change ids", core.ErrInvalidID)
		}
		pb, err := kernel.EncodePatch(ev.Patch)
		if err != nil {
			return graphstore.Internal("encode patch", err)
		}
		var patch *string
		if pb != nil {
			ps := string(pb)
			patch = &ps
		}
		base, err := encodeBase(ev.Base)
		if err != nil {
			return graphstore.Internal("encode base", err)
		}
		ids, types = append(ids, id), append(types, string(ev.Type))
		ents, edges, runs, patches = append(ents, ent), append(edges, edge), append(runs, run), append(patches, patch)
		tagTypes, tagNames, bases = append(tagTypes, nullText(ev.TagType)), append(tagNames, nullText(ev.TagName)), append(bases, base)
		causes, baselines = append(causes, nullText(ev.Cause)), append(baselines, ev.Baseline)
		ats = append(ats, ev.At)
	}
	b.Queue(insertEvents, tenant, program, ids, types, ents, edges, runs, patches, ats, tagTypes, tagNames, bases, causes, baselines)
	return nil
}

func nullText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func encodeBase(b map[string]any) (*string, error) {
	if b == nil {
		return nil, nil
	}
	j, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	s := string(j)
	return &s, nil
}

// errRetry asks tx to run the transaction again (heads changed between
// the preload and the locks).
var errRetry = errors.New("pg: authority heads changed, retry")

// preloadHeads reads the stored units of an authoritative observation's
// source (without locking) and returns the member edges and targets the
// observation may withdraw.
func preloadHeads(ctx context.Context, tx pgx.Tx, tenant graphstore.ID, p *kernel.Prepared) (edges, entities []string, err error) {
	if p.Auth == nil || !p.Auth.Complete || len(p.Auth.Covered) == 0 {
		return nil, nil, nil
	}
	u, err := uuidOf(core.PrefixEntity, p.SourceID())
	if err != nil {
		return nil, nil, graphstore.Internal("source id", err)
	}
	names := make([]string, len(p.Auth.Covered))
	for i, f := range p.Auth.Covered {
		names[i] = oam.UnitKey(p.Auth.Profile, f)
	}
	if _, err := tx.Exec(ctx, setTenant, tenant.UUID.String()); err != nil {
		return nil, nil, graphstore.Internal("set tenant", err)
	}
	rows, err := tx.Query(ctx, `SELECT name, content FROM graph.entity_tags
		WHERE tenant_id = $1 AND entity_id = $2::uuid AND ptype = '`+kernel.PTypeAuth+`' AND name = ANY($3)`, tenant.UUID.String(), u, names)
	if err != nil {
		return nil, nil, graphstore.Internal("preload heads", err)
	}
	defer rows.Close()
	for rows.Next() {
		t := kernel.StoredTag{PType: kernel.PTypeAuth}
		if err := rows.Scan(&t.Name, &t.Content); err != nil {
			return nil, nil, graphstore.Internal("preload heads", err)
		}
		h, err := kernel.DecodeHead(t)
		if err != nil {
			return nil, nil, graphstore.Internal("decode head", err)
		}
		e, n := kernel.HeadTargets(h)
		edges, entities = append(edges, e...), append(entities, n...)
	}
	return edges, entities, rows.Err()
}

func sortedUnion(a, b []string) []string {
	out := append(slices.Clone(a), b...)
	slices.Sort(out)
	return slices.Compact(out)
}

// AddTag implements graphstore.Store: the tag is ingested as an
// observation from source graphstore.SourceAPI.
func (s *Store) AddTag(ctx context.Context, c graphstore.Caller, req graphstore.AddTagRequest) (graphstore.Tag, error) {
	tenant, err := graphstore.TenantOf(c)
	if err != nil {
		return graphstore.Tag{}, err
	}
	var (
		target string
		prefix = core.PrefixEntity
		table  = "graph.entities"
	)
	switch req.TargetKind {
	case graphstore.TargetEntity:
		id, err := graphstore.ParseID(core.PrefixEntity, "target_id", req.TargetID)
		if err != nil {
			return graphstore.Tag{}, err
		}
		target = id.Text
	case graphstore.TargetEdge:
		id, err := graphstore.ParseID(core.PrefixEdge, "target_id", req.TargetID)
		if err != nil {
			return graphstore.Tag{}, err
		}
		target, prefix, table = id.Text, core.PrefixEdge, "graph.edges"
	default:
		return graphstore.Tag{}, graphstore.InvalidArgument("target_kind", "target_kind must be entity or edge")
	}
	prop, err := graphstore.TagProperty(req)
	if err != nil {
		return graphstore.Tag{}, err
	}
	u, _ := uuidOf(prefix, target)
	var progText string
	err = s.tx(ctx, tenant, txRead, func(tx pgx.Tx) error {
		var pu string
		err := tx.QueryRow(ctx, `SELECT program_id::text FROM `+table+` WHERE tenant_id = $1 AND id = $2::uuid`, tenant.UUID.String(), u).Scan(&pu)
		if err == pgx.ErrNoRows {
			return graphstore.NotFound(string(req.TargetKind), target)
		}
		if err != nil {
			return graphstore.Internal("find tag target", err)
		}
		progText, err = publicOf(core.PrefixProgram, &pu)
		return err
	})
	if err != nil {
		return graphstore.Tag{}, err
	}
	program, err := graphstore.ParseID(core.PrefixProgram, "program_id", progText)
	if err != nil {
		return graphstore.Tag{}, graphstore.Internal("program id", err)
	}
	ir, err := graphstore.AddTagIngest(tenant, program, target, prop, s.cfg.Now())
	if err != nil {
		return graphstore.Tag{}, err
	}
	if _, err := s.IngestObservation(ctx, ir); err != nil {
		return graphstore.Tag{}, err
	}
	ptype, name, _, _ := oam.SplitProperty(prop)
	var tag graphstore.Tag
	err = s.tx(ctx, tenant, txRead, func(tx pgx.Tx) error {
		all, err := loadTags(ctx, tx, tenant, req.TargetKind, []string{target})
		if err != nil {
			return graphstore.Internal("load tags", err)
		}
		tags, _, err := publicTags(tenant.Text, req.TargetKind, target, all[target])
		if err != nil {
			return graphstore.Internal("decode tags", err)
		}
		for _, t := range tags {
			if t.PropertyType == ptype && t.Name == name {
				tag = t
				return nil
			}
		}
		return graphstore.Internal("add tag", errTagMissing)
	})
	return tag, err
}

type errString string

func (e errString) Error() string { return string(e) }

const errTagMissing = errString("pg: tag missing after ingest")
