-- 0003_scope: spec section 10.3, transcribed verbatim. The default
-- partition and the monthly partition helper are added in
-- 0011_scope_decision_partitions.sql.

CREATE SCHEMA scope;

CREATE TABLE scope.programs (
  id uuid PRIMARY KEY, tenant_id uuid NOT NULL, name text NOT NULL, platform text,
  status text NOT NULL DEFAULT 'active' CHECK (status IN ('active','paused','archived')),
  current_version integer NOT NULL DEFAULT 0, created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, name)
);

CREATE TABLE scope.scope_versions (
  program_id uuid NOT NULL REFERENCES scope.programs(id) ON DELETE CASCADE,
  version integer NOT NULL, document jsonb NOT NULL,              -- section 4.1
  effective_from timestamptz NOT NULL, created_by uuid NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (program_id, version)
);

CREATE TABLE scope.scope_decisions (
  id uuid NOT NULL, tenant_id uuid NOT NULL, program_id uuid NOT NULL, scope_version integer NOT NULL,
  run_id uuid, effect_path text, target text NOT NULL,
  class text NOT NULL CHECK (class IN ('passive','active','intrusive')),
  decision text NOT NULL CHECK (decision IN ('allow','deny')), reason text NOT NULL, rule_id text,
  pinned_ips inet[] NOT NULL DEFAULT '{}', decided_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (id, decided_at)
) PARTITION BY RANGE (decided_at);                                 -- monthly partitions
CREATE INDEX ON scope.scope_decisions (tenant_id, program_id, decided_at DESC);
CREATE INDEX ON scope.scope_decisions (run_id);

CREATE TABLE scope.kill_switches (
  level text NOT NULL CHECK (level IN ('global','tenant','program')), target_id uuid,
  enabled boolean NOT NULL, set_by uuid NOT NULL, set_at timestamptz NOT NULL DEFAULT now(), reason text,
  PRIMARY KEY (level, target_id)
);
