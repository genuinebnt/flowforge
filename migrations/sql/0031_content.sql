-- 0031_content: the content schema, owned by the content service (V2
-- design §6.2 snapshot records, §6.4 secrets, §7 search watermark, §11
-- items 13, 14 and 18, §12.12 storage rules). Idempotent DDL guards are
-- not needed: the runner applies each file once.
--
-- * programs: per (tenant, program) the diff sequence (text-index document
--   ids and watermarks derive from it).
-- * snapshots: one row per stored capture. Validity is [valid_from,
--   valid_to); valid_to IS NULL is the open version, unique per (tenant,
--   program, url, profile) (partial unique index), so an as-of lookup is
--   one index probe and an unchanged re-fetch only moves last_seen. A
--   degraded capture is stored closed with an empty interval. The *_ref
--   and *_manifest columns are chunk-store manifest ids (hex) of the
--   tenant's chunk store; codec/delta_base/delta_depth describe the blob
--   encoding (contentstore FFZB delta frames). body_seal and headers_seal
--   are AES-256-GCM ciphertexts of the redacted spans; pinned snapshots
--   are never deleted by retention.
-- * diffs: follow their snapshots (ON DELETE CASCADE); seq is unique per
--   program.
-- * findings: secrets, encrypted; only rule_id, preview and keyed_hash in
--   the clear.
-- * volatility: learned-volatility counters per (url template, profile,
--   feature path) and the version that froze a path.
-- * quota_limits / quota_usage: per-tenant quotas (QUOTA_EXCEEDED).
-- * index_state: the text-index watermark per (tenant, program).
-- * data_keys: per-tenant data keys wrapped by the key-encryption key.
-- * outbox: content.changes events (spec 23.7 shape, relayed by
--   services/internal/bus PgOutbox).
--
-- Every table with tenant_id gets forced RLS and the tenant_isolation
-- policy through ff_secure_rls (redefined below to include the content
-- schema). Role ff_content owns the schema and is subject to RLS: the
-- content store sets app.tenant_id in every transaction; only
-- content.programs carries svc_ff_content (background sweeps enumerate
-- tenants there). ff_maintenance gains the schema like the others.

CREATE SCHEMA IF NOT EXISTS content;

CREATE TABLE content.programs (
  tenant_id  uuid NOT NULL,
  program_id uuid NOT NULL,
  diff_seq   bigint NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, program_id)
);

CREATE TABLE content.snapshots (
  id                  uuid PRIMARY KEY,
  tenant_id           uuid NOT NULL,
  program_id          uuid NOT NULL,
  entity_id           text NOT NULL DEFAULT '',
  url                 text NOT NULL,
  url_template        text NOT NULL DEFAULT '',
  host                text NOT NULL DEFAULT '',
  profile             text NOT NULL,
  run_id              text NOT NULL DEFAULT '',
  effect_path         text NOT NULL DEFAULT '',
  fetched_at          timestamptz NOT NULL,
  last_seen           timestamptz NOT NULL,
  valid_from          timestamptz NOT NULL,
  valid_to            timestamptz,
  status              int NOT NULL,
  mime                text NOT NULL DEFAULT '',
  kind                text NOT NULL,
  signature           text NOT NULL,
  headers_ref         text NOT NULL,
  body_manifest       text NOT NULL,
  body_sha256         text NOT NULL,
  raw_size            bigint NOT NULL,
  stored_size         bigint NOT NULL,
  normalizer_version  text NOT NULL,
  normalized_manifest text NOT NULL,
  parent_id           uuid REFERENCES content.snapshots (id) ON DELETE SET NULL,
  lineage_parent_id   uuid REFERENCES content.snapshots (id) ON DELETE SET NULL,
  features_ref        text NOT NULL,
  outcome             text NOT NULL,
  coverage            text NOT NULL,
  degraded            boolean NOT NULL DEFAULT false,
  simhash             bigint NOT NULL DEFAULT 0,
  pinned              boolean NOT NULL DEFAULT false,
  pin_reason          text NOT NULL DEFAULT '',
  codec               text NOT NULL DEFAULT 'chunk',
  delta_base          uuid REFERENCES content.snapshots (id),
  delta_depth         int NOT NULL DEFAULT 0,
  body_seal           bytea,
  headers_seal        bytea,
  key_id              text NOT NULL DEFAULT '',
  created_at          timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT snapshots_validity CHECK (valid_to IS NULL OR valid_to >= valid_from),
  CONSTRAINT snapshots_codec CHECK (codec IN ('chunk', 'delta')),
  CONSTRAINT snapshots_degraded_closed CHECK (NOT degraded OR valid_to IS NOT NULL)
);
CREATE UNIQUE INDEX snapshots_open ON content.snapshots (tenant_id, program_id, url, profile) WHERE valid_to IS NULL;
CREATE INDEX snapshots_key ON content.snapshots (tenant_id, program_id, url, profile, valid_from DESC);
CREATE INDEX snapshots_program ON content.snapshots (tenant_id, program_id, valid_from DESC, id DESC);
CREATE INDEX snapshots_host_open ON content.snapshots (tenant_id, program_id, host, profile) WHERE valid_to IS NULL;
CREATE INDEX snapshots_signature ON content.snapshots (tenant_id, program_id, url, profile, signature);
CREATE INDEX snapshots_delta_base ON content.snapshots (delta_base) WHERE delta_base IS NOT NULL;
CREATE INDEX snapshots_parent ON content.snapshots (parent_id) WHERE parent_id IS NOT NULL;
CREATE INDEX snapshots_lineage_parent ON content.snapshots (lineage_parent_id) WHERE lineage_parent_id IS NOT NULL;

