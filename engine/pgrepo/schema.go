package pgrepo

import (
	"context"
	"fmt"
)

// SchemaSQL creates the tables the engine needs beyond spec 10.4. It is a
// proposed migration (0013_engine_ext); until it moves to the migrations
// package, EnsureSchema applies it. Every statement is idempotent.
//
//   - workflow_version_ext: the registering actor (workflow_versions.
//     created_by is a uuid and cannot hold "apikey:<id>"), the program the
//     version was checked against, and the workflow attributes.
//   - run_ext: the fine-grained run status of spec 9.2 (engine.runs.status
//     is the scheduler's coarse status, which pgstore owns) and the outbox
//     watermark.
//   - run_requests: cancellation, unknown-outcome resolutions and scope
//     changes queued for the worker that holds the run's lease (only the
//     lease holder appends to the log).
//   - approval_ext: the deciding actor (approvals.decided_by is a uuid).
const SchemaSQL = `
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
`

// EnsureSchema applies SchemaSQL. The engine schema of the migrations
// package (spec 10.4) must already exist.
func EnsureSchema(ctx context.Context, db DB) error {
	if _, err := db.Exec(ctx, SchemaSQL); err != nil {
		return fmt.Errorf("pgrepo: ensure schema: %w", err)
	}
	return nil
}
