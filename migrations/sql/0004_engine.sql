-- 0004_engine: spec section 10.4, transcribed verbatim. Additions the
-- event store and the engine need live in 0008 (inbox) and 0009 (lease
-- bookkeeping columns, worker heartbeats).

CREATE SCHEMA engine;

CREATE TABLE engine.tools (
  name text NOT NULL, version text NOT NULL, spec jsonb NOT NULL, spec_sha256 bytea NOT NULL,
  enabled boolean NOT NULL DEFAULT true, PRIMARY KEY (name, version)
);

CREATE TABLE engine.workflows (
  id uuid PRIMARY KEY, tenant_id uuid NOT NULL, name text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, name)
);

CREATE TABLE engine.workflow_versions (
  id uuid PRIMARY KEY, workflow_id uuid NOT NULL REFERENCES engine.workflows(id) ON DELETE CASCADE,
  version integer NOT NULL, source text NOT NULL, source_sha256 bytea NOT NULL,
  module bytea NOT NULL, module_sha256 bytea NOT NULL,
  effects text[] NOT NULL, inferred_class text NOT NULL CHECK (inferred_class IN ('none','passive','active','intrusive')),
  status text NOT NULL DEFAULT 'published' CHECK (status IN ('published','deprecated')),
  diagnostics jsonb NOT NULL DEFAULT '[]', created_by uuid NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (workflow_id, version)
);

CREATE TABLE engine.runs (
  id uuid PRIMARY KEY, tenant_id uuid NOT NULL, program_id uuid,
  workflow_version_id uuid NOT NULL REFERENCES engine.workflow_versions(id),
  parent_run_id uuid REFERENCES engine.runs(id),
  scope_version integer,
  status text NOT NULL CHECK (status IN ('pending','running','waiting','paused','cancelling','compensating','succeeded','failed','cancelled')),
  compensation_outcome text NOT NULL DEFAULT 'none' CHECK (compensation_outcome IN ('none','completed','failed')),
  input jsonb NOT NULL DEFAULT '{}', output jsonb, failure_code text, failure_message text,
  created_by text NOT NULL,                                        -- 'user:<id>' | 'apikey:<id>' | 'schedule:<id>'
  idempotency_key text NOT NULL, request_hash bytea NOT NULL,
  priority smallint NOT NULL DEFAULT 0, deadline_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(), started_at timestamptz, finished_at timestamptz,
  last_seq bigint NOT NULL DEFAULT 0,
  next_wake_at timestamptz NOT NULL DEFAULT now(),
  lease_owner text, lease_expires_at timestamptz, fence bigint NOT NULL DEFAULT 0,
  UNIQUE (tenant_id, idempotency_key)
);
CREATE INDEX runs_ready ON engine.runs (next_wake_at) WHERE status IN ('pending','running','waiting');
CREATE INDEX runs_list  ON engine.runs (tenant_id, created_at DESC);
CREATE INDEX runs_prog  ON engine.runs (tenant_id, program_id, created_at DESC);

CREATE TABLE engine.run_events (                                   -- append-only
  run_id uuid NOT NULL REFERENCES engine.runs(id), seq bigint NOT NULL,
  type text NOT NULL, at timestamptz NOT NULL DEFAULT now(), actor text NOT NULL, payload jsonb NOT NULL,
  PRIMARY KEY (run_id, seq)
);
CREATE FUNCTION engine.forbid_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'run_events is append-only'; END $$;
CREATE TRIGGER run_events_immutable BEFORE UPDATE OR DELETE ON engine.run_events
  FOR EACH ROW EXECUTE FUNCTION engine.forbid_mutation();

CREATE TABLE engine.effects (
  run_id uuid NOT NULL REFERENCES engine.runs(id), path text NOT NULL,         -- '14' or '14/3/2'
  seq bigint NOT NULL, kind text NOT NULL, args_hash bytea NOT NULL,
  state text NOT NULL CHECK (state IN ('scheduled','started','completed','failed','unknown')),
  attempt integer NOT NULL DEFAULT 0, idempotency_key text NOT NULL,
  scheduled_at timestamptz NOT NULL DEFAULT now(), started_at timestamptz, completed_at timestamptz,
  next_retry_at timestamptz, result jsonb, error jsonb,
  PRIMARY KEY (run_id, path)
);

CREATE TABLE engine.timers (
  id uuid PRIMARY KEY, run_id uuid NOT NULL REFERENCES engine.runs(id), path text NOT NULL,
  fire_at timestamptz NOT NULL, fired_at timestamptz
);
CREATE INDEX timers_due ON engine.timers (fire_at) WHERE fired_at IS NULL;

CREATE TABLE engine.approvals (
  id uuid PRIMARY KEY, tenant_id uuid NOT NULL, run_id uuid NOT NULL REFERENCES engine.runs(id),
  effect_path text NOT NULL, role text NOT NULL, reason text NOT NULL, proposed jsonb NOT NULL DEFAULT '{}',
  status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','approved','denied','expired','cancelled')),
  expires_at timestamptz NOT NULL, escalate_role text,
  decided_by uuid, decided_at timestamptz, comment text, created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (run_id, effect_path)
);
CREATE INDEX approvals_open ON engine.approvals (tenant_id, role, expires_at) WHERE status = 'pending';

CREATE TABLE engine.snapshots (
  run_id uuid NOT NULL REFERENCES engine.runs(id), at_seq bigint NOT NULL,
  module_sha256 bytea NOT NULL, state bytea NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (run_id, at_seq)
);

CREATE TABLE engine.outbox (                                       -- same shape in every service schema
  id bigserial PRIMARY KEY, event_id uuid NOT NULL UNIQUE, topic text NOT NULL, ordering_key text,
  payload jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), published_at timestamptz
);
CREATE INDEX ON engine.outbox (id) WHERE published_at IS NULL;