CREATE TABLE content.diffs (
  id                 uuid PRIMARY KEY,
  tenant_id          uuid NOT NULL,
  program_id         uuid NOT NULL,
  seq                bigint NOT NULL,
  from_snapshot      uuid REFERENCES content.snapshots (id) ON DELETE CASCADE,
  to_snapshot        uuid NOT NULL REFERENCES content.snapshots (id) ON DELETE CASCADE,
  levels             text[] NOT NULL,
  algorithm          text NOT NULL,
  normalizer_version text NOT NULL,
  stats              jsonb NOT NULL,
  hunks_ref          text NOT NULL,
  semantic_ref       text NOT NULL,
  stored_size        bigint NOT NULL,
  index_docs         int NOT NULL,
  at                 timestamptz NOT NULL,
  url                text NOT NULL,
  profile            text NOT NULL,
  created_at         timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, program_id, seq)
);
CREATE INDEX diffs_to ON content.diffs (to_snapshot);
CREATE INDEX diffs_from ON content.diffs (from_snapshot) WHERE from_snapshot IS NOT NULL;

CREATE TABLE content.findings (
  id          uuid PRIMARY KEY,
  tenant_id   uuid NOT NULL,
  program_id  uuid NOT NULL,
  snapshot_id uuid NOT NULL REFERENCES content.snapshots (id) ON DELETE CASCADE,
  rule_id     text NOT NULL,
  preview     text NOT NULL,
  keyed_hash  text NOT NULL,
  ciphertext  bytea NOT NULL,
  key_id      text NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX findings_snapshot ON content.findings (snapshot_id);
CREATE INDEX findings_keyed_hash ON content.findings (tenant_id, keyed_hash);

CREATE TABLE content.volatility (
  tenant_id           uuid NOT NULL,
  program_id          uuid NOT NULL,
  url_template        text NOT NULL,
  profile             text NOT NULL,
  feature_path        text NOT NULL,
  changes             int NOT NULL DEFAULT 0,
  stable_observations int NOT NULL DEFAULT 0,
  streak              int NOT NULL DEFAULT 0,
  frozen_version      int NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, program_id, url_template, profile, feature_path)
);

CREATE TABLE content.quota_limits (
  tenant_id        uuid PRIMARY KEY,
  captures_per_day bigint NOT NULL DEFAULT 0 CHECK (captures_per_day >= 0),
  stored_bytes     bigint NOT NULL DEFAULT 0 CHECK (stored_bytes >= 0),
  index_bytes      bigint NOT NULL DEFAULT 0 CHECK (index_bytes >= 0)
);

CREATE TABLE content.quota_usage (
  tenant_id    uuid PRIMARY KEY,
  day          text NOT NULL DEFAULT '',
  captures     bigint NOT NULL DEFAULT 0,
  stored_bytes bigint NOT NULL DEFAULT 0,
  index_bytes  bigint NOT NULL DEFAULT 0
);

CREATE TABLE content.index_state (
  tenant_id  uuid NOT NULL,
  program_id uuid NOT NULL,
  watermark  bigint NOT NULL DEFAULT 0,
  docs       bigint NOT NULL DEFAULT 0,
  bytes      bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, program_id)
);

