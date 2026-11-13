package pg

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/graphstore/internal/kernel"
)

// readEntities returns the tenant's entities among ids (public ids)
// without tags, restricted to program when it is non-nil.
func readEntities(ctx context.Context, q querier, tenant graphstore.ID, program *graphstore.ID, ids []string) (map[string]graphstore.Entity, error) {
	out := map[string]graphstore.Entity{}
	if len(ids) == 0 {
		return out, nil
	}
	uuids, err := uuidsOf(core.PrefixEntity, ids)
	if err != nil {
		return nil, graphstore.Internal("entity ids", err)
	}
	var prog *string
	if program != nil {
		p := program.UUID.String()
		prog = &p
	}
	rows, err := q.Query(ctx, `SELECT `+entityCols+` FROM graph.entities e
		WHERE e.id = ANY($3::text[]::uuid[]) AND e.tenant_id::text = $1 AND ($2::text IS NULL OR e.program_id::text = $2)`,
		tenant.UUID.String(), prog, uuids)
	if err != nil {
		return nil, graphstore.Internal("read entities", err)
	}
	defer rows.Close()
	for rows.Next() {
		r, pid, err := scanEntityRow(rows)
		if err != nil {
			return nil, graphstore.Internal("scan entity", err)
		}
		e, err := entityOfRow(tenant.Text, pid, r)
		if err != nil {
			return nil, graphstore.Internal("decode entity", err)
		}
		out[e.ID] = e
	}
	if err := rows.Err(); err != nil {
		return nil, graphstore.Internal("read entities", err)
	}
	return out, nil
}

// withEntityTags fills tags and covering-run counters into entities.
func withEntityTags(ctx context.Context, q querier, tenant graphstore.ID, ents []graphstore.Entity) error {
	ids := make([]string, len(ents))
	for i, e := range ents {
		ids[i] = e.ID
	}
	all, err := loadTags(ctx, q, tenant, graphstore.TargetEntity, ids)
	if err != nil {
		return graphstore.Internal("load tags", err)
	}
	for i := range ents {
		if ents[i].Tags, ents[i].MissedRuns, err = publicTags(tenant.Text, graphstore.TargetEntity, ents[i].ID, all[ents[i].ID]); err != nil {
			return graphstore.Internal("decode tags", err)
		}
	}
	return nil
}

func withEdgeTags(ctx context.Context, q querier, tenant graphstore.ID, edges []graphstore.Edge) error {
	ids := make([]string, len(edges))
	for i, e := range edges {
		ids[i] = e.ID
	}
	all, err := loadTags(ctx, q, tenant, graphstore.TargetEdge, ids)
	if err != nil {
		return graphstore.Internal("load tags", err)
	}
	for i := range edges {
		if edges[i].Tags, edges[i].MissedRuns, err = publicTags(tenant.Text, graphstore.TargetEdge, edges[i].ID, all[edges[i].ID]); err != nil {
			return graphstore.Internal("decode tags", err)
		}
	}
	return nil
}

// entityOf returns one entity of the tenant, NOT_FOUND otherwise.
func entityOf(ctx context.Context, q querier, tenant graphstore.ID, field, id string) (graphstore.Entity, error) {
	eid, err := graphstore.EntityIDOf(field, id)
	if err != nil {
		return graphstore.Entity{}, err
	}
	m, err := readEntities(ctx, q, tenant, nil, []string{eid.Text})
	if err != nil {
		return graphstore.Entity{}, err
	}
	e, ok := m[eid.Text]
	if !ok {
		return graphstore.Entity{}, graphstore.NotFound("entity", eid.Text)
	}
	return e, nil
}

// GetEntity implements graphstore.Store.
func (s *Store) GetEntity(ctx context.Context, c graphstore.Caller, req graphstore.GetEntityRequest) (graphstore.Entity, error) {
	tenant, err := graphstore.TenantOf(c)
	if err != nil {
		return graphstore.Entity{}, err
	}
	var out graphstore.Entity
	err = s.tx(ctx, tenant, txRead, func(tx pgx.Tx) error {
		e, err := entityOf(ctx, tx, tenant, "entity_id", req.EntityID)
		if err != nil {
			return err
		}
		if req.IncludeTags {
			ents := []graphstore.Entity{e}
			if err := withEntityTags(ctx, tx, tenant, ents); err != nil {
				return err
			}
			e = ents[0]
		}
		out = e
		return nil
	})
	return out, err
}

