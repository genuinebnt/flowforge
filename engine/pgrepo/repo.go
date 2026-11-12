package pgrepo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
)

// DB is the subset of a connection pool the repository uses;
// *pgxpool.Pool satisfies it.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Repo is engine.Repo over the engine schema (spec 10.4) plus SchemaSQL.
// It shares engine.runs with eventstore/pgstore, which creates the run
// rows and owns their scheduling columns (status, lease, next_wake_at,
// last_seq); Repo writes the remaining projection columns. Use it with a
// pgstore.Store on the same database.
//
// Connect with a role that bypasses row-level security (spec 10.7, the
// engine scheduler path) or set app.tenant_id per transaction; every
// query also filters by tenant explicitly.
type Repo struct {
	db DB
}

var _ engine.Repo = (*Repo)(nil)

// New returns a repository over db.
func New(db DB) *Repo { return &Repo{db: db} }

func pgUUID(u core.UUID) pgtype.UUID { return pgtype.UUID{Bytes: u, Valid: true} }

func pgOptUUID(u core.UUID) pgtype.UUID { return pgtype.UUID{Bytes: u, Valid: !u.IsZero()} }

func pgOptTime(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: !t.IsZero()}
}

func pgOptText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func fromUUID(u pgtype.UUID) core.UUID {
	if !u.Valid {
		return core.Nil
	}
	return u.Bytes
}

func fromTime(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time.UTC()
}

// actorUUID extracts the uuid of "user:<uuid>" style actors (the uuid
// columns of spec 10.4); other actors map to NULL.
func actorUUID(actor string) pgtype.UUID {
	_, id, _ := strings.Cut(actor, ":")
	u, err := core.ParseUUID(id)
	if err != nil {
		return pgtype.UUID{}
	}
	return pgUUID(u)
}

func mapErr(op string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return engine.ErrNotFound
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return fmt.Errorf("pgrepo: %s: %w: %s", op, engine.ErrConflict, pe.Message)
	}
	if err != nil {
		return fmt.Errorf("pgrepo: %s: %w", op, err)
	}
	return nil
}

