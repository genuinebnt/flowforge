-- 0018_v1_db_roles: per-service database roles and grants (spec 10, 17.2;
-- G-OPS-01) and forced row-level security on every tenant-scoped table,
-- its partitions and its child tables (spec 10.7; G-OPS-02).
--
-- Four functions do the work so that cmd/migrate can re-run them after
-- every migration job (tables created by later files get the same
-- treatment) and the partition helper can secure the partitions it adds:
--
-- * public.ff_service_roles(): the role table below.
-- * public.ff_secure_rls(): catalog-driven and idempotent. Every table in a
--   service schema with a tenant_id column (partitioned parents and each
--   partition included: a query that names a partition directly does not
--   see the parent's policies) gets ENABLE + FORCE ROW LEVEL SECURITY and
--   the tenant_isolation policy of 0007. The child tables listed below,
--   which have no tenant_id, get ENABLE + FORCE and a parent_visible policy:
--   a row is visible when its parent row is, and the parent is itself
--   filtered by tenant_isolation (a policy's subquery runs under the
--   caller's row-level security). FORCE subjects the table owner too; only
--   superusers and the role-scoped svc_<role> policies (also created
--   here, for the roles that exist) get past it.
-- * public.ff_sync_roles(): creates the roles that do not exist yet
--   (NOLOGIN; on Cloud SQL, Terraform has already created them as login
--   users), revokes PUBLIC access to the service schemas, grants each role
--   USAGE and DML on its own schema only and revokes it everywhere else
--   (cross-service data moves by value over gRPC and the bus, never by a
--   cross-schema read), and removes the roles from cloudsqlsuperuser when that role exists.
-- * public.ff_role_problems(): what cmd/migrate refuses to leave behind (a
--   service role that is a superuser, bypasses RLS, can create roles or
--   databases, owns a service table, or is still in cloudsqlsuperuser).
--
-- Roles (names match infra's db_users with the ff_ prefix of the
-- cloud-sql module):
--
--   ff_gateway    identity       cross-tenant policy (login, refresh and
--                                API-key lookups happen before the tenant
--                                is known)
--   ff_engine     engine         cross-tenant policy (runs are keyed by run
--                                id; the scheduler's ListReady/ClaimReady
--                                and the outbox relay span tenants)
--   ff_scope      scope          cross-tenant policy (outbox relay)
--   ff_notifier   notifier       cross-tenant policy (delivery worker)
--   ff_graph      graph          subject to RLS: graphstore/pg sets
--                                app.tenant_id in every transaction; only
--                                graph.change_events (the cross-tenant
--                                relay's outbox) has a policy for it
--   ff_similarity graph vector tables (0012) only, subject to RLS
--   ff_maintenance every service schema, cross-tenant policy on every
--                 RLS table: retention, partition and archival jobs. NOLOGIN;
--                 the migrator is made a member (policies TO a role apply to
--                 its members) and jobs may SET ROLE to it.
--
-- The bus schema (0016, the local multi-process Postgres bus) is shared by
-- every service role when it exists.
--
-- Why role-scoped policies and not the BYPASSRLS attribute: a role with
-- CREATEROLE (the Cloud SQL migrator, a cloudsqlsuperuser member) cannot
-- grant BYPASSRLS without holding it (PostgreSQL 16), and BYPASSRLS would
-- exempt the role on every table it can reach. A policy "TO ff_engine
-- USING (true)" on the engine tables only is the narrower grant, and it is
-- plain DDL the migrator owns.

-- The role table, in one place: role, the schemas it owns (USAGE + DML),
-- and the schemas on whose RLS tables it gets the cross-tenant policy.
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
    ('ff_maintenance', ARRAY['identity', 'scope', 'engine', 'graph', 'notifier'],
                       ARRAY['identity', 'scope', 'engine', 'graph', 'notifier'])
$$;

CREATE OR REPLACE FUNCTION public.ff_secure_rls() RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
  r record;
  tenant_pol constant text := 'tenant_id = NULLIF(current_setting(''app.tenant_id'', true), '''')::uuid';
BEGIN
  FOR r IN
    SELECT c.oid::regclass AS t
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'tenant_id' AND NOT a.attisdropped
    WHERE n.nspname IN ('identity', 'scope', 'engine', 'graph', 'notifier') AND c.relkind IN ('r', 'p')
    ORDER BY 1
  LOOP
    EXECUTE format('ALTER TABLE %s ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY', r.t);
    IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polrelid = r.t AND polname = 'tenant_isolation') THEN
      EXECUTE format('CREATE POLICY tenant_isolation ON %s USING (%s)', r.t, tenant_pol);
    END IF;
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
    EXECUTE format('ALTER TABLE %s ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY', r.child);
    IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polrelid = to_regclass(r.child) AND polname = 'parent_visible') THEN
      -- The child column is qualified with the child's table name: an
      -- unqualified name would bind to the parent if it had such a column.
      EXECUTE format('CREATE POLICY parent_visible ON %s USING (EXISTS (SELECT 1 FROM %s p WHERE p.%I = %I.%I))',
        r.child, r.parent, r.pcol, split_part(r.child, '.', 2), r.col);
    END IF;
  END LOOP;

  -- Cross-tenant access, as a permissive policy scoped to a role (OR-ed
  -- with tenant_isolation / parent_visible; a policy TO a role also
  -- applies to its members). Roles that do not exist yet are skipped;
  -- ff_sync_roles creates them and calls this again.
  FOR r IN
    SELECT c.oid::regclass AS rel, n.nspname AS nsp, sr.role, sr.cross_tenant
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    CROSS JOIN public.ff_service_roles() sr
    WHERE c.relrowsecurity AND c.relkind IN ('r', 'p')
      AND n.nspname IN ('identity', 'scope', 'engine', 'graph', 'notifier')
      AND EXISTS (SELECT 1 FROM pg_roles WHERE rolname = sr.role)
  LOOP
    IF r.nsp = ANY (r.cross_tenant) OR (r.role = 'ff_graph' AND r.rel = 'graph.change_events'::regclass) THEN
      IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polrelid = r.rel AND polname = 'svc_' || r.role) THEN
        EXECUTE format('CREATE POLICY %I ON %s TO %I USING (true) WITH CHECK (true)', 'svc_' || r.role, r.rel, r.role);
      END IF;
    ELSIF EXISTS (SELECT 1 FROM pg_policy WHERE polrelid = r.rel AND polname = 'svc_' || r.role) THEN
      EXECUTE format('DROP POLICY %I ON %s', 'svc_' || r.role, r.rel);
    END IF;
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
             WHERE c.relowner = r.oid AND n.nspname IN ('identity', 'scope', 'engine', 'graph', 'notifier')),
     'owns tables in a service schema')
  ) AS p(bad, problem)
  WHERE r.rolname IN (SELECT role FROM public.ff_service_roles())
    AND p.bad
  ORDER BY 1
$$;

-- 0011's helper, now SECURITY DEFINER (creating and attaching a partition
-- needs ownership of scope.scope_decisions, which only the migrator has)
-- and securing the partition it creates (a partition queried by name does
-- not inherit the parent's policies). Body otherwise unchanged.
CREATE OR REPLACE FUNCTION scope.ensure_decision_partition(month timestamptz) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  lo   timestamptz := date_trunc('month', month AT TIME ZONE 'UTC') AT TIME ZONE 'UTC';
  hi   timestamptz := (date_trunc('month', month AT TIME ZONE 'UTC') + interval '1 month') AT TIME ZONE 'UTC';
  part text := format('scope_decisions_y%sm%s',
                      to_char(month AT TIME ZONE 'UTC', 'YYYY'), to_char(month AT TIME ZONE 'UTC', 'MM'));
BEGIN
  PERFORM pg_advisory_xact_lock(hashtext('scope.scope_decisions partitions'));
  IF EXISTS (
    SELECT 1 FROM pg_inherits i
    JOIN pg_class c ON c.oid = i.inhrelid
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE i.inhparent = 'scope.scope_decisions'::regclass AND n.nspname = 'scope' AND c.relname = part
  ) THEN
    RETURN 'scope.' || part;
  END IF;
  EXECUTE format('CREATE TABLE scope.%I (LIKE scope.scope_decisions INCLUDING DEFAULTS INCLUDING CONSTRAINTS)', part);
  EXECUTE format(
    'WITH moved AS (DELETE FROM scope.scope_decisions_default WHERE decided_at >= %L AND decided_at < %L RETURNING *) '
    'INSERT INTO scope.%I SELECT * FROM moved', lo, hi, part);
  EXECUTE format('ALTER TABLE scope.scope_decisions ATTACH PARTITION scope.%I FOR VALUES FROM (%L) TO (%L)', part, lo, hi);
  PERFORM public.ff_secure_rls();
  RETURN 'scope.' || part;
END $$;

SELECT public.ff_sync_roles();
