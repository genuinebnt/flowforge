-- requires-extension: vector
--
-- 0012_graph_vectors: embedding storage (section 22.9), transcribed
-- verbatim. Applied only when the pgvector extension is available on the
-- server (pg_available_extensions); otherwise the runner records it as
-- skipped and applies it on a later run once the extension is installed.
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE graph.embeddings (
  tenant_id      uuid NOT NULL,
  program_id     uuid NOT NULL,
  entity_id      uuid NOT NULL,
  model_id       text NOT NULL,
  model_version  text NOT NULL,
  dimension      int  NOT NULL,
  embedding      vector(768) NOT NULL,        -- dimension per model; use one table or partition per dimension
  content_hash   bytea NOT NULL,
  source_seq     bigint NOT NULL,
  created_at     timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, program_id, entity_id, model_id)
);
-- first version of the index: pgvector HNSW, one partial index per (program, model) is also fine
CREATE INDEX embeddings_hnsw ON graph.embeddings
  USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);

CREATE TABLE graph.vector_indexes (
  index_id       uuid PRIMARY KEY,
  tenant_id      uuid NOT NULL,
  program_id     uuid NOT NULL,
  model_id       text NOT NULL,
  model_version  text NOT NULL,
  dimension      int  NOT NULL,
  metric         text NOT NULL CHECK (metric IN ('l2','cosine','ip')),
  engine         text NOT NULL CHECK (engine IN ('pgvector','diskann')),
  params         jsonb NOT NULL,              -- R, L_build, alpha, pq_m, ...
  watermark_seq  bigint NOT NULL DEFAULT 0,
  UNIQUE (tenant_id, program_id, model_id, model_version)
);

CREATE TABLE graph.vector_segments (
  segment_id     uuid PRIMARY KEY,
  index_id       uuid NOT NULL REFERENCES graph.vector_indexes,
  gcs_uri        text NOT NULL,
  vectors        bigint NOT NULL,
  tombstones     bigint NOT NULL DEFAULT 0,
  level          smallint NOT NULL,           -- merge level
  state          text NOT NULL CHECK (state IN ('building','live','retiring')),
  created_at     timestamptz NOT NULL DEFAULT now()
);

-- Row-level security (section 10.7) for the tenant-scoped tables.
ALTER TABLE graph.embeddings ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON graph.embeddings
  USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
ALTER TABLE graph.vector_indexes ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON graph.vector_indexes
  USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
