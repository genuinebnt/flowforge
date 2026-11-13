// Package pg is the Postgres adapter of the contentstore Repo port: the
// content schema of migration 0031 (V2 design §6.2). Every transaction
// sets app.tenant_id first, so row-level security (forced on every
// tenant table) confines it to one tenant: the content service's role
// ff_content holds no cross-tenant policy except on content.programs,
// which background sweeps read to enumerate tenants.
package pg

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/genuinebnt/flowforge/contentstore"
	"github.com/genuinebnt/flowforge/core"
)

// DB is the part of a pgx pool the adapter uses (*pgxpool.Pool).
type DB interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Ping(ctx context.Context) error
}

// Repo implements contentstore.Repo over Postgres.
type Repo struct {
	DB DB
}

// New returns a Repo over db.
func New(db DB) *Repo { return &Repo{DB: db} }

// setTenant sets the transaction's tenant for RLS (spec 10.7), as
// graphstore/pg does.
const setTenant = `SELECT set_config('app.tenant_id', $1, true)`

// InTx implements contentstore.Repo. Unique violations (a concurrent
// ingest of the same key or capture) map to contentstore.ErrConflict, which
// the store retries.
func (r *Repo) InTx(ctx context.Context, tenant core.UUID, fn func(contentstore.Tx) error) (err error) {
	tx, err := r.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if _, err = tx.Exec(ctx, setTenant, tenant.String()); err != nil {
		return err
	}
	if err = fn(&pgTx{tx: tx, tenant: tenant}); err != nil {
		return mapErr(err)
	}
	return mapErr(tx.Commit(ctx))
}

func mapErr(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && (pe.Code == "23505" || pe.Code == "40001" || pe.Code == "40P01") {
		return &core.Error{Kind: core.KindAborted, Reason: core.ReasonVersionConflict, Message: "contentstore/pg: " + pe.Message, Err: err}
	}
	return err
}