func (r *Repo) inTx(ctx context.Context, fn func(pgx.Tx) error) (err error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// tenantCond is the "($n::uuid IS NULL OR tenant_id = $n)" filter: the
// zero tenant (system scope) sees every tenant.
func tenantCond(col string, n int) string {
	return fmt.Sprintf("($%d::uuid IS NULL OR %s = $%d)", n, col, n)
}

// ---- workflows ----

const versionCols = `v.id, w.id, w.tenant_id, w.name, v.version, v.status, v.source, v.source_sha256,
  v.module, v.module_sha256, v.effects, v.inferred_class, v.diagnostics, v.created_at,
  COALESCE(x.created_by_actor, v.created_by::text), x.program_id, COALESCE(x.attributes, '{}')`

const versionFrom = ` FROM engine.workflow_versions v JOIN engine.workflows w ON w.id = v.workflow_id
  LEFT JOIN engine.workflow_version_ext x ON x.workflow_version_id = v.id`

func scanVersion(row pgx.Row) (engine.WorkflowVersion, error) {
	var v engine.WorkflowVersion
	var id, wf, tenant, program pgtype.UUID
	var srcSHA, modSHA, diags, attrs []byte
	var class string
	var created pgtype.Timestamptz
	if err := row.Scan(&id, &wf, &tenant, &v.Name, &v.Version, &v.Status, &v.Source, &srcSHA, &v.Module, &modSHA,
		&v.Effects, &class, &diags, &created, &v.CreatedBy, &program, &attrs); err != nil {
		return v, err
	}
	v.ID, v.WorkflowID, v.Tenant, v.ProgramID = fromUUID(id), fromUUID(wf), fromUUID(tenant), fromUUID(program)
	copy(v.SourceSHA256[:], srcSHA)
	copy(v.ModuleSHA256[:], modSHA)
	v.InferredClass, _ = core.ParseActivityClass(class)
	v.CreatedAt = fromTime(created)
	if err := json.Unmarshal(diags, &v.Diagnostics); err != nil {
		return v, err
	}
	if err := json.Unmarshal(attrs, &v.Attributes); err != nil {
		return v, err
	}
	return v, nil
}

// CreateWorkflowVersion implements engine.Repo.
func (r *Repo) CreateWorkflowVersion(ctx context.Context, v engine.WorkflowVersion) (engine.WorkflowVersion, error) {
	diags, err := json.Marshal(v.Diagnostics)
	if err != nil {
		return v, err
	}
	attrs, err := json.Marshal(v.Attributes)
	if err != nil {
		return v, err
	}
	if v.Effects == nil {
		v.Effects = []string{}
	}
	err = r.inTx(ctx, func(tx pgx.Tx) error {
		var wf pgtype.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO engine.workflows (id, tenant_id, name) VALUES ($1, $2, $3)
ON CONFLICT (tenant_id, name) DO UPDATE SET name = EXCLUDED.name RETURNING id`,
			pgUUID(derivedWorkflowID(v.Tenant, v.Name)), pgUUID(v.Tenant), v.Name).Scan(&wf); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM engine.workflows WHERE id = $1 FOR UPDATE`, wf); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) + 1 FROM engine.workflow_versions WHERE workflow_id = $1`, wf).Scan(&v.Version); err != nil {
			return err
		}
		created := actorUUID(v.CreatedBy)
		if !created.Valid {
			created = pgUUID(core.Nil)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO engine.workflow_versions (id, workflow_id, version, source, source_sha256,
  module, module_sha256, effects, inferred_class, status, diagnostics, created_by, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::text::jsonb, $12, $13)`,
			pgUUID(v.ID), wf, v.Version, v.Source, v.SourceSHA256[:], v.Module, v.ModuleSHA256[:], v.Effects,
			v.InferredClass.String(), v.Status, string(diags), created, pgOptTime(v.CreatedAt)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO engine.workflow_version_ext (workflow_version_id, created_by_actor, program_id, attributes)
VALUES ($1, $2, $3, $4::text::jsonb)`, pgUUID(v.ID), v.CreatedBy, pgOptUUID(v.ProgramID), string(attrs))
		v.WorkflowID = fromUUID(wf)
		return err
	})
	return v, mapErr("create workflow version", err)
}

// derivedWorkflowID makes workflow ids deterministic per (tenant, name),
// so concurrent first registrations agree.
func derivedWorkflowID(tenant core.UUID, name string) core.UUID {
	return engine.DerivedID("workflow", tenant.String(), name)
}

// GetWorkflowVersion implements engine.Repo.
func (r *Repo) GetWorkflowVersion(ctx context.Context, tenant core.UUID, name string, n int32) (engine.WorkflowVersion, error) {
	v, err := scanVersion(r.db.QueryRow(ctx, `SELECT `+versionCols+versionFrom+`
WHERE w.tenant_id = $1 AND w.name = $2 AND (v.version = $3 OR ($3 = 0 AND v.status = 'published'))
ORDER BY v.version DESC LIMIT 1`, pgUUID(tenant), name, n))
	return v, mapErr("get workflow version", err)
}

// GetWorkflowVersionByID implements engine.Repo.
func (r *Repo) GetWorkflowVersionByID(ctx context.Context, tenant, id core.UUID) (engine.WorkflowVersion, error) {
	v, err := scanVersion(r.db.QueryRow(ctx, `SELECT `+versionCols+versionFrom+`
WHERE v.id = $1 AND `+tenantCond("w.tenant_id", 2), pgUUID(id), pgOptUUID(tenant)))
	return v, mapErr("get workflow version", err)
}

// ListWorkflows implements engine.Repo.
func (r *Repo) ListWorkflows(ctx context.Context, tenant core.UUID, afterName string, limit int) ([]engine.WorkflowVersion, error) {
	rows, err := r.db.Query(ctx, `SELECT DISTINCT ON (w.name) `+versionCols+versionFrom+`
WHERE w.tenant_id = $1 AND w.name > $2 ORDER BY w.name, v.version DESC LIMIT $3`, pgUUID(tenant), afterName, limit)
	if err != nil {
		return nil, mapErr("list workflows", err)
	}
	defer rows.Close()
	var out []engine.WorkflowVersion
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, mapErr("list workflows", err)
		}
		out = append(out, v)
	}
	return out, mapErr("list workflows", rows.Err())
}

// SetWorkflowVersionStatus implements engine.Repo.
func (r *Repo) SetWorkflowVersionStatus(ctx context.Context, tenant core.UUID, name string, n int32, status string) (engine.WorkflowVersion, error) {
	tag, err := r.db.Exec(ctx, `UPDATE engine.workflow_versions v SET status = $4 FROM engine.workflows w
WHERE w.id = v.workflow_id AND w.tenant_id = $1 AND w.name = $2 AND v.version = $3`, pgUUID(tenant), name, n, status)
	if err != nil {
		return engine.WorkflowVersion{}, mapErr("set workflow version status", err)
	}
	if tag.RowsAffected() == 0 {
		return engine.WorkflowVersion{}, engine.ErrNotFound
	}
	return r.GetWorkflowVersion(ctx, tenant, name, n)
}

// ---- runs ----

const runCols = `r.id, r.tenant_id, r.program_id, r.workflow_version_id, w.name, v.version, r.parent_run_id,
  COALESCE(r.scope_version, 0), COALESCE(x.status, r.status), r.compensation_outcome, r.input, r.output,
  COALESCE(r.failure_code, ''), COALESCE(r.failure_message, ''), r.created_by, r.idempotency_key, r.request_hash,
  r.priority, r.deadline_at, r.created_at, r.started_at, r.finished_at, r.last_seq, COALESCE(x.published_seq, 0)`

const runFrom = ` FROM engine.runs r JOIN engine.workflow_versions v ON v.id = r.workflow_version_id
  JOIN engine.workflows w ON w.id = v.workflow_id LEFT JOIN engine.run_ext x ON x.run_id = r.id`

func scanRun(row pgx.Row) (engine.RunRecord, error) {
	var rec engine.RunRecord
	var id, tenant, program, wfv, parent pgtype.UUID
	var status string
	var input, output []byte
	var deadline, created, started, finished pgtype.Timestamptz
	var lastSeq, pubSeq int64
	if err := row.Scan(&id, &tenant, &program, &wfv, &rec.WorkflowName, &rec.WorkflowVersion, &parent,
		&rec.ScopeVersion, &status, &rec.CompensationOutcome, &input, &output, &rec.FailureCode, &rec.FailureMessage,
		&rec.CreatedBy, &rec.IdempotencyKey, &rec.RequestHash, &rec.Priority, &deadline, &created, &started, &finished,
		&lastSeq, &pubSeq); err != nil {
		return rec, err
	}
	rec.ID, rec.Tenant, rec.ProgramID, rec.WorkflowVersionID, rec.ParentRunID =
		fromUUID(id), fromUUID(tenant), fromUUID(program), fromUUID(wfv), fromUUID(parent)
	rec.Status = engine.RunStatus(status)
	rec.Input, rec.Output = input, output
	rec.DeadlineAt, rec.CreatedAt, rec.StartedAt, rec.FinishedAt = fromTime(deadline), fromTime(created), fromTime(started), fromTime(finished)
	rec.LastSeq, rec.PublishedSeq = uint64(lastSeq), uint64(pubSeq)
	return rec, nil
}

// InsertRun implements engine.Repo: pgstore created the row; this adds
// the run_ext projection.
func (r *Repo) InsertRun(ctx context.Context, rec engine.RunRecord) error {
	_, err := r.db.Exec(ctx, `INSERT INTO engine.run_ext (run_id, status) VALUES ($1, $2) ON CONFLICT (run_id) DO NOTHING`,
		pgUUID(rec.ID), string(rec.Status))
	return mapErr("insert run", err)
}

// GetRun implements engine.Repo.
func (r *Repo) GetRun(ctx context.Context, tenant, id core.UUID) (engine.RunRecord, error) {
	rec, err := scanRun(r.db.QueryRow(ctx, `SELECT `+runCols+runFrom+` WHERE r.id = $1 AND `+tenantCond("r.tenant_id", 2),
		pgUUID(id), pgOptUUID(tenant)))
	return rec, mapErr("get run", err)
}

// FindRunByIdempotencyKey implements engine.Repo.
func (r *Repo) FindRunByIdempotencyKey(ctx context.Context, tenant core.UUID, key string) (engine.RunRecord, error) {
	rec, err := scanRun(r.db.QueryRow(ctx, `SELECT `+runCols+runFrom+` WHERE r.tenant_id = $1 AND r.idempotency_key = $2`,
		pgUUID(tenant), key))
	return rec, mapErr("find run", err)
}

func (r *Repo) queryRuns(ctx context.Context, op, where string, args ...any) ([]engine.RunRecord, error) {
	rows, err := r.db.Query(ctx, `SELECT `+runCols+runFrom+` WHERE `+where, args...)
	if err != nil {
		return nil, mapErr(op, err)
	}
	defer rows.Close()
	var out []engine.RunRecord
	for rows.Next() {
		rec, err := scanRun(rows)
		if err != nil {
			return nil, mapErr(op, err)
		}
		out = append(out, rec)
	}
	return out, mapErr(op, rows.Err())
}

// ListRuns implements engine.Repo.
func (r *Repo) ListRuns(ctx context.Context, tenant core.UUID, f engine.RunFilter, after engine.RunCursor, limit int) ([]engine.RunRecord, error) {
	return r.queryRuns(ctx, "list runs", tenantCond("r.tenant_id", 1)+`
  AND ($2::uuid IS NULL OR r.program_id = $2)
  AND ($3::text IS NULL OR COALESCE(x.status, r.status) = $3)
  AND ($4::text IS NULL OR w.name = $4)
  AND ($5::timestamptz IS NULL OR r.created_at > $5)
  AND ($6::uuid IS NULL OR (r.created_at, r.id) < ($7, $6))
ORDER BY r.created_at DESC, r.id DESC LIMIT $8`,
		pgOptUUID(tenant), pgOptUUID(f.ProgramID), pgOptText(string(f.Status)), pgOptText(f.WorkflowName),
		pgOptTime(f.CreatedAfter), pgOptUUID(after.ID), pgOptTime(after.CreatedAt), limit)
}

// ListActiveRuns implements engine.Repo.
func (r *Repo) ListActiveRuns(ctx context.Context, tenant, program core.UUID) ([]engine.RunRecord, error) {
	return r.queryRuns(ctx, "list active runs", tenantCond("r.tenant_id", 1)+`
  AND ($2::uuid IS NULL OR r.program_id = $2)
  AND COALESCE(x.status, r.status) NOT IN ('succeeded', 'failed', 'cancelled') ORDER BY r.id`,
		pgOptUUID(tenant), pgOptUUID(program))
}

// UpdateRun implements engine.Repo. The scheduling columns (status,
// last_seq, lease, wake) belong to pgstore and are not written.
func (r *Repo) UpdateRun(ctx context.Context, rec engine.RunRecord) error {
	var output pgtype.Text
	if rec.Output != nil {
		output = pgtype.Text{String: string(rec.Output), Valid: true}
	}
	var sv pgtype.Int4
	if rec.ScopeVersion != 0 {
		sv = pgtype.Int4{Int32: rec.ScopeVersion, Valid: true}
	}
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE engine.runs SET compensation_outcome = $2, output = $3::text::jsonb,
  failure_code = $4, failure_message = $5, scope_version = COALESCE($6, scope_version),
  started_at = $7, finished_at = $8 WHERE id = $1`,
			pgUUID(rec.ID), rec.CompensationOutcome, output, pgOptText(rec.FailureCode), pgOptText(rec.FailureMessage),
			sv, pgOptTime(rec.StartedAt), pgOptTime(rec.FinishedAt))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return engine.ErrNotFound
		}
		_, err = tx.Exec(ctx, `INSERT INTO engine.run_ext (run_id, status, published_seq) VALUES ($1, $2, $3)
ON CONFLICT (run_id) DO UPDATE SET status = EXCLUDED.status, published_seq = GREATEST(engine.run_ext.published_seq, EXCLUDED.published_seq)`,
			pgUUID(rec.ID), string(rec.Status), int64(rec.PublishedSeq))
		return err
	})
	if errors.Is(err, engine.ErrNotFound) {
		return err
	}
	return mapErr("update run", err)
}

