# graphstore: open questions and decisions

Spec gaps met while implementing the graph store, the choice made and the
test that pins it. Merge semantics themselves are oam's (see
`oam/OPEN_QUESTIONS.md`); this file covers the store around them.

## Identity and requests

1. **Tenant source.** proto `IngestRequest` has no tenant; the tenant
   comes from the verified identity (`IngestRequest.TenantID`, `Caller`
   for every other call, spec 11.6 "Authorization"). An envelope
   `tenant_id` must equal it. (`TestGraphIngest_Atomic`.)
2. **Canonical ids.** The schema stores tenant, program and run as uuid,
   so they must be public ids (`ten_`, `prg_`, `run_`) or UUIDs; both
   spellings are canonicalized to the public form before oam derives
   entity ids, so Mem and Postgres agree. Conformance fixtures with names
   like `"p"` or `"r1"` are mapped to derived ids by the suite.
3. **Request vs envelope fields.** `program_id`, `run_id`, `tool_name` and
   `tool_version` may come from either; when both are set they must agree
   (INVALID_ARGUMENT otherwise). The run is optional.
4. **Time precision.** `observed_at` and every clock reading are truncated
   to microseconds (Postgres `timestamptz`) on entry (`StoreTime`), in
   both stores.
5. **NUL characters** anywhere in an envelope are rejected in both stores
   (Postgres text and jsonb cannot hold them). (`TestGraphIngest_Atomic`.)
6. **Observation idempotency** is per (tenant, observation_id), across
   programs, as in oam. The `graph.observations` row id is derived from
   both, so no tenant can suppress another's observation by reusing an id.
   A rejected observation does not consume its id.
   (`TestGraphIngest_Idempotent`.)

## Postgres storage

7. **Hidden bookkeeping tags.** Convergent merge needs per-field write
   stamps, the first/last sighting stamps and per-tool covering-run
   counters, which spec 10.5 has no columns for and this package may not
   migrate. They are stored as tag rows with reserved types: `ff:meta`
   (name `""`) and `ff:run` (name = tool). Reads never return them; oam
   rejects any user property type outside its five, so they cannot
   collide. A later migration could move them to a `merge_meta jsonb`
   column. A row without `ff:meta` (written by another tool) loads with
   zero stamps, so any later write wins.
8. **`missed_runs` column** holds the smallest miss counter over the tools
   that covered the record (the record is stale when it reaches
   `stale_after_runs`); the API exposes the per-tool map (`MissedRuns`).
9. **`confidence real`.** The column is for SQL filtering only; values are
   rounded to four decimals by oam, and reads re-round the float32, which
   recovers the exact float64.
10. **Covering runs** are recorded as `graph.observations` rows with
    `effect_path = 'ff:covering_run'` and a derived id per (tenant,
    program, tool, run), which makes `SetStale` idempotent. Observation
    retention (90 days, 10.7) also forgets them; re-sending a covering run
    older than that would count again.
11. **Merge in SQL and Go.** Entity and edge upserts are `INSERT ... ON
    CONFLICT DO UPDATE` with `created_at = LEAST(...)`, `last_seen =
    GREATEST(...)`; content (per-field last-writer-wins), confidence
    (noisy-or over SourceProperty tags) and scope status are computed in
    Go by the kernel with the oam primitives on rows loaded `FOR UPDATE`,
    in the same transaction. The kernel is tested against `oam.Graph`
    through a storage round trip after every operation
    (`TestGraphKernel_MatchesOAM`).
12. **Locking protocol.** Writers take transaction-scoped advisory locks
    per entity and edge id (sorted; covers rows that do not exist yet),
    then row locks in id order, and the per-(tenant, program) advisory
    lock only just before inserting change events. Writers of different
    records run concurrently, and a program's change-event seqs commit in
    seq order, so a snapshot watermark never skips an uncommitted event
    (21.4). Serialization failures and deadlocks are retried (3 times).
    (`TestGraphIngest_ConcurrentConvergence`.)
13. **Plan stability.** Store transactions force custom plans, and id
    lookups compare tenant and program as text, so the planner always uses
    the primary key; on fresh, unanalyzed tables it otherwise preferred
    the (tenant, program, ...) indexes and ingest time grew with the graph.
14. **RLS.** Every transaction sets `app.tenant_id` (SET LOCAL via
    `set_config`). The outbox `Relay` spans tenants and must connect as the
    table owner or a BYPASSRLS role. (`TestGraphPG_RowLevelSecurity`.)
