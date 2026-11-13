# conformance/graphstore

Store-level graph conformance cases (spec Part II 28.8, G-SPEC-07), run by
`graphstore` against `Mem` (`graphstore/conformance_test.go`) and against
Postgres with `Mem` as the oracle (`graphstore/pg/conformance_test.go`,
skipped unless `FF_TEST_PG_DSN` is set). Required ids are in
`modules.toml` (`[module.graphstore]`, `TestGraph<NNN>_*`).

Each `gs_NNN_*.json` uses the `conformance/oam/merge` schema (steps with
`observation`, `complete_run`, `error`, `pending`; `scope`,
`stale_after_runs`, `order_dependent`; `expect.entities`, `expect.edges`
and `expect.changes`, compared exactly) and adds:

* `id`: the case id (`gs_NNN`);
* a step kind `content_change`: `{entity (key), run_id, at, patch, repeat,
  error}`; `repeat` asserts the store returns the event of the identical
  earlier request and appends nothing; `error` is `invalid_argument` or
  `not_found`;
* `as_of`: reads evaluated after all steps at the change seq reached after
  the first `after_step` steps (seqs are store-local, so files name steps):
  `entities` by key (`exists: false` means NOT_FOUND; `content`, `state`,
  `scope_status`, `tags` compared when present) and `neighbors` by key,
  `direction` (`out`, `in`, `both`), `depth` and `labels`, with entities as
  `"<key> <state>"` and edges as `"<from key> <label> <to key> <state>"`,
  compared as sets.

A case with covering runs or content changes must be `order_dependent`;
the others are also replayed reversed and duplicated and must converge.

Expected values were written by hand from the rules of spec 5.7 and Part
II 28 and confirmed against both stores; change them only with a written
justification.