// ---- run requests ----

// AddRunRequest implements engine.Repo.
func (r *Repo) AddRunRequest(ctx context.Context, q engine.RunRequest) (engine.RunRequest, error) {
	var result pgtype.Text
	if len(q.Result) > 0 {
		result = pgtype.Text{String: string(q.Result), Valid: true}
	}
	err := r.db.QueryRow(ctx, `INSERT INTO engine.run_requests (run_id, kind, actor, reason, path, resolution, result, new_version, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7::text::jsonb, $8, COALESCE($9, now())) RETURNING id`,
		pgUUID(q.RunID), q.Kind, q.Actor, q.Reason, q.Path, q.Resolution, result, q.NewVersion, pgOptTime(q.CreatedAt)).Scan(&q.ID)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23503" {
		return q, engine.ErrNotFound
	}
	return q, mapErr("add run request", err)
}

// PendingRunRequests implements engine.Repo.
func (r *Repo) PendingRunRequests(ctx context.Context, run core.UUID) ([]engine.RunRequest, error) {
	rows, err := r.db.Query(ctx, `SELECT id, kind, actor, reason, path, resolution, result, new_version, created_at
FROM engine.run_requests WHERE run_id = $1 AND applied_at IS NULL ORDER BY id`, pgUUID(run))
	if err != nil {
		return nil, mapErr("pending run requests", err)
	}
	defer rows.Close()
	var out []engine.RunRequest
	for rows.Next() {
		q := engine.RunRequest{RunID: run}
		var result []byte
		var created pgtype.Timestamptz
		if err := rows.Scan(&q.ID, &q.Kind, &q.Actor, &q.Reason, &q.Path, &q.Resolution, &result, &q.NewVersion, &created); err != nil {
			return nil, mapErr("pending run requests", err)
		}
		q.Result, q.CreatedAt = result, fromTime(created)
		out = append(out, q)
	}
	return out, mapErr("pending run requests", rows.Err())
}