// Tenants implements contentstore.Repo (content.programs, the one table
// ff_content may read across tenants).
func (r *Repo) Tenants(ctx context.Context) ([]core.UUID, error) {
	rows, err := r.DB.Query(ctx, `SELECT DISTINCT tenant_id::text FROM content.programs ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	ss, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make([]core.UUID, 0, len(ss))
	for _, s := range ss {
		u, err := core.ParseUUID(s)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

// Ping implements contentstore.Repo.
func (r *Repo) Ping(ctx context.Context) error { return r.DB.Ping(ctx) }

type pgTx struct {
	tx     pgx.Tx
	tenant core.UUID
}

func nullUUID(u core.UUID) *string {
	if u.IsZero() {
		return nil
	}
	s := u.String()
	return &s
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func parseNull(s *string) (core.UUID, error) {
	if s == nil || *s == "" {
		return core.Nil, nil
	}
	return core.ParseUUID(*s)
}

const snapshotCols = `id::text, program_id::text, entity_id, url, url_template, host, profile, run_id, effect_path,
	fetched_at, last_seen, valid_from, valid_to, status, mime, kind, signature, headers_ref, body_manifest, body_sha256,
	raw_size, stored_size, normalizer_version, normalized_manifest, parent_id::text, lineage_parent_id::text, features_ref,
	outcome, coverage, degraded, simhash, pinned, pin_reason, codec, delta_base::text, delta_depth, body_seal, headers_seal,
	key_id, created_at, derive_stamp`

func (t *pgTx) scanSnapshots(rows pgx.Rows) ([]contentstore.Snapshot, error) {
	defer rows.Close()
	var out []contentstore.Snapshot
	for rows.Next() {
		var s contentstore.Snapshot
		var id, prog string
		var validTo *time.Time
		var parent, lineage, base *string
		var sim int64
		if err := rows.Scan(&id, &prog, &s.EntityID, &s.URL, &s.URLTemplate, &s.Host, &s.Profile, &s.RunID, &s.EffectPath,
			&s.FetchedAt, &s.LastSeen, &s.ValidFrom, &validTo, &s.Status, &s.Mime, &s.Kind, &s.Signature, &s.HeadersRef,
			&s.BodyManifest, &s.BodySHA256, &s.RawSize, &s.StoredSize, &s.NormalizerVersion, &s.NormalizedManifest, &parent,
			&lineage, &s.FeaturesRef, &s.Outcome, &s.Coverage, &s.Degraded, &sim, &s.Pinned, &s.PinReason, &s.Codec, &base,
			&s.DeltaDepth, &s.BodySeal, &s.HeadersSeal, &s.KeyID, &s.CreatedAt, &s.DeriveStamp); err != nil {
			return nil, err
		}
		var err error
		if s.ID, err = core.ParseUUID(id); err != nil {
			return nil, err
		}
		if s.Program, err = core.ParseUUID(prog); err != nil {
			return nil, err
		}
		if s.ParentID, err = parseNull(parent); err != nil {
			return nil, err
		}
		if s.LineageParentID, err = parseNull(lineage); err != nil {
			return nil, err
		}
		if s.DeltaBase, err = parseNull(base); err != nil {
			return nil, err
		}
		if validTo != nil {
			s.ValidTo = validTo.UTC()
		}
		s.Tenant, s.SimHash = t.tenant, uint64(sim)
		s.FetchedAt, s.LastSeen, s.ValidFrom, s.CreatedAt = s.FetchedAt.UTC(), s.LastSeen.UTC(), s.ValidFrom.UTC(), s.CreatedAt.UTC()
		out = append(out, s)
	}
	return out, rows.Err()
}

func (t *pgTx) snapshots(ctx context.Context, where string, args ...any) ([]contentstore.Snapshot, error) {
	rows, err := t.tx.Query(ctx, `SELECT `+snapshotCols+` FROM content.snapshots WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	return t.scanSnapshots(rows)
}

func (t *pgTx) one(ctx context.Context, where string, args ...any) (contentstore.Snapshot, bool, error) {
	ss, err := t.snapshots(ctx, where, args...)
	if err != nil || len(ss) == 0 {
		return contentstore.Snapshot{}, false, err
	}
	return ss[0], true, nil
}

func (t *pgTx) Programs(ctx context.Context) ([]core.UUID, error) {
	rows, err := t.tx.Query(ctx, `SELECT program_id::text FROM content.programs WHERE tenant_id = $1 ORDER BY 1`, t.tenant.String())
	if err != nil {
		return nil, err
	}
	ss, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make([]core.UUID, 0, len(ss))
	for _, s := range ss {
		u, err := core.ParseUUID(s)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

func (t *pgTx) EnsureProgram(ctx context.Context, p core.UUID) error {
	_, err := t.tx.Exec(ctx, `INSERT INTO content.programs (tenant_id, program_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, t.tenant.String(), p.String())
	return err
}

const keyWhere = `tenant_id = $1 AND program_id = $2 AND url = $3 AND profile = $4`

func (t *pgTx) keyArgs(k contentstore.Key) []any {
	return []any{t.tenant.String(), k.Program.String(), k.URL, k.Profile}
}

func (t *pgTx) OpenVersion(ctx context.Context, k contentstore.Key) (contentstore.Snapshot, bool, error) {
	return t.one(ctx, keyWhere+` AND valid_to IS NULL FOR UPDATE`, t.keyArgs(k)...)
}

func (t *pgTx) SnapshotBySignature(ctx context.Context, k contentstore.Key, sig string, at time.Time) (contentstore.Snapshot, bool, error) {
	return t.one(ctx, keyWhere+` AND signature = $5 AND fetched_at = $6 LIMIT 1`, append(t.keyArgs(k), sig, at)...)
}

func (t *pgTx) GetSnapshot(ctx context.Context, id core.UUID) (contentstore.Snapshot, bool, error) {
	return t.one(ctx, `tenant_id = $1 AND id = $2`, t.tenant.String(), id.String())
}

func (t *pgTx) SnapshotAsOf(ctx context.Context, k contentstore.Key, at time.Time) (contentstore.Snapshot, bool, error) {
	return t.one(ctx, keyWhere+` AND valid_from <= $5 AND (valid_to IS NULL OR valid_to > $5) ORDER BY valid_from DESC LIMIT 1`,
		append(t.keyArgs(k), at)...)
}

func (t *pgTx) ListSnapshots(ctx context.Context, f contentstore.SnapshotFilter) ([]contentstore.Snapshot, error) {
	where := `tenant_id = $1 AND program_id = $2 AND ($3 = '' OR url = $3) AND ($4 = '' OR profile = $4) AND (NOT $5 OR valid_to IS NULL)`
	args := []any{t.tenant.String(), f.Program.String(), f.URL, f.Profile, f.OpenOnly}
	if f.Before != nil {
		where += ` AND (valid_from, id) < ($6, $7::uuid)`
		args = append(args, f.Before.ValidFrom, f.Before.ID.String())
	}
	where += ` ORDER BY valid_from DESC, id DESC`
	if f.Limit > 0 {
		where += ` LIMIT ` + strconv.Itoa(f.Limit)
	}
	return t.snapshots(ctx, where, args...)
}

func (t *pgTx) KeySnapshots(ctx context.Context, k contentstore.Key) ([]contentstore.Snapshot, error) {
	return t.snapshots(ctx, keyWhere+` ORDER BY valid_from DESC, id DESC`, t.keyArgs(k)...)
}

func (t *pgTx) Keys(ctx context.Context, program core.UUID) ([]contentstore.Key, error) {
	rows, err := t.tx.Query(ctx, `SELECT DISTINCT url, profile FROM content.snapshots WHERE tenant_id = $1 AND program_id = $2 ORDER BY url, profile`,
		t.tenant.String(), program.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []contentstore.Key
	for rows.Next() {
		k := contentstore.Key{Program: program}
		if err := rows.Scan(&k.URL, &k.Profile); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (t *pgTx) HostOpenVersions(ctx context.Context, program core.UUID, host, profile, url string, limit int) ([]contentstore.Snapshot, error) {
	return t.snapshots(ctx, `tenant_id = $1 AND program_id = $2 AND host = $3 AND profile = $4 AND url <> $5 AND valid_to IS NULL
		ORDER BY valid_from DESC, id DESC LIMIT $6`, t.tenant.String(), program.String(), host, profile, url, max(limit, 1))
}

func (t *pgTx) OpenVersionsByURL(ctx context.Context, program core.UUID, profile string, urls []string) ([]contentstore.Snapshot, error) {
	return t.snapshots(ctx, `tenant_id = $1 AND program_id = $2 AND profile = $3 AND url = ANY($4) AND valid_to IS NULL
		ORDER BY valid_from DESC, id DESC`, t.tenant.String(), program.String(), profile, urls)
}

func (t *pgTx) InsertSnapshot(ctx context.Context, s contentstore.Snapshot) error {
	_, err := t.tx.Exec(ctx, `INSERT INTO content.snapshots (id, tenant_id, program_id, entity_id, url, url_template, host, profile,
		run_id, effect_path, fetched_at, last_seen, valid_from, valid_to, status, mime, kind, signature, headers_ref, body_manifest,
		body_sha256, raw_size, stored_size, normalizer_version, normalized_manifest, parent_id, lineage_parent_id, features_ref,
		outcome, coverage, degraded, simhash, pinned, pin_reason, codec, delta_base, delta_depth, body_seal, headers_seal, key_id,
		created_at, derive_stamp)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24,
		$25, $26, $27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38, $39, $40, $41, $42)`,
		s.ID.String(), t.tenant.String(), s.Program.String(), s.EntityID, s.URL, s.URLTemplate, s.Host, s.Profile, s.RunID,
		s.EffectPath, s.FetchedAt, s.LastSeen, s.ValidFrom, nullTime(s.ValidTo), s.Status, s.Mime, s.Kind, s.Signature,
		s.HeadersRef, s.BodyManifest, s.BodySHA256, s.RawSize, s.StoredSize, s.NormalizerVersion, s.NormalizedManifest,
		nullUUID(s.ParentID), nullUUID(s.LineageParentID), s.FeaturesRef, s.Outcome, s.Coverage, s.Degraded, int64(s.SimHash),
		s.Pinned, s.PinReason, codecOf(s.Codec), nullUUID(s.DeltaBase), s.DeltaDepth, s.BodySeal, s.HeadersSeal, s.KeyID, s.CreatedAt,
		s.DeriveStamp)
	return err
}

func (t *pgTx) StaleSnapshots(ctx context.Context, program core.UUID, stamp string, after *contentstore.Cursor, limit int) ([]contentstore.Snapshot, error) {
	where := `tenant_id = $1 AND program_id = $2 AND derive_stamp <> $3`
	args := []any{t.tenant.String(), program.String(), stamp}
	if after != nil {
		where += ` AND (valid_from, id) > ($4, $5::uuid)`
		args = append(args, after.ValidFrom, after.ID.String())
	}
	return t.snapshots(ctx, where+` ORDER BY valid_from, id LIMIT `+strconv.Itoa(max(limit, 1)), args...)
}

func (t *pgTx) LockSnapshot(ctx context.Context, id core.UUID) (contentstore.Snapshot, bool, error) {
	return t.one(ctx, `tenant_id = $1 AND id = $2 FOR UPDATE`, t.tenant.String(), id.String())
}

func (t *pgTx) UpdateDerived(ctx context.Context, s contentstore.Snapshot) error {
	return t.exec1(ctx, "snapshot to re-derive not found", `UPDATE content.snapshots SET headers_ref = $3, body_manifest = $4,
		normalized_manifest = $5, features_ref = $6, codec = $7, delta_base = $8, delta_depth = $9, kind = $10,
		normalizer_version = $11, coverage = $12, simhash = $13, derive_stamp = $14 WHERE tenant_id = $1 AND id = $2`,
		t.tenant.String(), s.ID.String(), s.HeadersRef, s.BodyManifest, s.NormalizedManifest, s.FeaturesRef, codecOf(s.Codec),
		nullUUID(s.DeltaBase), s.DeltaDepth, s.Kind, s.NormalizerVersion, s.Coverage, int64(s.SimHash), s.DeriveStamp)
}

func codecOf(c string) string {
	if c == "" {
		return "chunk"
	}
	return c
}

func (t *pgTx) exec1(ctx context.Context, what string, sql string, args ...any) error {
	tag, err := t.tx.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return &core.Error{Kind: core.KindAborted, Reason: core.ReasonVersionConflict, Message: "contentstore/pg: " + what}
	}
	return nil
}

func (t *pgTx) CloseVersion(ctx context.Context, id core.UUID, validTo time.Time) error {
	return t.exec1(ctx, "snapshot is not an open version", `UPDATE content.snapshots SET valid_to = $3
		WHERE tenant_id = $1 AND id = $2 AND valid_to IS NULL`, t.tenant.String(), id.String(), validTo)
}

func (t *pgTx) Touch(ctx context.Context, id core.UUID, lastSeen time.Time) error {
	_, err := t.tx.Exec(ctx, `UPDATE content.snapshots SET last_seen = greatest(last_seen, $3) WHERE tenant_id = $1 AND id = $2`,
		t.tenant.String(), id.String(), lastSeen)
	return err
}

func (t *pgTx) SetPinned(ctx context.Context, id core.UUID, pinned bool, reason string) error {
	if !pinned {
		reason = ""
	}
	_, err := t.tx.Exec(ctx, `UPDATE content.snapshots SET pinned = $3, pin_reason = $4 WHERE tenant_id = $1 AND id = $2`,
		t.tenant.String(), id.String(), pinned, reason)
	return err
}

func idStrings(ids []core.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

func (t *pgTx) DeltaDependents(ctx context.Context, ids []core.UUID) ([]contentstore.Snapshot, error) {
	return t.snapshots(ctx, `tenant_id = $1 AND delta_base = ANY($2::uuid[]) ORDER BY valid_from DESC, id DESC`, t.tenant.String(), idStrings(ids))
}

func (t *pgTx) RewriteBlobs(ctx context.Context, id core.UUID, b contentstore.BlobRefs) error {
	return t.exec1(ctx, "snapshot to rewrite not found", `UPDATE content.snapshots SET headers_ref = $3, body_manifest = $4,
		normalized_manifest = $5, features_ref = $6, codec = $7, delta_base = $8, delta_depth = $9 WHERE tenant_id = $1 AND id = $2`,
		t.tenant.String(), id.String(), b.HeadersRef, b.BodyManifest, b.NormalizedManifest, b.FeaturesRef, codecOf(b.Codec),
		nullUUID(b.DeltaBase), b.DeltaDepth)
}

func (t *pgTx) DeleteSnapshots(ctx context.Context, ids []core.UUID) error {
	// Diffs and findings cascade; parent links are set NULL by their
	// foreign keys.
	_, err := t.tx.Exec(ctx, `DELETE FROM content.snapshots WHERE tenant_id = $1 AND id = ANY($2::uuid[])`, t.tenant.String(), idStrings(ids))
	return err
}

func (t *pgTx) NextDiffSeq(ctx context.Context, program core.UUID) (int64, error) {
	var seq int64
	err := t.tx.QueryRow(ctx, `UPDATE content.programs SET diff_seq = diff_seq + 1 WHERE tenant_id = $1 AND program_id = $2 RETURNING diff_seq`,
		t.tenant.String(), program.String()).Scan(&seq)
	return seq, err
}

const diffCols = `id::text, program_id::text, seq, from_snapshot::text, to_snapshot::text, levels, algorithm, normalizer_version,
	stats, hunks_ref, semantic_ref, stored_size, index_docs, at, url, profile, created_at`

func (t *pgTx) diffs(ctx context.Context, where string, args ...any) ([]contentstore.Diff, error) {
	rows, err := t.tx.Query(ctx, `SELECT `+diffCols+` FROM content.diffs WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []contentstore.Diff
	for rows.Next() {
		var d contentstore.Diff
		var id, prog, to string
		var from *string
		var stats []byte
		if err := rows.Scan(&id, &prog, &d.Seq, &from, &to, &d.Levels, &d.Algorithm, &d.NormalizerVersion, &stats, &d.HunksRef,
			&d.SemanticRef, &d.StoredSize, &d.IndexDocs, &d.At, &d.URL, &d.Profile, &d.CreatedAt); err != nil {
			return nil, err
		}
		var err error
		if d.ID, err = core.ParseUUID(id); err != nil {
			return nil, err
		}
		if d.Program, err = core.ParseUUID(prog); err != nil {
			return nil, err
		}
		if d.To, err = core.ParseUUID(to); err != nil {
			return nil, err
		}
		if d.From, err = parseNull(from); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(stats, &d.Stats); err != nil {
			return nil, err
		}
		d.Tenant, d.At, d.CreatedAt = t.tenant, d.At.UTC(), d.CreatedAt.UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

func (t *pgTx) InsertDiff(ctx context.Context, d contentstore.Diff) error {
	stats, err := json.Marshal(d.Stats)
	if err != nil {
		return err
	}
	levels := d.Levels
	if levels == nil {
		levels = []string{}
	}
	_, err = t.tx.Exec(ctx, `INSERT INTO content.diffs (id, tenant_id, program_id, seq, from_snapshot, to_snapshot, levels, algorithm,
		normalizer_version, stats, hunks_ref, semantic_ref, stored_size, index_docs, at, url, profile, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`,
		d.ID.String(), t.tenant.String(), d.Program.String(), d.Seq, nullUUID(d.From), d.To.String(), levels, d.Algorithm,
		d.NormalizerVersion, stats, d.HunksRef, d.SemanticRef, d.StoredSize, d.IndexDocs, d.At, d.URL, d.Profile, d.CreatedAt)
	return err
}

func (t *pgTx) RewriteDiff(ctx context.Context, d contentstore.Diff) error {
	stats, err := json.Marshal(d.Stats)
	if err != nil {
		return err
	}
	levels := d.Levels
	if levels == nil {
		levels = []string{}
	}
	return t.exec1(ctx, "diff to rewrite not found", `UPDATE content.diffs SET levels = $3, algorithm = $4, normalizer_version = $5,
		stats = $6, hunks_ref = $7, semantic_ref = $8, stored_size = $9, index_docs = $10 WHERE tenant_id = $1 AND id = $2`,
		t.tenant.String(), d.ID.String(), levels, d.Algorithm, d.NormalizerVersion, stats, d.HunksRef, d.SemanticRef, d.StoredSize,
		d.IndexDocs)
}

func (t *pgTx) oneDiff(ctx context.Context, where string, args ...any) (contentstore.Diff, bool, error) {
	ds, err := t.diffs(ctx, where, args...)
	if err != nil || len(ds) == 0 {
		return contentstore.Diff{}, false, err
	}
	return ds[0], true, nil
}

func (t *pgTx) GetDiff(ctx context.Context, id core.UUID) (contentstore.Diff, bool, error) {
	return t.oneDiff(ctx, `tenant_id = $1 AND id = $2`, t.tenant.String(), id.String())
}

func (t *pgTx) DiffInto(ctx context.Context, to core.UUID) (contentstore.Diff, bool, error) {
	return t.oneDiff(ctx, `tenant_id = $1 AND to_snapshot = $2 ORDER BY seq DESC LIMIT 1`, t.tenant.String(), to.String())
}

func (t *pgTx) DiffsAfter(ctx context.Context, program core.UUID, after int64, limit int) ([]contentstore.Diff, error) {
	return t.diffs(ctx, `tenant_id = $1 AND program_id = $2 AND seq > $3 ORDER BY seq LIMIT $4`, t.tenant.String(), program.String(),
		after, max(limit, 1))
}

func (t *pgTx) DiffsTouching(ctx context.Context, ids []core.UUID) ([]contentstore.Diff, error) {
	return t.diffs(ctx, `tenant_id = $1 AND (to_snapshot = ANY($2::uuid[]) OR from_snapshot = ANY($2::uuid[])) ORDER BY program_id, seq`,
		t.tenant.String(), idStrings(ids))
}

func (t *pgTx) InsertFindings(ctx context.Context, fs []contentstore.Finding) error {
	b := &pgx.Batch{}
	for _, f := range fs {
		b.Queue(`INSERT INTO content.findings (id, tenant_id, program_id, snapshot_id, rule_id, preview, keyed_hash, ciphertext, key_id,
			created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) ON CONFLICT (id) DO NOTHING`,
			f.ID.String(), t.tenant.String(), f.Program.String(), f.Snapshot.String(), f.RuleID, f.Preview, f.KeyedHash, f.Ciphertext,
			f.KeyID, f.CreatedAt)
	}
	return t.tx.SendBatch(ctx, b).Close()
}

func (t *pgTx) Findings(ctx context.Context, snapshot core.UUID) ([]contentstore.Finding, error) {
	rows, err := t.tx.Query(ctx, `SELECT id::text, program_id::text, rule_id, preview, keyed_hash, ciphertext, key_id, created_at
		FROM content.findings WHERE tenant_id = $1 AND snapshot_id = $2 ORDER BY id`, t.tenant.String(), snapshot.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []contentstore.Finding
	for rows.Next() {
		f := contentstore.Finding{Tenant: t.tenant, Snapshot: snapshot}
		var id, prog string
		if err := rows.Scan(&id, &prog, &f.RuleID, &f.Preview, &f.KeyedHash, &f.Ciphertext, &f.KeyID, &f.CreatedAt); err != nil {
			return nil, err
		}
		var err error
		if f.ID, err = core.ParseUUID(id); err != nil {
			return nil, err
		}
		if f.Program, err = core.ParseUUID(prog); err != nil {
			return nil, err
		}
		f.CreatedAt = f.CreatedAt.UTC()
		out = append(out, f)
	}
	return out, rows.Err()
}

func (t *pgTx) Volatility(ctx context.Context, program core.UUID, tmpl, profile string) ([]contentstore.VolatilityRow, error) {
	rows, err := t.tx.Query(ctx, `SELECT feature_path, changes, stable_observations, streak, frozen_version FROM content.volatility
		WHERE tenant_id = $1 AND program_id = $2 AND url_template = $3 AND profile = $4 ORDER BY feature_path`,
		t.tenant.String(), program.String(), tmpl, profile)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []contentstore.VolatilityRow
	for rows.Next() {
		v := contentstore.VolatilityRow{Program: program, URLTemplate: tmpl, Profile: profile}
		if err := rows.Scan(&v.Path, &v.Changes, &v.Stable, &v.Streak, &v.FrozenVersion); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (t *pgTx) PutVolatility(ctx context.Context, vs []contentstore.VolatilityRow) error {
	b := &pgx.Batch{}
	for _, v := range vs {
		b.Queue(`INSERT INTO content.volatility (tenant_id, program_id, url_template, profile, feature_path, changes,
			stable_observations, streak, frozen_version) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (tenant_id, program_id, url_template, profile, feature_path) DO UPDATE SET changes = EXCLUDED.changes,
			stable_observations = EXCLUDED.stable_observations, streak = EXCLUDED.streak, frozen_version = EXCLUDED.frozen_version`,
			t.tenant.String(), v.Program.String(), v.URLTemplate, v.Profile, v.Path, v.Changes, v.Stable, v.Streak, v.FrozenVersion)
	}
	return t.tx.SendBatch(ctx, b).Close()
}

// Usage locks the tenant's usage row for the rest of the transaction, so
// concurrent ingests check and count quotas one at a time (exactness).
func (t *pgTx) Usage(ctx context.Context) (contentstore.Usage, error) {
	if _, err := t.tx.Exec(ctx, `INSERT INTO content.quota_usage (tenant_id) VALUES ($1) ON CONFLICT DO NOTHING`, t.tenant.String()); err != nil {
		return contentstore.Usage{}, err
	}
	var u contentstore.Usage
	err := t.tx.QueryRow(ctx, `SELECT day, captures, stored_bytes, index_bytes FROM content.quota_usage WHERE tenant_id = $1 FOR UPDATE`,
		t.tenant.String()).Scan(&u.Day, &u.Captures, &u.StoredBytes, &u.IndexBytes)
	return u, err
}

func (t *pgTx) PutUsage(ctx context.Context, u contentstore.Usage) error {
	_, err := t.tx.Exec(ctx, `INSERT INTO content.quota_usage (tenant_id, day, captures, stored_bytes, index_bytes) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id) DO UPDATE SET day = EXCLUDED.day, captures = EXCLUDED.captures, stored_bytes = EXCLUDED.stored_bytes,
		index_bytes = EXCLUDED.index_bytes`, t.tenant.String(), u.Day, u.Captures, u.StoredBytes, u.IndexBytes)
	return err
}

func (t *pgTx) Limits(ctx context.Context) (contentstore.Limits, bool, error) {
	var l contentstore.Limits
	err := t.tx.QueryRow(ctx, `SELECT captures_per_day, stored_bytes, index_bytes FROM content.quota_limits WHERE tenant_id = $1`,
		t.tenant.String()).Scan(&l.CapturesPerDay, &l.StoredBytes, &l.IndexBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return contentstore.Limits{}, false, nil
	}
	return l, err == nil, err
}

func (t *pgTx) PutLimits(ctx context.Context, l contentstore.Limits) error {
	_, err := t.tx.Exec(ctx, `INSERT INTO content.quota_limits (tenant_id, captures_per_day, stored_bytes, index_bytes) VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id) DO UPDATE SET captures_per_day = EXCLUDED.captures_per_day, stored_bytes = EXCLUDED.stored_bytes,
		index_bytes = EXCLUDED.index_bytes`, t.tenant.String(), l.CapturesPerDay, l.StoredBytes, l.IndexBytes)
	return err
}

func (t *pgTx) IndexState(ctx context.Context, program core.UUID) (contentstore.IndexState, error) {
	s := contentstore.IndexState{Program: program}
	err := t.tx.QueryRow(ctx, `SELECT watermark, docs, bytes FROM content.index_state WHERE tenant_id = $1 AND program_id = $2`,
		t.tenant.String(), program.String()).Scan(&s.Watermark, &s.Docs, &s.Bytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, nil
	}
	return s, err
}

func (t *pgTx) PutIndexState(ctx context.Context, s contentstore.IndexState) error {
	_, err := t.tx.Exec(ctx, `INSERT INTO content.index_state (tenant_id, program_id, watermark, docs, bytes) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, program_id) DO UPDATE SET watermark = EXCLUDED.watermark, docs = EXCLUDED.docs, bytes = EXCLUDED.bytes`,
		t.tenant.String(), s.Program.String(), s.Watermark, s.Docs, s.Bytes)
	return err
}

func (t *pgTx) DataKeys(ctx context.Context) ([]contentstore.DataKey, error) {
	rows, err := t.tx.Query(ctx, `SELECT purpose, version, kek_id, wrapped, created_at FROM content.data_keys WHERE tenant_id = $1
		ORDER BY purpose, version`, t.tenant.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []contentstore.DataKey
	for rows.Next() {
		k := contentstore.DataKey{Tenant: t.tenant}
		if err := rows.Scan(&k.Purpose, &k.Version, &k.KEKID, &k.Wrapped, &k.CreatedAt); err != nil {
			return nil, err
		}
		k.CreatedAt = k.CreatedAt.UTC()
		out = append(out, k)
	}
	return out, rows.Err()
}

func (t *pgTx) InsertDataKey(ctx context.Context, k contentstore.DataKey) (bool, error) {
	tag, err := t.tx.Exec(ctx, `INSERT INTO content.data_keys (tenant_id, purpose, version, kek_id, wrapped, created_at)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`, t.tenant.String(), k.Purpose, k.Version, k.KEKID, k.Wrapped, k.CreatedAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (t *pgTx) AppendOutbox(ctx context.Context, e contentstore.OutboxEvent) error {
	_, err := t.tx.Exec(ctx, `INSERT INTO content.outbox (event_id, topic, ordering_key, payload) VALUES ($1, $2, $3, $4)
		ON CONFLICT (event_id) DO NOTHING`, e.EventID.String(), e.Topic, e.OrderingKey, string(e.Payload))
	return err
}

func (t *pgTx) Manifests(ctx context.Context) (live, pinned []string, err error) {
	rows, err := t.tx.Query(ctx, `SELECT r, p FROM (
		SELECT unnest(ARRAY[headers_ref, body_manifest, normalized_manifest, features_ref]) AS r, pinned AS p
		FROM content.snapshots WHERE tenant_id = $1
		UNION ALL SELECT unnest(ARRAY[hunks_ref, semantic_ref]), false FROM content.diffs WHERE tenant_id = $1) m
		WHERE r <> '' ORDER BY r`, t.tenant.String())
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r string
		var p bool
		if err := rows.Scan(&r, &p); err != nil {
			return nil, nil, err
		}
		if len(live) == 0 || live[len(live)-1] != r {
			live = append(live, r)
		}
		if p && (len(pinned) == 0 || pinned[len(pinned)-1] != r) {
			pinned = append(pinned, r)
		}
	}
	return live, pinned, rows.Err()
}