CREATE TABLE content.data_keys (
  tenant_id  uuid NOT NULL,
  purpose    text NOT NULL,
  version    int NOT NULL,
  kek_id     text NOT NULL,
  wrapped    bytea NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, purpose, version)
);

CREATE TABLE content.outbox (
  id bigserial PRIMARY KEY, event_id uuid NOT NULL UNIQUE, topic text NOT NULL, ordering_key text,
  payload jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), published_at timestamptz
);
CREATE INDEX ON content.outbox (id) WHERE published_at IS NULL;

-- The 0018 functions, redefined with the content schema and the ff_content
-- role; bodies are otherwise those of 0018 (see its header comment).

CREATE OR REPLACE FUNCTION public.ff_service_roles()
RETURNS TABLE (role text, own text[], cross_tenant text[])
LANGUAGE sql IMMUTABLE AS $$
  VALUES
    ('ff_gateway', ARRAY['identity'], ARRAY['identity']),
    ('ff_engine', ARRAY['engine'], ARRAY['engine']),
    ('ff_scope', ARRAY['scope'], ARRAY['scope']),
    ('ff_graph', ARRAY['graph'], ARRAY[]::text[]),
    ('ff_notifier', ARRAY['notifier'], ARRAY['notifier']),
    ('ff_similarity', ARRAY[]::text[], ARRAY[]::text[]),
    ('ff_content', ARRAY['content'], ARRAY[]::text[]),
    ('ff_maintenance', ARRAY['identity', 'scope', 'engine', 'graph', 'notifier', 'content'],
                       ARRAY['identity', 'scope', 'engine', 'graph', 'notifier', 'content'])
$$;

CREATE OR REPLACE FUNCTION public.ff_secure_table(t regclass, policy_name text, using_expr text) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
  r record;
  nsp text;
  want boolean;
  has boolean;
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_class WHERE oid = t AND relrowsecurity AND relforcerowsecurity) THEN
    EXECUTE format('ALTER TABLE %s ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY', t);
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polrelid = t AND polname = policy_name) THEN
    EXECUTE format('CREATE POLICY %I ON %s USING (%s)', policy_name, t, using_expr);
  END IF;
  -- Cross-tenant access, as a permissive policy scoped to a role (OR-ed
  -- with the policy above; a policy TO a role also applies to its
  -- members). Roles that do not exist yet are skipped; ff_sync_roles
  -- creates them and runs this again.
  SELECT n.nspname INTO nsp FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = t;
  FOR r IN SELECT sr.role, sr.cross_tenant FROM public.ff_service_roles() sr
           WHERE EXISTS (SELECT 1 FROM pg_roles WHERE rolname = sr.role) LOOP
    want := nsp = ANY (r.cross_tenant) OR (r.role = 'ff_graph' AND t = 'graph.change_events'::regclass)
            OR (r.role = 'ff_content' AND t::text = 'content.programs');
    has := EXISTS (SELECT 1 FROM pg_policy WHERE polrelid = t AND polname = 'svc_' || r.role);
    IF want AND NOT has THEN
      EXECUTE format('CREATE POLICY %I ON %s TO %I USING (true) WITH CHECK (true)', 'svc_' || r.role, t, r.role);
    ELSIF has AND NOT want THEN
      EXECUTE format('DROP POLICY %I ON %s', 'svc_' || r.role, t);
    END IF;
  END LOOP;
END $$;

CREATE OR REPLACE FUNCTION public.ff_secure_rls() RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
  r record;