// MarkRunRequestsApplied implements engine.Repo.
func (r *Repo) MarkRunRequestsApplied(ctx context.Context, run core.UUID, ids []int64) error {
	_, err := r.db.Exec(ctx, `UPDATE engine.run_requests SET applied_at = now() WHERE run_id = $1 AND id = ANY($2) AND applied_at IS NULL`,
		pgUUID(run), ids)
	return mapErr("mark run requests applied", err)
}

// ---- approvals ----

const approvalCols = `a.id, a.tenant_id, a.run_id, a.effect_path, a.role, a.reason, a.proposed, a.status, a.expires_at,
  COALESCE(a.escalate_role, ''), COALESCE(x.decided_actor, a.decided_by::text, ''), a.decided_at, COALESCE(a.comment, ''), a.created_at`

const approvalFrom = ` FROM engine.approvals a LEFT JOIN engine.approval_ext x ON x.approval_id = a.id`

func scanApproval(row pgx.Row) (engine.Approval, error) {
	var a engine.Approval
	var id, tenant, run pgtype.UUID
	var expires, decided, created pgtype.Timestamptz
	var proposed []byte
	if err := row.Scan(&id, &tenant, &run, &a.EffectPath, &a.Role, &a.Reason, &proposed, &a.Status, &expires,
		&a.EscalateRole, &a.DecidedBy, &decided, &a.Comment, &created); err != nil {
		return a, err
	}
	a.ID, a.Tenant, a.RunID = fromUUID(id), fromUUID(tenant), fromUUID(run)
	a.Proposed = proposed
	a.ExpiresAt, a.DecidedAt, a.CreatedAt = fromTime(expires), fromTime(decided), fromTime(created)
	return a, nil
}

