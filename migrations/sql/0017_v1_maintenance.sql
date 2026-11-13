-- 0017_v1_maintenance: durable alert state (G-LIM-06), workflow schedules
-- (G-OPS-10) and the audited run archival path of the retention job
-- (spec 10.7; G-OPS-03).

-- Alert throttle windows and open digests (spec 15.3). The notifier keeps
-- them in memory for speed and writes every change here in the same
-- transaction as the alert events it produces, so a restart (or a rule
-- change, which rebuilds the processor) neither re-fires a throttled key
-- nor loses a half-collected digest. program_id is the zero UUID for
-- events without a program; a tenant-wide rule has state per program
-- because each program has its own processor.
CREATE TABLE notifier.throttle_windows (
  tenant_id uuid NOT NULL, program_id uuid NOT NULL, dedupe_key text NOT NULL,
  until timestamptz NOT NULL, suppressed integer NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, program_id, dedupe_key)
);
CREATE INDEX throttle_windows_until ON notifier.throttle_windows (until);

CREATE TABLE notifier.digest_buffers (
  tenant_id uuid NOT NULL, program_id uuid NOT NULL, rule_id uuid NOT NULL,
  digest jsonb NOT NULL, window_end timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, program_id, rule_id)
);

-- Workflow schedules created at runtime (POST /v1/schedules). The engine's
-- in-process scheduler fires due rows, and on GCP one Cloud Scheduler job
-- calls POST /internal/schedules:tick every minute, so a new schedule needs
-- no new Cloud Scheduler job. Each firing submits a run with idempotency key
-- schedule:<id>:<fire time> (spec 17), so two engines firing the same
-- minute create one run.
CREATE TABLE engine.schedules (
  id uuid PRIMARY KEY, tenant_id uuid NOT NULL, name text NOT NULL,
  cron text NOT NULL,                                               -- 5 fields, UTC
  workflow_name text NOT NULL, workflow_version integer NOT NULL DEFAULT 0,
  program_id uuid, input jsonb NOT NULL DEFAULT '{}', enabled boolean NOT NULL DEFAULT true,
  next_fire_at timestamptz NOT NULL, last_fire_at timestamptz, last_run_id uuid, last_error text NOT NULL DEFAULT '',
  created_by text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, name)
);
CREATE INDEX schedules_due ON engine.schedules (next_fire_at) WHERE enabled;

-- One row per archived run: where its run_events went (zstd JSON Lines in
-- the blob store), how many and their digest. It is the audit trail of the
-- only path that may delete run_events.
CREATE TABLE engine.run_archives (
  run_id uuid PRIMARY KEY, tenant_id uuid NOT NULL, uri text NOT NULL,
  events bigint NOT NULL, sha256 bytea NOT NULL, archived_at timestamptz NOT NULL DEFAULT now()
);

-- run_events stays append-only (0004) with one exception: the maintenance
-- transaction that archived a run first records it in run_archives and sets
-- ff.archive_run to its id (set_config(..., true): transaction-local), and
-- may then delete that run's events. Every other UPDATE or DELETE still
-- raises. This keeps the trigger enabled (disabling it needs table
-- ownership, which service roles do not have, and would open a window for
-- every session).
CREATE OR REPLACE FUNCTION engine.forbid_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE'
     AND OLD.run_id::text = current_setting('ff.archive_run', true)
     AND EXISTS (SELECT 1 FROM engine.run_archives a WHERE a.run_id = OLD.run_id) THEN
    RETURN OLD;
  END IF;
  RAISE EXCEPTION 'run_events is append-only';
END $$;