// queryEntities runs a SELECT entityCols query.
func queryEntities(ctx context.Context, q querier, tenant graphstore.ID, sql string, args ...any) ([]graphstore.Entity, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, graphstore.Internal("query entities", err)
	}
	defer rows.Close()
	var out []graphstore.Entity
	for rows.Next() {
		r, pid, err := scanEntityRow(rows)
		if err != nil {
			return nil, graphstore.Internal("scan entity", err)
		}
		e, err := entityOfRow(tenant.Text, pid, r)
		if err != nil {
			return nil, graphstore.Internal("decode entity", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, graphstore.Internal("query entities", err)
	}
	return out, nil
}

// queryEdges runs a SELECT edgeCols query.
func queryEdges(ctx context.Context, q querier, tenant graphstore.ID, sql string, args ...any) ([]graphstore.Edge, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, graphstore.Internal("query edges", err)
	}
	defer rows.Close()
	var out []graphstore.Edge
	for rows.Next() {
		r, pid, err := scanEdgeRow(rows)
		if err != nil {
			return nil, graphstore.Internal("scan edge", err)
		}
		e, err := edgeOfRow(tenant.Text, pid, r)
		if err != nil {
			return nil, graphstore.Internal("decode edge", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, graphstore.Internal("query edges", err)
	}
	return out, nil
}

// FindEntities implements graphstore.Store.
func (s *Store) FindEntities(ctx context.Context, c graphstore.Caller, req graphstore.FindEntitiesRequest) (graphstore.FindEntitiesResult, error) {
	tenant, err := graphstore.TenantOf(c)
	if err != nil {
		return graphstore.FindEntitiesResult{}, err
	}
	q, err := graphstore.ValidateFind(req)
	if err != nil {
		return graphstore.FindEntitiesResult{}, err
	}
	after := core.Nil.String()
	if q.After != "" {
		if after, err = uuidOf(core.PrefixEntity, q.After); err != nil {
			return graphstore.FindEntitiesResult{}, graphstore.InvalidArgument("page_token", "malformed page token")
		}
	}
	var seen *time.Time
	if !q.SeenAfter.IsZero() {
		seen = &q.SeenAfter
	}
	var res graphstore.FindEntitiesResult
	err = s.tx(ctx, tenant, txRead, func(tx pgx.Tx) error {
		ents, err := queryEntities(ctx, tx, tenant, `SELECT `+entityCols+` FROM graph.entities e
			WHERE tenant_id = $1 AND program_id = $2 AND ($3::text[] IS NULL OR asset_type = ANY($3))
			AND ($4 = '' OR scope_status = $4) AND ($5 = '' OR state = $5)
			AND ($6::timestamptz IS NULL OR last_seen >= $6) AND starts_with(key, $7) AND id > $8::uuid
			ORDER BY e.id LIMIT $9`,
			tenant.UUID.String(), q.Program.UUID.String(), nilIfEmpty(q.AssetTypes), string(q.ScopeStatus), string(q.State),
			seen, q.KeyPrefix, after, q.Size+1)
		if err != nil {
			return err
		}
		if len(ents) > q.Size {
			ents = ents[:q.Size]
			res.NextPageToken = graphstore.EncodeIDToken(ents[q.Size-1].ID)
		}
		res.Entities = ents
		return nil
	})
	return res, err
}

func nilIfEmpty(xs []string) []string {
	if len(xs) == 0 {
		return nil
	}
	return xs
}

// Export implements graphstore.Store.
func (s *Store) Export(ctx context.Context, c graphstore.Caller, programID string) (graphstore.Export, error) {
	tenant, err := graphstore.TenantOf(c)
	if err != nil {
		return graphstore.Export{}, err
	}
	prog, err := graphstore.ParseID(core.PrefixProgram, "program_id", programID)
	if err != nil {
		return graphstore.Export{}, err
	}
	ex := graphstore.Export{Entities: []graphstore.Entity{}, Edges: []graphstore.Edge{}}
	err = s.tx(ctx, tenant, txRead, func(tx pgx.Tx) error {
		ents, err := queryEntities(ctx, tx, tenant, `SELECT `+entityCols+` FROM graph.entities e
			WHERE tenant_id = $1 AND program_id = $2 ORDER BY e.id`, tenant.UUID.String(), prog.UUID.String())
		if err != nil {
			return err
		}
		edges, err := queryEdges(ctx, tx, tenant, `SELECT `+edgeCols+` FROM graph.edges e
			WHERE tenant_id = $1 AND program_id = $2 ORDER BY e.id`, tenant.UUID.String(), prog.UUID.String())
		if err != nil {
			return err
		}
		if err := withEntityTags(ctx, tx, tenant, ents); err != nil {
			return err
		}
		if err := withEdgeTags(ctx, tx, tenant, edges); err != nil {
			return err
		}
		if ents != nil {
			ex.Entities = ents
		}
		if edges != nil {
			ex.Edges = edges
		}
		return nil
	})
	return ex, err
}

// reader is the traversal Reader over one program inside a read
// transaction.
type reader struct {
	q       querier
	tenant  graphstore.ID
	program graphstore.ID
}

func (r reader) Entities(ctx context.Context, ids []string) (map[string]graphstore.Entity, error) {
	return readEntities(ctx, r.q, r.tenant, &r.program, ids)
}

func (r reader) Edges(ctx context.Context, ids []string, out, in bool, labels []string) ([]graphstore.Edge, error) {
	uuids, err := uuidsOf(core.PrefixEntity, ids)
	if err != nil {
		return nil, err
	}
	// Tenant and program compared as text: see byIDs.
	return queryEdges(ctx, r.q, r.tenant, `SELECT `+edgeCols+` FROM graph.edges e
		WHERE e.tenant_id::text = $1 AND e.program_id::text = $2
		AND (($3 AND e.from_entity_id = ANY($5::text[]::uuid[])) OR ($4 AND e.to_entity_id = ANY($5::text[]::uuid[])))
		AND ($6::text[] IS NULL OR e.label = ANY($6))`,
		r.tenant.UUID.String(), r.program.UUID.String(), out, in, uuids, nilIfEmpty(labels))
}

func (s *Store) readerFor(ctx context.Context, tx pgx.Tx, tenant graphstore.ID, e graphstore.Entity) (reader, error) {
	prog, err := graphstore.ParseID(core.PrefixProgram, "program_id", e.ProgramID)
	if err != nil {
		return reader{}, graphstore.Internal("program id", err)
	}
	return reader{q: tx, tenant: tenant, program: prog}, nil
}

// Neighbors implements graphstore.Store.
func (s *Store) Neighbors(ctx context.Context, c graphstore.Caller, req graphstore.NeighborsRequest) (graphstore.Subgraph, error) {
	tenant, err := graphstore.TenantOf(c)
	if err != nil {
		return graphstore.Subgraph{}, err
	}
	if req, err = graphstore.ValidateNeighbors(req); err != nil {
		return graphstore.Subgraph{}, err
	}
	var out graphstore.Subgraph
	err = s.tx(ctx, tenant, txRead, func(tx pgx.Tx) error {
		start, err := entityOf(ctx, tx, tenant, "entity_id", req.EntityID)
		if err != nil {
			return err
		}
		r, err := s.readerFor(ctx, tx, tenant, start)
		if err != nil {
			return err
		}
		out, err = graphstore.TraverseNeighbors(ctx, r, start, req)
		return err
	})
	return out, err
}

// FindPath implements graphstore.Store.
func (s *Store) FindPath(ctx context.Context, c graphstore.Caller, req graphstore.FindPathRequest) (graphstore.Path, error) {
	tenant, err := graphstore.TenantOf(c)
	if err != nil {
		return graphstore.Path{}, err
	}
	if req, err = graphstore.ValidatePath(req); err != nil {
		return graphstore.Path{}, err
	}
	var out graphstore.Path
	err = s.tx(ctx, tenant, txRead, func(tx pgx.Tx) error {
		from, err := entityOf(ctx, tx, tenant, "from_entity_id", req.FromEntityID)
		if err != nil {
			return err
		}
		to, err := entityOf(ctx, tx, tenant, "to_entity_id", req.ToEntityID)
		if err != nil {
			return err
		}
		if from.ProgramID != to.ProgramID {
			return nil
		}
		r, err := s.readerFor(ctx, tx, tenant, from)
		if err != nil {
			return err
		}
		out, err = graphstore.TraversePath(ctx, r, from, to, req)
		return err
	})
	return out, err
}

// BlastRadius implements graphstore.Store.
func (s *Store) BlastRadius(ctx context.Context, c graphstore.Caller, req graphstore.BlastRadiusRequest) ([]graphstore.Reach, error) {
	tenant, err := graphstore.TenantOf(c)
	if err != nil {
		return nil, err
	}
	if req, err = graphstore.ValidateBlast(req); err != nil {
		return nil, err
	}
	var out []graphstore.Reach
	err = s.tx(ctx, tenant, txRead, func(tx pgx.Tx) error {
		start, err := entityOf(ctx, tx, tenant, "entity_id", req.EntityID)
		if err != nil {
			return err
		}
		r, err := s.readerFor(ctx, tx, tenant, start)
		if err != nil {
			return err
		}
		out, err = graphstore.TraverseBlastRadius(ctx, r, start, req)
		return err
	})
	return out, err
}

// Search implements graphstore.Store: candidates come from the trigram
// index (LIKE for substrings, % for similarity), ranked by similarity in
// SQL; the final match and score are computed by graphstore.MatchSearch,
// exactly as Mem does.
func (s *Store) Search(ctx context.Context, c graphstore.Caller, req graphstore.SearchRequest) (graphstore.SearchResult, error) {
	tenant, err := graphstore.TenantOf(c)
	if err != nil {
		return graphstore.SearchResult{}, err
	}
	query, limit, err := graphstore.ValidateSearch(req)
	if err != nil {
		return graphstore.SearchResult{}, err
	}
	prog, err := graphstore.ParseID(core.PrefixProgram, "program_id", req.ProgramID)
	if err != nil {
		return graphstore.SearchResult{}, err
	}
	thr := s.cfg.FuzzyThreshold
	var res graphstore.SearchResult
	err = s.tx(ctx, tenant, txRead, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('pg_trgm.similarity_threshold', $1, true)`, formatFloat(max(thr-0.01, 0.01))); err != nil {
			return graphstore.Internal("set similarity threshold", err)
		}
		rows, err := tx.Query(ctx, `SELECT `+entityCols+`, search_text FROM graph.entities e
			WHERE tenant_id = $1 AND program_id = $2 AND ($3::text[] IS NULL OR asset_type = ANY($3))
			AND (search_text LIKE $4 ESCAPE '\' OR ($5 AND search_text % $6 AND similarity(search_text, $6) >= $7))
			ORDER BY similarity(search_text, $6) DESC, e.id LIMIT $8`,
			tenant.UUID.String(), prog.UUID.String(), nilIfEmpty(req.AssetTypes), likePattern(query), req.Fuzzy, query,
			thr-1e-6, limit+16)
		if err != nil {
			return graphstore.Internal("search", err)
		}
		defer rows.Close()
		for rows.Next() {
			r, pid, text, err := scanSearchRow(rows)
			if err != nil {
				return graphstore.Internal("scan search hit", err)
			}
			score, matched, ok := graphstore.MatchSearch(query, text, req.Fuzzy, thr)
			if !ok {
				continue
			}
			e, err := entityOfRow(tenant.Text, pid, r)
			if err != nil {
				return graphstore.Internal("decode entity", err)
			}
			res.Hits = append(res.Hits, graphstore.SearchHit{Entity: e, Score: score, MatchedTrigrams: matched})
		}
		return rows.Err()
	})
	if err != nil {
		return graphstore.SearchResult{}, err
	}
	graphstore.SortHits(res.Hits)
	if len(res.Hits) > limit {
		res.Hits = res.Hits[:limit]
	}
	return res, nil
}

// scanSearchRow scans entityCols followed by search_text.
func scanSearchRow(rows pgx.Rows) (kernel.EntityRow, string, string, error) {
	var text string
	r, pid, err := scanEntityRow(extraCol{rows, &text})
	return r, pid, text, err
}

// extraCol appends one destination to Scan, so scanEntityRow serves
// queries with a trailing column.
type extraCol struct {
	rows  scanner
	extra any
}

func (s extraCol) Scan(dest ...any) error { return s.rows.Scan(append(dest, s.extra)...) }

func likePattern(q string) string {
	out := make([]byte, 0, len(q)+2)
	out = append(out, '%')
	for i := 0; i < len(q); i++ {
		if c := q[i]; c == '\\' || c == '%' || c == '_' {
			out = append(out, '\\')
		}
		out = append(out, q[i])
	}
	return string(append(out, '%'))
}

func formatFloat(f float64) string { return strconv.FormatFloat(f, 'f', 6, 64) }