// UpsertApproval implements engine.Repo.
func (r *Repo) UpsertApproval(ctx context.Context, a engine.Approval) (engine.Approval, error) {
	proposed := string(a.Proposed)
	if proposed == "" {
		proposed = "{}"
	}
	if _, err := r.db.Exec(ctx, `INSERT INTO engine.approvals (id, tenant_id, run_id, effect_path, role, reason, proposed,
  status, expires_at, escalate_role, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7::text::jsonb, 'pending', $8, $9, COALESCE($10, now()))
ON CONFLICT (run_id, effect_path) DO NOTHING`,
		pgUUID(a.ID), pgUUID(a.Tenant), pgUUID(a.RunID), a.EffectPath, a.Role, a.Reason, proposed,
		a.ExpiresAt.UTC(), pgOptText(a.EscalateRole), pgOptTime(a.CreatedAt)); err != nil {
		return a, mapErr("upsert approval", err)
	}
	got, err := scanApproval(r.db.QueryRow(ctx, `SELECT `+approvalCols+approvalFrom+` WHERE a.run_id = $1 AND a.effect_path = $2`,
		pgUUID(a.RunID), a.EffectPath))
	return got, mapErr("upsert approval", err)
}

// GetApproval implements engine.Repo.
func (r *Repo) GetApproval(ctx context.Context, tenant, id core.UUID) (engine.Approval, error) {
	a, err := scanApproval(r.db.QueryRow(ctx, `SELECT `+approvalCols+approvalFrom+` WHERE a.id = $1 AND `+tenantCond("a.tenant_id", 2),
		pgUUID(id), pgOptUUID(tenant)))
	return a, mapErr("get approval", err)
}

