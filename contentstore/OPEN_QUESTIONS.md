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
   service replica, or partition tenants across replicas. A GCS chunk
   backend does not exist yet; `FF_CONTENT_DIR` (file system) or memory.
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
   unrelated bundles out (CONTENT-INV-08, CSTORE-INV-11).
8. **Alerts.** Content events carry `alertable` (not a baseline, cause
   target, not pre-existing, a change). The notifier alerts on
   content.changes only and ignores the graph's `graph.content_changed`
   copy, so one change alerts once.
9. **content.Redact idempotence.** `FuzzCStoreSealRoundTrip` found that
   `content.Redact` is not idempotent on
   `"ABIA0000000000000000postgres://:0@"`: the first pass redacts the AWS
   key id, the second then finds a one-character connection-string
   password. CONTENT-INV-10 states idempotence; the input is kept in
   `testdata/fuzz` (the seal round trip itself holds).
10. **First appeared** verifies candidates by exact substring on the
    stored hunk text; candidates come from a phrase query (or a regex for
    text without word tokens), at most 10,000.
