-- 0030_graph_v2: graph expansion for content capture (V2 design 3.4).
-- Idempotent: every statement can run again on a database that already
-- has it.
--
-- * Change-event types gain content_changed and tag_changed.
-- * tag_type / tag_name name the tag of a tag_changed event.
-- * base holds a record's state at creation (entity_added: content and
--   scope status; the edge_added that creates an edge: its content), so
--   GetEntityAsOf / NeighborsAsOf can replay events from the empty graph.
-- * cause ('parser' | 'normalizer'; NULL = target) and baseline mark
--   events feeds hide (re-derivations after an upgrade, the first
--   complete capture of a URL and profile).
-- * (entity_id, seq) and (edge_id, seq) indexes serve the AsOf replays.
--
-- Authority bookkeeping needs no column: it lives in hidden tag rows
-- (ptype 'ff:live' per record and 'ff:auth' per authority unit on the
-- source entity), next to the existing 'ff:meta' and 'ff:run' rows; see
-- graphstore/OPEN_QUESTIONS.md.

ALTER TABLE graph.change_events DROP CONSTRAINT IF EXISTS change_events_type_check;
ALTER TABLE graph.change_events ADD CONSTRAINT change_events_type_check CHECK (type IN (
  'entity_added','entity_changed','entity_stale','entity_reactivated','edge_added','edge_removed','scope_status_changed',
  'content_changed','tag_changed'));

ALTER TABLE graph.change_events ADD COLUMN IF NOT EXISTS tag_type text;
ALTER TABLE graph.change_events ADD COLUMN IF NOT EXISTS tag_name text;
ALTER TABLE graph.change_events ADD COLUMN IF NOT EXISTS base jsonb;
ALTER TABLE graph.change_events ADD COLUMN IF NOT EXISTS cause text;
ALTER TABLE graph.change_events ADD COLUMN IF NOT EXISTS baseline boolean NOT NULL DEFAULT false;

ALTER TABLE graph.change_events DROP CONSTRAINT IF EXISTS change_events_tag_check;
ALTER TABLE graph.change_events ADD CONSTRAINT change_events_tag_check
  CHECK ((type = 'tag_changed') = (tag_type IS NOT NULL AND tag_name IS NOT NULL));
ALTER TABLE graph.change_events DROP CONSTRAINT IF EXISTS change_events_cause_check;
ALTER TABLE graph.change_events ADD CONSTRAINT change_events_cause_check
  CHECK (cause IS NULL OR cause IN ('parser','normalizer'));

CREATE INDEX IF NOT EXISTS change_events_entity_seq ON graph.change_events (entity_id, seq) WHERE entity_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS change_events_edge_seq ON graph.change_events (edge_id, seq) WHERE edge_id IS NOT NULL;
