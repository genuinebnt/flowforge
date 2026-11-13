# contentstore: open questions and decisions

1. **What "storage < 25 % of raw" measures.** V2 §10.7 asks that all
   versions be stored in less than 25 % of their raw size. The SPA fixture
   (nine small, mostly unrelated files) cannot meet that if the derived
   records count: the raw snapshots alone (redacted bodies and capture
   records) take 21.4 % with the delta codec, but normalized texts,
   features (module hashes are incompressible SHA-256s), and diff
   records bring the total to 59.8 %. CSTORE-INV-08 asserts the raw
   snapshots (what is needed to reproduce every version) and logs the
   total. On realistic histories (50 versions of a bundle with small
   edits) the total is 4.7 % (`BenchmarkCStoreAlgo/codec/delta/versions50`).
   If the total must meet 25 % on small fixtures, the features record
   would have to be re-derived from the raw body on demand instead of
   stored.
2. **Delta codec vs chunk store dedupe.** Chunk-level dedupe does nothing
   for small files and renames with renamed locals; per-chunk zstd loses
   against whole-blob zstd on small blobs. The delta codec wins on both
   benchmarks (docs/ALGORITHMS.md) at some CPU cost. Delta frames are
   produced and decoded by `chunkstore` (`EncodeDelta`/`DecodeDelta`)
   because zstd raw dictionaries are not available through
   `codec.Inflate`; if `codec` gains a raw-dictionary option, decoding can
   move there.
3. **Single writer per tenant chunk store.** `chunkstore` assumes one
   writer per namespace, and the GC gate is per process. Run one content
   service replica, or partition tenants across replicas (G-CST-03, v3).
   Storage: `FF_CONTENT_BUCKET` (Cloud Storage, `chunkstore/gcsstore`),
   `FF_CONTENT_DIR` (file system) or memory. On GCS every write is
   conditional on the generation last observed, so a second replica's
   flush or GC fails with `gcsstore.ErrConflict` instead of replacing the
   ref, and its sweep refuses to delete once the ref moved; a narrow window
   between that check and a delete remains until leases exist.
4. **Stored-bytes accounting.** Usage grows by the record bytes each
   ingest adds and is reset to the store's physical record bytes after
   each GC; bytes of a capture refused by the stored-bytes quota after its
   chunks were written are physically present until GC and are not
   counted until then.
5. **Out-of-order captures** (older than the open version, not a re-send)
   are refused (`CAPTURE_OUT_OF_ORDER`) rather than inserted into the
   history: inserting would re-parent and re-diff later versions.
6. **Volatility observations** come from comparing the normalized main
   artifact and headers of consecutive versions when the capture had no
   level-3 change: HTML attribute values (html@1 lines), JSON leaves
   (array indices as `*`) and header values. Unchanged re-fetches are not
   counted (they would bias every path towards stable). Re-derive jobs on
   a normalizer upgrade (V2 §12.2) are not implemented here.
7. **Lineage candidates** are the page's previous scripts plus the
   host's other open versions of the same kind (bounded to 64): lazily
   loaded chunks have no page reference. The matcher's threshold keeps
   unrelated bundles out (CONTENT-INV-08, CSTORE-INV-11). A host version
   whose lineage key equals one of the page's scripts' is left out: every
   earlier rename of a bundle stays an open version (nothing re-fetches a
   URL the site stopped serving), so after two renames the key matched
   several candidates and SimHash picked the oldest generation, diffing
   the third bundle against the first (found by the V2 lab;
   `TestCStoreRegress_LineagePrefersPageScripts`).
8. **Alerts.** Content events carry `alertable` (not a baseline, cause
   target, not pre-existing, a change). The notifier alerts on
   content.changes only and ignores the graph's `graph.content_changed`
   copy, so one change alerts once.
9. **content.Redact idempotence (resolved).** `FuzzCStoreSealRoundTrip`
   found that `content.Redact` was not idempotent on
   `"ABIA0000000000000000postgres://:0@"`: redacting the AWS key id
   exposed a connection-string password the first scan had skipped. Redact
   now rescans until stable (content OPEN_QUESTIONS, CONTENT-INV-10); the
   input stays in `testdata/fuzz` and seeds `FuzzContentRedactIdempotent`.
10. **First appeared** verifies candidates by exact substring on the
    stored hunk text; candidates come from a phrase query (or a regex for
    text without word tokens), at most 10,000.
11. **Re-derivation records no cause = normalizer change (decision).**
    V2 §12.2 says re-derived snapshots' resulting changes are "recorded as
    cause = parser/normalizer". The store instead treats re-derivation as
    not a change at all: the diffs touching a re-derived snapshot are
    recomputed between snapshots of the new derivation (or keep their old
    content until the other side is re-derived), and no event, alert or
    diff row is added (CSTORE-INV-15). Comparing an old derivation with a
    new one only ever measures the reader, which is the noise §12.2 wants
    hidden from feeds; content's `causeOf` still labels such a pair
    `normalizer`/`parser` if one is diffed on demand (`Store.Diff`).
12. **What re-derivation does not recompute.** The integrity verdict
    (degraded or not) is kept, because the host's catch-all sample it was
    judged against is not stored; if the old capture was complete and the
    new pipeline alone would grade it degraded, coverage keeps the new
    pipeline's (failed) value, which is conservative. Learned volatility
    is not re-learned: each snapshot is re-derived with the learned set
    named by its normalizer_version. Findings are append-only: secrets a
    new extractor finds are not added to old snapshots, and old finding
    rows stay. A pipeline change that changes lineage fingerprints does not
    re-link renamed scripts. Index documents are replaced right after the
    commit (like retention's tombstones): a crash in between leaves stale
    documents until `Rebuild`.
13. **Rows from before migration 0032** have an empty `derive_stamp`, so
    the first sweep after the upgrade visits every snapshot once; with an
    unchanged pipeline it rewrites only the stamps (CSTORE-INV-14).
