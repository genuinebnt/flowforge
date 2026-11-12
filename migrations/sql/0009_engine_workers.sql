-- 0009_engine_workers: engine additions for work distribution and the
-- event store (sections 9.4, 23.3, 23.4). Not spelled out by the spec.

-- Membership (23.3): each worker upserts its row every 5 s; a worker is
-- live for 15 s after heartbeat_at. Rendezvous hashing runs over the live
-- set. heartbeat_at uses the database clock (now()), like leases.
CREATE TABLE engine.workers (
  id           text PRIMARY KEY,                        -- WorkerID
  started_at   timestamptz NOT NULL DEFAULT now(),
  heartbeat_at timestamptz NOT NULL DEFAULT now(),
  address      text,                                    -- host:port for diagnostics
  meta         jsonb NOT NULL DEFAULT '{}'              -- version, capacity, ...
);
CREATE INDEX workers_live ON engine.workers (heartbeat_at);

-- Lost-wakeup protection for the lease protocol (eventstore.ReleaseLease):
-- an out-of-band wake (timer fired, approval decided, cancel requested,
-- scope change; section 9.4) that lands while a worker holds the lease must
-- not be overwritten when that worker parks the run with a later
-- next_wake_at. Wake sets woken_at = now(); acquire sets
-- lease_acquired_at = now(); release wakes the run immediately when
-- woken_at >= lease_acquired_at.
ALTER TABLE engine.runs
  ADD COLUMN lease_acquired_at timestamptz,
  ADD COLUMN woken_at timestamptz;

-- The ready scan orders by priority before next_wake_at (eventstore
-- ListReady); this partial index matches it. runs_ready (0004) stays for
-- the plain next_wake_at scan.
CREATE INDEX runs_ready_prio ON engine.runs (priority DESC, next_wake_at, id)
  WHERE status IN ('pending','running','waiting');
