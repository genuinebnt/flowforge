-- 0007_rls: row-level security as defense in depth (spec section 10.7).
--
-- Every table that carries a tenant_id column gets
--   ALTER TABLE ... ENABLE ROW LEVEL SECURITY;
--   CREATE POLICY tenant_isolation ... USING (tenant_id = <app.tenant_id>);
-- Services set the tenant per transaction with
--   SELECT set_config('app.tenant_id', '<uuid>', true);
--
-- Deviations (see migrations/OPEN_QUESTIONS.md):
--   * current_setting(..., true) with NULLIF: an unset or reset setting
--     yields NULL, which matches no row (fail closed) instead of raising.
--   * ENABLE, not FORCE: the table owner (the migration role) and roles with
--     BYPASSRLS are exempt. Cross-tenant system paths (the engine scheduler,
--     outbox relays) must connect with such a role; request paths use an
--     application role that is subject to the policies.
--   * Not covered: tables without tenant_id (reached through a tenant-scoped
--     parent: run_events, effects, timers, engine.snapshots,
--     scope_versions, deliveries, outbox and inbox tables), global tables
--     (identity.tenants, identity.users, identity.signing_keys,
--     engine.tools, scope.kill_switches) and identity.auth_audit, whose
--     tenant_id is nullable (failed logins have no tenant).
DO $$
DECLARE
  t text;
BEGIN
  FOREACH t IN ARRAY ARRAY[
    'identity.memberships', 'identity.invites', 'identity.refresh_tokens', 'identity.api_keys',
    'scope.programs', 'scope.scope_decisions',
    'engine.workflows', 'engine.runs', 'engine.approvals',
    'graph.entities', 'graph.edges', 'graph.entity_tags', 'graph.edge_tags',
    'graph.observations', 'graph.change_events', 'graph.snapshots',
    'notifier.channels', 'notifier.alert_rules', 'notifier.alert_events'
  ] LOOP
    EXECUTE format('ALTER TABLE %s ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON %s USING (tenant_id = NULLIF(current_setting(''app.tenant_id'', true), '''')::uuid)',
      t);
  END LOOP;
END $$;