15. **Seq numbering** is per program in Mem (one oam.Graph per program)
    and global `bigserial` in Postgres; both are strictly increasing per
    program, which is all WatchChanges and Diff rely on.
16. **jsonb numbers.** Postgres normalizes numeric literals (`1e2` becomes
    `100`); content compared after such a round trip could differ from
    Mem's textual `json.Number`. Tool adapters emit plain integers.

## Reads and analyses

17. **Stale records** are hidden from traversals (neighbors, path, blast
    radius, components, snapshots) and returned by GetEntity, FindEntities
    (state filter), Search and Export. (`TestGraphStaleness`.)
18. **Neighbors.** `DIRECTION_UNSPECIFIED` is rejected; depth 0 means 1,
    maximum 6; `max_nodes` default 1,000, maximum 10,000; the start entity
    is always returned; `since` (inclusive) filters edges and neighbours
    on `last_seen`. An edge is returned when it was followed from a node
    above the depth limit and both endpoints are in the result, so edges
    between two nodes at the last level are not. Entities are ordered by
    (distance, id), edges by id. (`TestGraphNeighbors`.)
19. **Path** is directed (from -> to), over active edges with confidence >
    0 between active entities; `max_cost` 0 means unbounded; endpoints in
    different programs give an empty path; `from == to` is a one-node
    path with confidence 1; more than 100,000 settled entities is
    RESOURCE_EXHAUSTED (`TRAVERSAL_LIMIT_EXCEEDED`). (`TestGraphPath`.)
