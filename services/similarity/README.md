# services/similarity

Similarity (spec 22.9) over `vecindex.Registry`, one index per (tenant,
program, model).

* The graph.changes consumer embeds added, changed and reactivated entities
  (asset type + graphstore display text) with the `Embedder` port and
  upserts them ordered by change seq; the index watermark follows.
* `Upsert` is ordered by `source_seq` and a no-op for an unchanged
  `content_hash`; indexes are created on first use (cosine, the vector's
  dimension).
* Searches over-fetch, post-filter by asset type, tags and last_seen (entity
  metadata from GraphService), and pass every hit through
  ScopeService.CheckBatch (passive): out-of-scope hits are dropped unless
  `include_out_of_scope` is set by an analyst or above, and are then marked
  `in_scope = false`. `HybridSearch` fuses the vector search with
  GraphService.Search by reciprocal rank fusion.
* `scope.tenant_id` is cross-checked against the token.

`HashEmbedder` is a deterministic feature-hashing embedder for dev and
tests. **It is not semantic.** A Vertex AI / Genkit embedder is future work,
as is persistence (pgvector, `graph.embeddings`): vectors live in memory.
