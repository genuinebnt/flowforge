package pg

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/csr"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/oam"
)

// BuildSnapshot implements graphstore.Store. It reads the watermark,
// the active entities (in id order) and the active edges in one
// repeatable-read transaction, so the snapshot reflects exactly the
// change events up to the watermark (ingest commits a program's events in
// seq order, see lockProgram).
func (s *Store) BuildSnapshot(ctx context.Context, c graphstore.Caller, programID string) (*csr.Graph, uint64, error) {
	tenant, err := graphstore.TenantOf(c)
	if err != nil {
		return nil, 0, err
	}
	prog, err := graphstore.ParseID(core.PrefixProgram, "program_id", programID)
	if err != nil {
		return nil, 0, err
	}
	var (
		ents  []graphstore.Entity
		edges []graphstore.Edge
		wm    int64
	)
	err = s.tx(ctx, tenant, txRead, func(tx pgx.Tx) error {
		ents, edges = nil, nil
		t, p := tenant.UUID.String(), prog.UUID.String()
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(seq), 0) FROM graph.change_events WHERE tenant_id = $1 AND program_id = $2`,
			t, p).Scan(&wm); err != nil {
			return graphstore.Internal("snapshot watermark", err)
		}
		rows, err := tx.Query(ctx, `SELECT e.id::text, e.asset_type FROM graph.entities e
			WHERE e.tenant_id = $1 AND e.program_id = $2 AND e.state = 'active' ORDER BY e.id`, t, p)
		if err != nil {
			return graphstore.Internal("snapshot entities", err)
		}
		for rows.Next() {
			var id string
			e := graphstore.Entity{State: oam.StateActive}
			if err := rows.Scan(&id, &e.AssetType); err != nil {
				rows.Close()
				return graphstore.Internal("snapshot entities", err)
			}
			if e.ID, err = publicOf(core.PrefixEntity, &id); err != nil {
				rows.Close()
				return graphstore.Internal("snapshot entities", err)
			}
			ents = append(ents, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return graphstore.Internal("snapshot entities", err)
		}
		rows, err = tx.Query(ctx, `SELECT from_entity_id::text, to_entity_id::text, label, confidence FROM graph.edges
			WHERE tenant_id = $1 AND program_id = $2 AND state = 'active'`, t, p)
		if err != nil {
			return graphstore.Internal("snapshot edges", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				from, to string
				conf     float32
			)
			e := graphstore.Edge{State: oam.StateActive}
			if err := rows.Scan(&from, &to, &e.Label, &conf); err != nil {
				return graphstore.Internal("snapshot edges", err)
			}
			e.FromEntityID, _ = publicOf(core.PrefixEntity, &from)
			e.ToEntityID, _ = publicOf(core.PrefixEntity, &to)
			e.Confidence = graphstore.RoundConfidence(float64(conf))
			edges = append(edges, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, err
	}
	g, err := graphstore.BuildCSR(graphstore.SnapshotMeta(tenant, prog, uint64(wm), s.cfg.Now()), ents, edges)
	return g, uint64(wm), err
}

// Components implements graphstore.Store (on a fresh snapshot).
func (s *Store) Components(ctx context.Context, c graphstore.Caller, programID string) (graphstore.Components, error) {
	g, wm, err := s.BuildSnapshot(ctx, c, programID)
	if err != nil {
		return graphstore.Components{}, err
	}
	return graphstore.ComponentsOf(g, wm)
}

const snapshotCols = `snapshot_id::text, tenant_id::text, program_id::text, watermark_seq, node_count, edge_count,
	gcs_uri, sha256, format_version, built_at`

// WriteSnapshot implements graphstore.Store: the file is written through
// blobs, then recorded in graph.snapshots. A snapshot at an already
// recorded watermark keeps the first catalogue row.
func (s *Store) WriteSnapshot(ctx context.Context, c graphstore.Caller, programID string, blobs graphstore.BlobStore) (graphstore.SnapshotInfo, error) {
	g, wm, err := s.BuildSnapshot(ctx, c, programID)
	if err != nil {
		return graphstore.SnapshotInfo{}, err
	}
	tenant, _ := graphstore.TenantOf(c)
	prog, _ := graphstore.ParseID(core.PrefixProgram, "program_id", programID)
	info, err := graphstore.PublishSnapshot(ctx, g, tenant, prog, wm, blobs, s.cfg.Now())
	if err != nil {
		return graphstore.SnapshotInfo{}, err
	}
	var out graphstore.SnapshotInfo
	err = s.tx(ctx, tenant, txWrite, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO graph.snapshots (snapshot_id, tenant_id, program_id, watermark_seq, node_count,
			edge_count, gcs_uri, sha256, format_version, built_at) VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (tenant_id, program_id, watermark_seq) DO NOTHING`,
			info.SnapshotID, tenant.UUID.String(), prog.UUID.String(), int64(info.WatermarkSeq), info.NodeCount, info.EdgeCount,
			info.URI, info.SHA256[:], int16(info.FormatVersion), info.BuiltAt); err != nil {
			return graphstore.Internal("record snapshot", err)
		}
		out, err = scanSnapshot(tx.QueryRow(ctx, `SELECT `+snapshotCols+` FROM graph.snapshots
			WHERE tenant_id = $1 AND program_id = $2 AND watermark_seq = $3`, tenant.UUID.String(), prog.UUID.String(), int64(wm)))
		return err
	})
	return out, err
}

// LatestSnapshot implements graphstore.Store.
func (s *Store) LatestSnapshot(ctx context.Context, c graphstore.Caller, programID string) (graphstore.SnapshotInfo, error) {
	tenant, err := graphstore.TenantOf(c)
	if err != nil {
		return graphstore.SnapshotInfo{}, err
	}
	prog, err := graphstore.ParseID(core.PrefixProgram, "program_id", programID)
	if err != nil {
		return graphstore.SnapshotInfo{}, err
	}
	var out graphstore.SnapshotInfo
	err = s.tx(ctx, tenant, txRead, func(tx pgx.Tx) error {
		out, err = scanSnapshot(tx.QueryRow(ctx, `SELECT `+snapshotCols+` FROM graph.snapshots
			WHERE tenant_id = $1 AND program_id = $2 ORDER BY watermark_seq DESC LIMIT 1`, tenant.UUID.String(), prog.UUID.String()))
		if err == pgx.ErrNoRows {
			return graphstore.NotFound("snapshot of program", prog.Text)
		}
		return err
	})
	return out, err
}

func scanSnapshot(row pgx.Row) (graphstore.SnapshotInfo, error) {
	var (
		info            graphstore.SnapshotInfo
		tenant, program string
		wm              int64
		sum             []byte
		version         int16
		built           time.Time
	)
	if err := row.Scan(&info.SnapshotID, &tenant, &program, &wm, &info.NodeCount, &info.EdgeCount, &info.URI, &sum, &version, &built); err != nil {
		if err == pgx.ErrNoRows {
			return info, err
		}
		return info, graphstore.Internal("read snapshot", err)
	}
	info.TenantID, _ = publicOf(core.PrefixTenant, &tenant)
	info.ProgramID, _ = publicOf(core.PrefixProgram, &program)
	info.WatermarkSeq, info.FormatVersion, info.BuiltAt = uint64(wm), int(version), built.UTC()
	copy(info.SHA256[:], sum)
	return info, nil
}