BEGIN
  FOR r IN
    SELECT c.oid::regclass AS t
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'tenant_id' AND NOT a.attisdropped
    WHERE n.nspname IN ('identity', 'scope', 'engine', 'graph', 'notifier', 'content') AND c.relkind IN ('r', 'p')
    ORDER BY 1
  LOOP
    PERFORM public.ff_secure_table(r.t, 'tenant_isolation', public.ff_tenant_policy());
  END LOOP;

  -- Child tables: (child, column, parent, parent column). The global
  -- tables (identity.tenants, users, signing_keys, email_verifications;
  -- engine.tools, workers; scope.kill_switches) and the outbox and inbox
  -- tables are system tables with no tenant parent; schema grants alone
  -- protect them (see migrations/OPEN_QUESTIONS.md 13).
  FOR r IN
    SELECT v.child, v.col, v.parent, v.pcol
    FROM (VALUES
      ('engine.run_events', 'run_id', 'engine.runs', 'id'),
      ('engine.effects', 'run_id', 'engine.runs', 'id'),
      ('engine.timers', 'run_id', 'engine.runs', 'id'),
      ('engine.snapshots', 'run_id', 'engine.runs', 'id'),
      ('engine.run_ext', 'run_id', 'engine.runs', 'id'),
      ('engine.run_requests', 'run_id', 'engine.runs', 'id'),
      ('engine.approval_ext', 'approval_id', 'engine.approvals', 'id'),
      ('engine.workflow_versions', 'workflow_id', 'engine.workflows', 'id'),
      ('engine.workflow_version_ext', 'workflow_version_id', 'engine.workflow_versions', 'id'),
      ('scope.scope_versions', 'program_id', 'scope.programs', 'id'),
      ('notifier.deliveries', 'alert_event_id', 'notifier.alert_events', 'id')
    ) AS v(child, col, parent, pcol)
    WHERE to_regclass(v.child) IS NOT NULL AND to_regclass(v.parent) IS NOT NULL
  LOOP
    -- The child column is qualified with the child's table name: an
    -- unqualified name would bind to the parent if it had such a column.
    PERFORM public.ff_secure_table(to_regclass(r.child), 'parent_visible',
      format('EXISTS (SELECT 1 FROM %s p WHERE p.%I = %I.%I)', r.parent, r.pcol, split_part(r.child, '.', 2), r.col));
  END LOOP;
END $$;

CREATE OR REPLACE FUNCTION public.ff_sync_roles() RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
  sr record;
  s text;
  t text;
  own text[];
  all_schemas text[] := ARRAY['identity', 'scope', 'engine', 'graph', 'notifier'];