20. **Blast radius** follows edges forward over `dns_record, port,
    certificate, contains, owns` (csr's list), default depth 3, ranked by
    (distance, id). **Components** are computed on a fresh CSR snapshot
    (whole-graph analytics use the snapshot, 21.4) and report its
    watermark.
21. **Diff.** `since` is inclusive on the event's `at`. `diff(run_a,
    run_b)` returns the events with seq in (last seq of run A, last seq of
    run B], or up to the newest event without run B: "what changed after
    run A, up to run B", including other writers' events in between. A
    run without change events is NOT_FOUND (its position is unknown).
    Results are change events (patches included), paged by seq tokens
    `{"seq": n}`. (`TestGraphDiff`.)
22. **Search.** `search_text` is the lower-cased, space-joined top-level
    string and number content values (field order), without `raw`,
    `username` and `password`, capped at 2 KiB. A hit is an exact
    substring (always) or, with `fuzzy`, pg_trgm `similarity()` at least
    0.3 (`Config.FuzzyThreshold`). Without `fuzzy` every hit is verified
    by the exact substring test, which is a plain scan for queries
    shorter than three characters. Score is the trigram similarity
    (computed in Go in both stores; Postgres uses the trigram index for
    candidates, ordered by SQL similarity with a small slack), ties by id;
    `matched_trigrams` are the shared trigrams. Word characters are
    Unicode letters and digits (pg_trgm follows the database locale; the
    two agree on ASCII). The spec example score 0.78 is illustrative:
    `jenkin` against `jenkins.acme.com` scores 6/18. (`TestGraphSearch`.)
23. **FindEntities** returns entities without tags; `seen_after` is
    inclusive; pages are ordered by id with `{"id": ...}` tokens.
24. **Change events carry an entity snapshot** (proto field 9): the state
    right after the operation in Ingest/SetStale results, the current
    state in ReadChanges, Diff, WatchChanges and the relay.
25. **WatchChanges** is a pull loop (`ReadChanges` pages of 500) that
    wakes on local commits and, in Postgres, polls every
    `PollInterval` (500 ms) for other writers. A cancelled context
    returns ABORTED (`CANCELLED`).
26. **Export** (not in the proto) returns a program's whole graph with
    tags, every state; it backs convergence tests and OAM export (5.1).

## Tags and snapshots

27. **AddTag** is applied as an observation from source `api` stamped with
    the clock, so API and ingested tags merge by the same rule; its
    observation id is derived from the tag and the instant. SourceProperty
    is rejected (provenance only comes from ingest), and tagging does not
    count as a sighting (oam). `Name` may replace the property's name
    field; when both are given they must agree. (`TestGraphAddTag`.)
28. **Snapshots.** Active entities in id order, active edges between
    them, label dictionary = sorted labels used, node type = a fixed
    asset-type table (0 = other), header `created_at` = the clock,
    `FlagStaleExcluded` always set (the covering-run rule; time-based
    ageing is not applied). Postgres reads the watermark and the graph in
    one repeatable-read transaction. Equal graphs, watermark and clock
    give equal sha256 in Mem and Postgres
    (`TestGraphSnapshot_Deterministic`).
29. **Catalogue.** Files are written as
    `snapshots/{tenant}/{program}/{watermark}.ffcs` through `BlobStore`;
    the catalogue keeps the first row per watermark (`snapshot_id`
    derived from tenant, program and watermark) and a repeated write
    returns it. GCS is a later `BlobStore`.

## For other owners

30. **archtest.** `graphstore/pg` imports pgx; `internal/archtest` has the
    adapter entry for it (`{prefix: "graphstore/pg", layer: layerAdapters,
    adapter: true, thirdParty: []string{"github.com/jackc/"}}`, before the
    `graphstore` one), so `TestArchDeps` passes.

## v2 graph expansion (V2 design §3)

Merge semantics (authority, coverage gate, tag_changed, base, cause) are
oam's: `oam/OPEN_QUESTIONS.md` 22-35. This section covers the stores.

31. **Authority bookkeeping in Postgres** lives in hidden tag rows next to
    `ff:meta` and `ff:run`, not in new columns: `ff:live` (name `""`) per
    record holds classic staleness, the profiles holding an edge
    (`auth_in`), an entity's authoritative references (`auth_refs`) and the
    support of every non-classic tag; `ff:auth` (name
    `<profile>/<family>`) on the source entity holds one unit (latest
    complete stamp, its members, newer partial members with stamps, and
    the target of every edge member). A row without `ff:live` was written
    before migration 0030 and loads as classic (`classic_stale = state =
    'stale'`, every tag classic). Reads drop withdrawn tags
    (`kernel.PublicTagRows`).
32. **Locking with authority.** A complete capture may withdraw edges the
    observation does not name. The store reads the source's `ff:auth`
    rows before taking the record locks (no lock), adds their members and
    targets to the lock set, then loads under lock; if the heads changed in
    between, the kernel reports `ErrMissingRecords` and the transaction is
    retried (up to `MaxRetries`). Lock order stays sorted, so this cannot
    deadlock. (`TestGraphIngest_ConcurrentAuthority`.)
33. **Covering runs** load only state, run rows and `ff:live`; they write
    back `ff:live` merged with its stored tag entries (`MergedLiveTag`), so
    a covering run never touches tag liveness.
34. **AsOf** replays from the empty graph, not from a CSR snapshot (the
    design's "nearest snapshot"): snapshots hold no content or tags. Reads
    use the `(entity_id, seq)` / `(edge_id, seq)` indexes of 0030 and
    replay only the records a read touches, so cost is proportional to
    their history. Not reconstructed (no events): confidence, times, runs,
    SourceProperty tags, edge content after creation; they are zero in the
    result. `since` is rejected by `NeighborsAsOf`. Seqs are per program in
    Mem and global in Postgres; both are comparable within a program.
35. **content_changed storage.** The `patch` column holds the
    `ContentChangePatch` object (not an op list); Go exposes it as
    `ChangeEvent.ContentChange`. The row is inserted under the program lock
    (seq order) with `ON CONFLICT (id) DO NOTHING`; a repeat returns the
    stored event. `At` zero means the clock. The gRPC/REST conversion in
    `services/` does not yet map `content_change`, `base`, `tag_type`,
    `tag_name`, `cause` or `baseline` (owned by the services agent).
36. **Cause and baseline** are stored as `change_events.cause` (NULL =
    target) and `change_events.baseline`; feeds filter on them. The
    `graph.observations` row does not record them.
37. **Commonness** is computed per request (`ComputeCommonness`): sources
    are distinct active entities with an active edge into the node (self
    loops excluded), holders their distinct hosts (`HolderHost`: URL, File
    and Endpoint by URL host, FQDN by name, IP by address, other types by
    entity id). Nothing is stored; `common` is `holders > 50`. Pivots and
    leads are expected to call it; no traversal filters on it yet.
38. **Migration numbering.** The v2 migration is `0030_graph_v2.sql`
    (0015-0029 are reserved for v1 fixes; 0015-0018 exist). The runner
    (`migrations.All`) allows gaps and rejects duplicate versions, so
    0019-0029 stay free for later v1 fixes.