// ListApprovals implements engine.Repo.
func (r *Repo) ListApprovals(ctx context.Context, tenant core.UUID, f engine.ApprovalFilter, after core.UUID, limit int) ([]engine.Approval, error) {
	rows, err := r.db.Query(ctx, `SELECT `+approvalCols+approvalFrom+` WHERE `+tenantCond("a.tenant_id", 1)+`
  AND ($2::text IS NULL OR a.status = $2) AND ($3::text IS NULL OR a.role = $3) AND ($4::uuid IS NULL OR a.run_id = $4)
  AND ($5::uuid IS NULL OR (a.created_at, a.id) > (SELECT created_at, id FROM engine.approvals WHERE id = $5))
ORDER BY a.created_at, a.id LIMIT $6`,
		pgOptUUID(tenant), pgOptText(f.Status), pgOptText(f.Role), pgOptUUID(f.RunID), pgOptUUID(after), limit)
	if err != nil {
		return nil, mapErr("list approvals", err)
	}
	defer rows.Close()
	var out []engine.Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, mapErr("list approvals", err)
		}
		out = append(out, a)
	}
	return out, mapErr("list approvals", rows.Err())
}

// DecideApproval implements engine.Repo (first decision wins: the update
// only matches a pending row).
func (r *Repo) DecideApproval(ctx context.Context, tenant, id core.UUID, status, actor, comment string, at time.Time) (engine.Approval, bool, error) {
	changed := false
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE engine.approvals SET status = $3, decided_by = $4, decided_at = $5, comment = $6
WHERE id = $1 AND `+tenantCond("tenant_id", 2)+` AND status = 'pending'`,
			pgUUID(id), pgOptUUID(tenant), status, actorUUID(actor), at.UTC(), comment)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		changed = true
		_, err = tx.Exec(ctx, `INSERT INTO engine.approval_ext (approval_id, decided_actor) VALUES ($1, $2)
ON CONFLICT (approval_id) DO UPDATE SET decided_actor = EXCLUDED.decided_actor`, pgUUID(id), actor)
		return err
	})
	if err != nil {
		return engine.Approval{}, false, mapErr("decide approval", err)
	}
	a, err := r.GetApproval(ctx, tenant, id)
	return a, changed, err
}

// ---- timers and effects ----

// UpsertTimer implements engine.Repo.
func (r *Repo) UpsertTimer(ctx context.Context, t engine.Timer) (engine.Timer, error) {
	var fire, fired pgtype.Timestamptz
	err := r.db.QueryRow(ctx, `WITH ins AS (
  INSERT INTO engine.timers (id, run_id, path, fire_at) VALUES ($1, $2, $3, $4) ON CONFLICT (id) DO NOTHING RETURNING fire_at, fired_at)
SELECT fire_at, fired_at FROM ins UNION ALL SELECT fire_at, fired_at FROM engine.timers WHERE id = $1 LIMIT 1`,
		pgUUID(t.ID), pgUUID(t.RunID), t.Path, t.FireAt.UTC()).Scan(&fire, &fired)
	if err != nil {
		return t, mapErr("upsert timer", err)
	}
	t.FireAt, t.Fired = fromTime(fire), fromTime(fired)
	return t, nil
}

// FireTimer implements engine.Repo.
func (r *Repo) FireTimer(ctx context.Context, id core.UUID, at time.Time) error {
	tag, err := r.db.Exec(ctx, `UPDATE engine.timers SET fired_at = COALESCE(fired_at, $2) WHERE id = $1`, pgUUID(id), at.UTC())
	if err != nil {
		return mapErr("fire timer", err)
	}
	if tag.RowsAffected() == 0 {
		return engine.ErrNotFound
	}
	return nil
}

func optJSON(b json.RawMessage) pgtype.Text {
	if len(b) == 0 {
		return pgtype.Text{}
	}
	return pgtype.Text{String: string(b), Valid: true}
}

// UpsertEffect implements engine.Repo.
func (r *Repo) UpsertEffect(ctx context.Context, e engine.EffectRecord) error {
	args := e.ArgsHash
	if args == nil {
		args = []byte{}
	}
	_, err := r.db.Exec(ctx, `INSERT INTO engine.effects (run_id, path, seq, kind, args_hash, state, attempt, idempotency_key,
  scheduled_at, started_at, completed_at, next_retry_at, result, error)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, COALESCE($9, now()), $10, $11, $12, $13::text::jsonb, $14::text::jsonb)
