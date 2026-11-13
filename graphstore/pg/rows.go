package pg

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/graphstore/internal/kernel"
	"github.com/genuinebnt/flowforge/oam"
)

// uuidOf returns the UUID text of a public id (ent_, edg_, ...).
func uuidOf(p core.Prefix, id string) (string, error) {
	u, err := core.ParsePublicID(p, id)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func uuidsOf(p core.Prefix, ids []string) ([]string, error) {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		u, err := uuidOf(p, id)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

// publicOf returns the public id of a UUID text ("" for NULL).
func publicOf(p core.Prefix, u *string) (string, error) {
	if u == nil || *u == "" {
		return "", nil
	}
	parsed, err := core.ParseUUID(*u)
	if err != nil {
		return "", err
	}
	return core.PublicID(p, parsed), nil
}

func nullUUID(p core.Prefix, id string) (*string, error) {
	if id == "" {
		return nil, nil
	}
	u, err := uuidOf(p, id)
	return &u, err
}

const entityCols = `id::text, asset_type, key, content, scope_status, confidence, state, created_at, last_seen,
	first_seen_run::text, last_seen_run::text, missed_runs, program_id::text`

const edgeCols = `id::text, rel_type, label, discriminator, content, from_entity_id::text, to_entity_id::text,
	confidence, state, created_at, last_seen, program_id::text`

// scanEntityRow scans entityCols; it also returns the program id.
func scanEntityRow(rows scanner) (kernel.EntityRow, string, error) {
	var (
		prog              string
		r                 kernel.EntityRow
		id                string
		scope, state      string
		first, last       *string
		created, lastSeen time.Time
		conf              float32
	)
	if err := rows.Scan(&id, &r.AssetType, &r.Key, &r.Content, &scope, &conf, &state, &created, &lastSeen, &first, &last, &r.MissedRuns, &prog); err != nil {
		return r, "", err
	}
	var err error
	if r.ID, err = publicOf(core.PrefixEntity, &id); err != nil {
		return r, "", err
	}
	if r.FirstSeenRun, err = publicOf(core.PrefixRun, first); err != nil {
		return r, "", err
	}
	if r.LastSeenRun, err = publicOf(core.PrefixRun, last); err != nil {
		return r, "", err
	}
	if prog, err = publicOf(core.PrefixProgram, &prog); err != nil {
		return r, "", err
	}
	r.Scope, r.State, r.Confidence = oam.ScopeStatus(scope), oam.State(state), graphstore.RoundConfidence(float64(conf))
	r.CreatedAt, r.LastSeen = created.UTC(), lastSeen.UTC()
	return r, prog, nil
}

// scanEdgeRow scans edgeCols; it also returns the program id.
func scanEdgeRow(rows scanner) (kernel.EdgeRow, string, error) {
	var (
		prog              string
		r                 kernel.EdgeRow
		id, from, to      string
		state             string
		created, lastSeen time.Time
		conf              float32
	)
	if err := rows.Scan(&id, &r.RelType, &r.Label, &r.Disc, &r.Content, &from, &to, &conf, &state, &created, &lastSeen, &prog); err != nil {
		return r, "", err
	}
	var err error
	if r.ID, err = publicOf(core.PrefixEdge, &id); err != nil {
		return r, "", err
	}
	if r.From, err = publicOf(core.PrefixEntity, &from); err != nil {
		return r, "", err
	}
	if r.To, err = publicOf(core.PrefixEntity, &to); err != nil {
		return r, "", err
	}
	if prog, err = publicOf(core.PrefixProgram, &prog); err != nil {
		return r, "", err
	}
	r.State, r.Confidence = oam.State(state), graphstore.RoundConfidence(float64(conf))
	r.CreatedAt, r.LastSeen = created.UTC(), lastSeen.UTC()
	return r, prog, nil
}

func entityOfRow(tenant, program string, r kernel.EntityRow) (graphstore.Entity, error) {
	content, err := kernel.DecodeObject(r.Content)
	if err != nil {
		return graphstore.Entity{}, err
	}
	return graphstore.Entity{
		ID: r.ID, TenantID: tenant, ProgramID: program, AssetType: r.AssetType, Key: r.Key, Content: content,
		ScopeStatus: r.Scope, Confidence: r.Confidence, State: r.State, CreatedAt: r.CreatedAt, LastSeen: r.LastSeen,
		FirstSeenRun: r.FirstSeenRun, LastSeenRun: r.LastSeenRun,
	}, nil
}

func edgeOfRow(tenant, program string, r kernel.EdgeRow) (graphstore.Edge, error) {
	content, err := kernel.DecodeObject(r.Content)
	if err != nil {
		return graphstore.Edge{}, err
	}
	return graphstore.Edge{
		ID: r.ID, TenantID: tenant, ProgramID: program, RelationType: r.RelType, Label: r.Label, Discriminator: r.Disc,
		Content: content, FromEntityID: r.From, ToEntityID: r.To, Confidence: r.Confidence, State: r.State,
		CreatedAt: r.CreatedAt, LastSeen: r.LastSeen,
	}, nil
}

// tagTable returns the tag table and owner column of a target kind.
func tagTable(kind graphstore.TargetKind) (table, owner string) {
	if kind == graphstore.TargetEdge {
		return "graph.edge_tags", "edge_id"
	}
	return "graph.entity_tags", "entity_id"
}

// loadTags returns the tag rows of the given owners (public ids), hidden
// ones included, by owner.
func loadTags(ctx context.Context, q querier, tenant graphstore.ID, kind graphstore.TargetKind, ids []string) (map[string][]kernel.StoredTag, error) {
	out := map[string][]kernel.StoredTag{}
	if len(ids) == 0 {
		return out, nil
	}
	prefix := core.PrefixEntity
	if kind == graphstore.TargetEdge {
		prefix = core.PrefixEdge
	}
	uuids, err := uuidsOf(prefix, ids)
	if err != nil {
		return nil, err
	}
	table, owner := tagTable(kind)
	rows, err := q.Query(ctx, `SELECT `+owner+`::text, ptype, name, content, created_at, last_seen FROM `+table+
		` WHERE tenant_id = $1 AND `+owner+` = ANY($2::text[]::uuid[])`, tenant.UUID.String(), uuids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			oid string
			t   kernel.StoredTag
		)
		if err := rows.Scan(&oid, &t.PType, &t.Name, &t.Content, &t.CreatedAt, &t.LastSeen); err != nil {
			return nil, err
		}
		t.CreatedAt, t.LastSeen = t.CreatedAt.UTC(), t.LastSeen.UTC()
		pid, err := publicOf(prefix, &oid)
		if err != nil {
			return nil, err
		}
		out[pid] = append(out[pid], t)
	}
	return out, rows.Err()
}

// publicTags converts stored tags to API tags (hidden ones dropped),
// sorted by (type, name), plus the covering-run counters.
func publicTags(tenant string, kind graphstore.TargetKind, target string, stored []kernel.StoredTag) ([]graphstore.Tag, map[string]int, error) {
	tags := []graphstore.Tag{}
	var missed map[string]int
	visible, err := kernel.PublicTagRows(stored)
	if err != nil {
		return nil, nil, err
	}
	for _, t := range stored {
		if t.PType == kernel.PTypeRun {
			rs, err := runState(t)
			if err != nil {
				return nil, nil, err
			}
			if rs.Covered {
				if missed == nil {
					missed = map[string]int{}
				}
				missed[t.Name] = rs.Missed
			}
		}
	}
	for _, t := range visible {
		c, err := kernel.DecodeObject(t.Content)
		if err != nil {
			return nil, nil, err
		}
		tags = append(tags, graphstore.TagFromOAM(tenant, kind, target, oam.Tag{
			PropertyType: t.PType, Name: t.Name, Content: c, CreatedAt: t.CreatedAt, LastSeen: t.LastSeen,
		}))
	}
	slices.SortFunc(tags, func(a, b graphstore.Tag) int {
		return cmp.Or(cmp.Compare(a.PropertyType, b.PropertyType), cmp.Compare(a.Name, b.Name))
	})
	return tags, missed, nil
}

func runState(t kernel.StoredTag) (kernel.RunState, error) { return kernel.DecodeRunTag(t) }

// scanner is a row being scanned.
type scanner interface{ Scan(dest ...any) error }

// querier is what read helpers need (a pgx.Tx).
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}
