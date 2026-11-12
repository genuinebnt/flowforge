# services/graph

GraphService (spec 11.6) and GraphAnalytics (spec 21.6).

* `Server` maps each RPC onto `graphstore.Store` (`graphstore/pg` or
  `graphstore.Mem`) with the token's tenant as the caller. Observations
  arrive in the proto form and are decoded strictly into `oam.Observation`
  (`convert.ObservationDomain`). Passive scope status comes from the scope
  service (`ScopeFunc`, current policy cached 5 s). `WatchChanges` streams
  after `after_seq` and follows new commits.
* graph.changes: with Postgres the `graph.change_events` rows are the
  outbox and `graphstore/pg.Relay` publishes them; in memory the service
  publishes each write's changes inline.
* `Analytics` runs BFS, shortest path, PageRank, components, SCC and
  k-core on a CSR snapshot from `Store.BuildSnapshot`, cached per program for
  30 s unless a newer `min_watermark_seq` is requested; a snapshot older than
  `min_watermark_seq` is FAILED_PRECONDITION. `tenant_id` in the request is
  cross-checked against the token (mismatch: NOT_FOUND). Path edge ids are
  resolved from the store (the snapshot keeps none).