BEGIN
  IF to_regnamespace('content') IS NOT NULL THEN
    all_schemas := all_schemas || 'content'::text;
  END IF;
  IF to_regnamespace('bus') IS NOT NULL THEN
    all_schemas := all_schemas || 'bus'::text;
  END IF;

  FOR sr IN SELECT * FROM public.ff_service_roles() LOOP
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = sr.role) THEN
      BEGIN
        EXECUTE format('CREATE ROLE %I NOLOGIN', sr.role);
      EXCEPTION WHEN duplicate_object OR unique_violation THEN
        NULL; -- created concurrently by a migrator of another database
      END;
    END IF;
  END LOOP;

  PERFORM public.ff_secure_rls();

  -- PUBLIC gets nothing in the service schemas. CREATE on public is
  -- revoked by default since PostgreSQL 15; revoked again for older
  -- servers. USAGE on public stays: pg_trgm and citext live there.
  FOREACH s IN ARRAY all_schemas LOOP
    EXECUTE format('REVOKE ALL ON SCHEMA %I FROM PUBLIC', s);
    EXECUTE format('REVOKE ALL ON ALL TABLES IN SCHEMA %I FROM PUBLIC', s);
    EXECUTE format('REVOKE ALL ON ALL SEQUENCES IN SCHEMA %I FROM PUBLIC', s);
    EXECUTE format('REVOKE ALL ON ALL FUNCTIONS IN SCHEMA %I FROM PUBLIC', s);
  END LOOP;
  REVOKE CREATE ON SCHEMA public FROM PUBLIC;
  IF to_regclass('public.schema_migrations') IS NOT NULL THEN
    REVOKE ALL ON TABLE public.schema_migrations FROM PUBLIC;
  END IF;
  REVOKE ALL ON FUNCTION public.ff_secure_rls() FROM PUBLIC;
  REVOKE ALL ON FUNCTION public.ff_secure_table(regclass, text, text) FROM PUBLIC;
  REVOKE ALL ON FUNCTION public.ff_sync_roles() FROM PUBLIC;
  REVOKE ALL ON FUNCTION public.ff_role_problems() FROM PUBLIC;

  FOR sr IN SELECT * FROM public.ff_service_roles() LOOP
    own := sr.own;
    IF sr.role <> 'ff_maintenance' AND to_regnamespace('bus') IS NOT NULL THEN
      own := own || 'bus'::text;
    END IF;
    FOREACH s IN ARRAY all_schemas LOOP
      IF s = ANY (own) THEN
        EXECUTE format('GRANT USAGE ON SCHEMA %I TO %I', s, sr.role);
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %I TO %I', s, sr.role);
        EXECUTE format('GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA %I TO %I', s, sr.role);
        EXECUTE format('GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA %I TO %I', s, sr.role);
        -- Tables a later migration creates are usable before the next
        -- ff_sync_roles run (default privileges of the migrating role).
        EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA %I GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %I', s, sr.role);
        EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA %I GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO %I', s, sr.role);
      ELSE
        -- Idempotent narrowing: a grant left by an earlier layout or by
        -- hand is removed, so "own schema only" holds after every run.
        EXECUTE format('REVOKE ALL ON ALL TABLES IN SCHEMA %I FROM %I', s, sr.role);
        EXECUTE format('REVOKE ALL ON ALL SEQUENCES IN SCHEMA %I FROM %I', s, sr.role);
        EXECUTE format('REVOKE ALL ON ALL FUNCTIONS IN SCHEMA %I FROM %I', s, sr.role);
        EXECUTE format('REVOKE ALL ON SCHEMA %I FROM %I', s, sr.role);
        EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA %I REVOKE ALL ON TABLES FROM %I', s, sr.role);
        EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA %I REVOKE ALL ON SEQUENCES FROM %I', s, sr.role);
      END IF;
    END LOOP;
  END LOOP;

  -- ff_similarity: the pgvector tables of 0012 only (absent without the
  -- extension).
  IF to_regclass('graph.embeddings') IS NOT NULL THEN
    GRANT USAGE ON SCHEMA graph TO ff_similarity;
    FOREACH t IN ARRAY ARRAY['graph.embeddings', 'graph.vector_indexes', 'graph.vector_segments'] LOOP
      IF to_regclass(t) IS NOT NULL THEN
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %s TO ff_similarity', t);
      END IF;
    END LOOP;
  END IF;

  -- The migrator runs retention as a member of ff_maintenance: FORCE
  -- subjects the owner to the policies too. A superuser needs nothing.
  IF NOT (SELECT rolsuper FROM pg_roles WHERE rolname = current_user)
     AND NOT pg_has_role(current_user, 'ff_maintenance', 'MEMBER') THEN
    EXECUTE format('GRANT ff_maintenance TO %I', current_user);
  END IF;

  -- Cloud SQL adds every user created through its API to cloudsqlsuperuser
  -- (CREATEROLE, CREATEDB and whatever else that role was granted). A
  -- service role keeps only the grants above. Without the privilege to
  -- revoke, ff_role_problems reports it and cmd/migrate fails.
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cloudsqlsuperuser') THEN
    FOR sr IN SELECT * FROM public.ff_service_roles() LOOP
      IF EXISTS (SELECT 1 FROM pg_auth_members m
                 JOIN pg_roles g ON g.oid = m.roleid JOIN pg_roles u ON u.oid = m.member
                 WHERE g.rolname = 'cloudsqlsuperuser' AND u.rolname = sr.role) THEN
        BEGIN
          EXECUTE format('REVOKE cloudsqlsuperuser FROM %I', sr.role);
        EXCEPTION WHEN insufficient_privilege THEN
          RAISE WARNING 'ff_sync_roles: cannot remove % from cloudsqlsuperuser: %', sr.role, SQLERRM;
        END;
      END IF;
    END LOOP;
  END IF;
END $$;

CREATE OR REPLACE FUNCTION public.ff_role_problems() RETURNS SETOF text
LANGUAGE sql STABLE AS $$
  SELECT r.rolname || ': ' || p.problem
  FROM pg_roles r
  CROSS JOIN LATERAL (VALUES
    (r.rolsuper, 'is a superuser'),
    (r.rolbypassrls, 'has BYPASSRLS'),
    (r.rolcreaterole, 'has CREATEROLE'),
    (r.rolcreatedb, 'has CREATEDB'),
    (EXISTS (SELECT 1 FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid
             WHERE m.member = r.oid AND g.rolname = 'cloudsqlsuperuser'), 'is a member of cloudsqlsuperuser'),
    (EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
             WHERE c.relowner = r.oid AND n.nspname IN ('identity', 'scope', 'engine', 'graph', 'notifier', 'content')),
     'owns tables in a service schema')
  ) AS p(bad, problem)
  WHERE r.rolname IN (SELECT role FROM public.ff_service_roles())
    AND p.bad
  ORDER BY 1
$$;

SELECT public.ff_sync_roles();
