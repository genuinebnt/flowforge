-- 0011_scope_decision_partitions: partitions for scope.scope_decisions
-- (section 10.3 says "monthly partitions" but defines none; a partitioned
-- table without partitions rejects every insert).
--
-- * scope.scope_decisions_default catches rows outside every monthly
--   partition, so a missed maintenance run never loses a decision.
-- * scope.ensure_decision_partition(month) creates the partition for the
--   month containing its argument, moving any rows for that month out of
--   the default partition first (ATTACH would fail otherwise). It is
--   idempotent and returns the partition name. A scheduled job calls it for
--   the next month; retention (13 months, 10.7) drops old partitions.
CREATE TABLE scope.scope_decisions_default PARTITION OF scope.scope_decisions DEFAULT;

CREATE FUNCTION scope.ensure_decision_partition(month timestamptz) RETURNS text
LANGUAGE plpgsql AS $$
DECLARE
  lo   timestamptz := date_trunc('month', month AT TIME ZONE 'UTC') AT TIME ZONE 'UTC';
  hi   timestamptz := (date_trunc('month', month AT TIME ZONE 'UTC') + interval '1 month') AT TIME ZONE 'UTC';
  part text := format('scope_decisions_y%sm%s',
                      to_char(month AT TIME ZONE 'UTC', 'YYYY'), to_char(month AT TIME ZONE 'UTC', 'MM'));
BEGIN
  -- Serialize concurrent callers for the same table.
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
  RETURN 'scope.' || part;
END $$;

-- The current and the next month, so a fresh database is ready to use.
SELECT scope.ensure_decision_partition(now());
SELECT scope.ensure_decision_partition(now() + interval '1 month');