ON CONFLICT (run_id, path) DO UPDATE SET state = EXCLUDED.state, attempt = EXCLUDED.attempt,
  started_at = EXCLUDED.started_at, completed_at = EXCLUDED.completed_at, next_retry_at = EXCLUDED.next_retry_at,
  result = EXCLUDED.result, error = EXCLUDED.error`,
		pgUUID(e.RunID), e.Path, int64(e.Seq), e.Kind, args, e.State, e.Attempt, e.IdempotencyKey,
		pgOptTime(e.ScheduledAt), pgOptTime(e.StartedAt), pgOptTime(e.CompletedAt), pgOptTime(e.NextRetryAt),
		optJSON(e.Result), optJSON(e.Error))
	return mapErr("upsert effect", err)
}

// ListEffects implements engine.Repo.
func (r *Repo) ListEffects(ctx context.Context, run core.UUID) ([]engine.EffectRecord, error) {
	rows, err := r.db.Query(ctx, `SELECT path, seq, kind, args_hash, state, attempt, idempotency_key, scheduled_at,
  started_at, completed_at, next_retry_at, result, error FROM engine.effects WHERE run_id = $1 ORDER BY seq`, pgUUID(run))
	if err != nil {
		return nil, mapErr("list effects", err)
	}
	defer rows.Close()
	var out []engine.EffectRecord
	for rows.Next() {
		e := engine.EffectRecord{RunID: run}
		var seq int64
		var sched, started, completed, retry pgtype.Timestamptz
		var result, eerr []byte
		if err := rows.Scan(&e.Path, &seq, &e.Kind, &e.ArgsHash, &e.State, &e.Attempt, &e.IdempotencyKey,
			&sched, &started, &completed, &retry, &result, &eerr); err != nil {
			return nil, mapErr("list effects", err)
		}
		e.Seq = uint64(seq)
		e.ScheduledAt, e.StartedAt, e.CompletedAt, e.NextRetryAt = fromTime(sched), fromTime(started), fromTime(completed), fromTime(retry)
		e.Result, e.Error = result, eerr
		out = append(out, e)
	}
	return out, mapErr("list effects", rows.Err())
}
