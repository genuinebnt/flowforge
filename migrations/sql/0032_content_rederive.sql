-- 0032_content_rederive: re-derive jobs (V2 design §12.2; contentstore
-- Store.Rederive). derive_stamp names the pipeline (normalizers,
-- extractors and their rule tables) that derived a snapshot's normalized
-- text, features, kind, coverage and simhash; a snapshot whose stamp is not
-- the running pipeline's is re-derived from its stored raw body. Rows
-- written before this migration have the empty stamp, so the first
-- re-derive pass visits each of them once (and rewrites nothing when the
-- pipeline did not change). The index serves the job's ordered scan of
-- one program's rows in (valid_from, id) order; the stamp is in it, so
-- fresh rows are filtered out in the index before any row is fetched.

ALTER TABLE content.snapshots ADD COLUMN derive_stamp text NOT NULL DEFAULT '';
CREATE INDEX snapshots_derive ON content.snapshots (tenant_id, program_id, valid_from, id, derive_stamp);
