-- 0013_engine_extras: the engine tables beyond spec 10.4 that engine/pgrepo
-- needs (see engine/OPEN_QUESTIONS.md 19). This is engine/pgrepo.SchemaSQL
-- verbatim; every statement is idempotent, so pgrepo.EnsureSchema (kept as
-- a bootstrap for databases migrated before this file existed) and this
-- migration can both run in either order.
--
-- * workflow_version_ext: the registering actor, the program the version was
--   checked against and the workflow attributes.
-- * run_ext: the fine-grained run status (spec 9.2) and the outbox watermark.
-- * run_requests: cancel / resolve_unknown / scope_changed requests queued for
--   the lease holder.
-- * approval_ext: the deciding actor (approvals.decided_by is a uuid).
CREATE TABLE IF NOT EXISTS engine.workflow_version_ext (
  workflow_version_id uuid PRIMARY KEY REFERENCES engine.workflow_versions(id) ON DELETE CASCADE,
  created_by_actor text NOT NULL, program_id uuid, attributes jsonb NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS engine.run_ext (
  run_id uuid PRIMARY KEY REFERENCES engine.runs(id),
  status text NOT NULL CHECK (status IN ('pending','running','waiting','paused','cancelling','compensating','succeeded','failed','cancelled')),
  published_seq bigint NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS engine.run_requests (
  id bigserial PRIMARY KEY, run_id uuid NOT NULL REFERENCES engine.runs(id),
  kind text NOT NULL CHECK (kind IN ('cancel','resolve_unknown','scope_changed')),
  actor text NOT NULL, reason text NOT NULL DEFAULT '', path text NOT NULL DEFAULT '',
  resolution text NOT NULL DEFAULT '', result jsonb, new_version integer NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now(), applied_at timestamptz
);
CREATE INDEX IF NOT EXISTS run_requests_pending ON engine.run_requests (run_id, id) WHERE applied_at IS NULL;
CREATE TABLE IF NOT EXISTS engine.approval_ext (
  approval_id uuid PRIMARY KEY REFERENCES engine.approvals(id), decided_actor text NOT NULL
);
