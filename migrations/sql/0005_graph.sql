-- 0005_graph: spec section 10.5 (Open Asset Model aligned tables) and the
-- graph.snapshots catalogue from section 21, transcribed verbatim. The
-- pgvector tables (section 22.9) are in 0012_graph_vectors.sql because they
-- need an optional extension.

CREATE SCHEMA graph;

CREATE TABLE graph.entities (
  id uuid PRIMARY KEY, tenant_id uuid NOT NULL, program_id uuid NOT NULL,
  asset_type text NOT NULL,                                   -- OAM asset type name, e.g. 'FQDN'
  key text NOT NULL,                                          -- canonical dedup key (section 5.4)
  content jsonb NOT NULL,                                     -- OAM asset content
  scope_status text NOT NULL DEFAULT 'unknown' CHECK (scope_status IN ('in_scope','out_of_scope','unknown')),
  confidence real NOT NULL DEFAULT 0.5 CHECK (confidence BETWEEN 0 AND 1),
  state text NOT NULL DEFAULT 'active' CHECK (state IN ('active','stale')),
  created_at timestamptz NOT NULL DEFAULT now(), last_seen timestamptz NOT NULL DEFAULT now(),
  first_seen_run uuid, last_seen_run uuid, missed_runs integer NOT NULL DEFAULT 0,
  search_text text NOT NULL DEFAULT '',                       -- lower-cased display text for trigram search
  UNIQUE (tenant_id, program_id, asset_type, key)
);
CREATE INDEX entities_type  ON graph.entities (tenant_id, program_id, asset_type, last_seen DESC);
CREATE INDEX entities_scope ON graph.entities (tenant_id, program_id, scope_status);
CREATE INDEX entities_json  ON graph.entities USING gin (content jsonb_path_ops);
CREATE INDEX entities_trgm  ON graph.entities USING gin (search_text gin_trgm_ops);

CREATE TABLE graph.edges (
  id uuid PRIMARY KEY, tenant_id uuid NOT NULL, program_id uuid NOT NULL,
  rel_type text NOT NULL,                                     -- SimpleRelation, PortRelation, BasicDNSRelation, ...
  label text NOT NULL,                                        -- dns_record, port, contains, ...
  discriminator text NOT NULL DEFAULT '',                     -- 'tcp/443' or 'rr1' (section 5.7)
  content jsonb NOT NULL DEFAULT '{}',
  from_entity_id uuid NOT NULL REFERENCES graph.entities(id) ON DELETE CASCADE,
  to_entity_id   uuid NOT NULL REFERENCES graph.entities(id) ON DELETE CASCADE,
  confidence real NOT NULL DEFAULT 0.5, state text NOT NULL DEFAULT 'active' CHECK (state IN ('active','stale')),
  created_at timestamptz NOT NULL DEFAULT now(), last_seen timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, program_id, from_entity_id, to_entity_id, rel_type, label, discriminator)
);
CREATE INDEX edges_from ON graph.edges (from_entity_id, label);
CREATE INDEX edges_to   ON graph.edges (to_entity_id, label);

CREATE TABLE graph.entity_tags (
  id uuid PRIMARY KEY, tenant_id uuid NOT NULL,
  entity_id uuid NOT NULL REFERENCES graph.entities(id) ON DELETE CASCADE,
  ptype text NOT NULL,                                        -- SourceProperty, SimpleProperty, DNSRecordProperty, VulnProperty, FlowforgeProperty
  name text NOT NULL, content jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(), last_seen timestamptz NOT NULL DEFAULT now(),
  UNIQUE (entity_id, ptype, name)
);

CREATE TABLE graph.edge_tags (
  id uuid PRIMARY KEY, tenant_id uuid NOT NULL,
  edge_id uuid NOT NULL REFERENCES graph.edges(id) ON DELETE CASCADE,
  ptype text NOT NULL, name text NOT NULL, content jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(), last_seen timestamptz NOT NULL DEFAULT now(),
  UNIQUE (edge_id, ptype, name)
);

CREATE TABLE graph.observations (
  id uuid PRIMARY KEY, tenant_id uuid NOT NULL, program_id uuid NOT NULL, run_id uuid NOT NULL, effect_path text NOT NULL,
  tool_name text NOT NULL, tool_version text NOT NULL, observed_at timestamptz NOT NULL, confidence real NOT NULL,
  raw_ref text, entity_count integer NOT NULL, edge_count integer NOT NULL, ingested_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON graph.observations (tenant_id, run_id);

CREATE TABLE graph.change_events (                                  -- history and outbox in one
  id uuid PRIMARY KEY, seq bigserial UNIQUE, tenant_id uuid NOT NULL, program_id uuid NOT NULL,
  type text NOT NULL CHECK (type IN ('entity_added','entity_changed','entity_stale','entity_reactivated','edge_added','edge_removed','scope_status_changed')),
  entity_id uuid, edge_id uuid, run_id uuid, patch jsonb, at timestamptz NOT NULL DEFAULT now(), published_at timestamptz
);
CREATE INDEX ON graph.change_events (tenant_id, program_id, seq);
CREATE INDEX ON graph.change_events (id) WHERE published_at IS NULL;

-- schema graph: snapshot catalogue (section 21)
CREATE TABLE graph.snapshots (
  snapshot_id    uuid PRIMARY KEY,
  tenant_id      uuid NOT NULL,
  program_id     uuid NOT NULL,
  watermark_seq  bigint NOT NULL,
  node_count     bigint NOT NULL,
  edge_count     bigint NOT NULL,
  gcs_uri        text NOT NULL,
  sha256         bytea NOT NULL,
  format_version smallint NOT NULL,
  built_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, program_id, watermark_seq)
);
CREATE INDEX snapshots_latest ON graph.snapshots (tenant_id, program_id, watermark_seq DESC);
