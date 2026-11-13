# contentstore

Versioned storage of captured content, the diff pipeline, encrypted
findings, learned volatility, retention, quotas and change search. Layer 4:
`core`, `oam`, `content`, `chunkstore`, `textindex`, `diff`, `sketch`.
`contentstore/pg` is the Postgres adapter (migration 0031, schema
`content`); `contentstore/storetest` is the Repo contract suite.

**Spec:** `docs/V2_DESIGN.md` §6.2, §6.3, §6.4, §7, §11 items 13, 14, 18,
and §12 (which wins): §12.11 redaction, §12.12 storage rules, §12.13
classes and newness, §12.14 search defaults; §12.2/§12.3 cause and
baseline on events.

## Pipeline

```
IngestRequest{program, Capture, entity, run, effect, page_url}
  │ signature = SHA-256(body hash, status, media type, headers@1 block of every hop)
  ├─ same (key, signature, fetched_at) stored ─────────────▶ Duplicate (nothing written)
  ├─ open version has the signature ───────────────────────▶ Unchanged: last_seen moves, capture counted,
  │                                                          observation re-published (stored processed
  │                                                          capture + decrypted findings, byte-identical)
  ├─ older than the open version ──────────────────────────▶ CAPTURE_OUT_OF_ORDER
  └─ new version
       content.Process(learned volatility of (url template, profile), tenant keyed hasher,
                       previous capture, host bodies, page url)
       lineage: no own predecessor + script/style → content.FindPredecessor over the page's
                previous scripts and the host's open versions (minus those sharing a lineage key
                with one of the page's scripts) → lineage parent (diff rebased)
       content.Compare(predecessor, cur) (baseline: every artifact against the empty document)
       blobs ── redact body and header values (spans sealed) ── delta codec ── chunk store
       rows ── one transaction: open-version check, exact quota check, close parent,
               snapshot, diff (seq), findings (AES-256-GCM), volatility, usage, outbox event
       then: retention of the key, inline indexing (IndexMode)
```

Degraded captures (V2 §12.5) are stored closed with an empty validity
interval, get no diff, and never become the open version.

Each event's payload (`EventPayload`) carries the change set's levels and,
for changes that are not baselines, `Excerpt`: at most 40 lines and
2 KiB of the unified hunks, level-3 artifacts first, from redacted text
and redacted once more (`TestCStoreEventExcerpt`); the notifier shows it
in alerts.

`SnapshotStorage` gives a snapshot's stored record bytes (raw: capture
record + redacted body; derived: normalized text + features) and
`StorageMetrics` the tenant's whole chunk store, so callers outside the
package measure CSTORE-INV-08's quantity (the V2 lab does;
`TestCStoreSnapshotStorage`).

## Records

| Blob (column) | Content |
|---|---|
| `headers_ref` | capture record: URL, profile, status, headers and redirect chain with header values redacted (canonical JSON) |
| `body_manifest` | the body with credential-shaped spans replaced by `content.RedactionPlaceholder` |
| `normalized_manifest` | the main artifact's normalized text (redacted by content) |
| `features_ref` | the `content.Processed` without texts and derived bodies, the other artifacts' normalized texts, and the feature-id history (for "returned") |
| `hunks_ref` | unified hunks by reference (`{"h": header, "o": "c3 d2 i1"}`, lines read back from the two snapshots' normalized texts; verbatim when that does not reproduce them); none for a baseline (recomputed) |
| `semantic_ref` | the change set without hunks, side-by-side rows and per-artifact feature changes; a baseline's changes are recomputed |

`body_seal` / `headers_seal` hold the redacted spans, and `findings` one
row per secret occurrence, all AES-256-GCM (`0x01 ‖ nonce ‖ ct ‖ tag`,
additional data naming tenant, snapshot and record) under the tenant's
`enc` data key. Data keys are 32 random bytes wrapped by the KEK
(`KeyEncrypter`: `LocalKEK` locally, a KMS adapter fits the same port);
keyed hashes use a separate `hash` data key (`oam.SecretKeyedHash`), so
they survive an `enc` rotation. Raw reads (`ContentOptions.Raw`) are the
only path that restores secrets; the service allows them to admins.

### Blob codecs (`Config.Codec`)

`delta` (default): each blob is a chunkstore delta frame ("FFZB", format in
`chunkstore/delta.go`): one zstd frame whose raw dictionary is the delta
base's same blob (parent, or lineage parent) and, for the normalized text
and features, this snapshot's body and normalized text. Chains are bounded
by `KeyframeInterval` (16); retention re-encodes a deleted base's
dependents as keyframes first. `chunk`: blobs stored plain; chunkstore's
FastCDC dedupe and per-chunk zstd do the work. Decoded blobs are cached
(`CacheBytes`, 64 MiB LRU).

## Search

One `textindex.Index` per (tenant, program); `Schema()`: text fields
`text`, `url`, `decoded`; keyword fields `program`, `url_template`,
`kind` (`hunk`, `<feature>_<op>` such as `endpoint_added`, `snapshot`),
`side` (`added`, `removed`, `modified`), `feature`, `mime`, `vendor`,
`class`, `newness`, `cause`, `profile`, `baseline`, `decoded_from`;
numeric `at`. Documents: per hunk its removed and added sides, per level-3
change one record, optionally (`Indexes.IndexLatest`) each key's latest
normalized snapshot. Text is cut at 64 KiB and redacted; base64, hex,
gzip, zlib and zstd blobs inside it are decoded by `content.DecodeBlobs`
(bounded, never inside a secret, redacted) into the `decoded` field, with
`decoded_from` naming the encodings. Ids are `seq<<20 | position`, so a
rebuild from the stored diffs reproduces the index; writes use `Update`,
so re-indexing after a crash is idempotent. Vendored bundles (node_modules
paths, vendor/framework/polyfills chunk names, all-vendor module sets) are
excluded unless the query names `vendor` or `IncludeVendor` is set.
`FirstAppeared` returns the earliest snapshot whose diff (baselines
included) added a string.

### Index mode (`Indexes.Mode`)

`inline` (default): `IndexPending` after each ingest. `batched`: a loop
calls it. Measured equal within noise on the SPA (below); inline keeps
"searchable when Ingest returns".

## Re-derive jobs (V2 §12.2)

Every snapshot records the stamp of the pipeline that derived it
(`Snapshot.DeriveStamp`; `Config.Pipeline`, default `content.Process`
stamped by `DerivationStamp()`: a hash over every normalizer's version and
rule table and every extractor's version; migration 0032 adds the column).
After an upgrade changes the stamp, `Store.Rederive(tenant,
RederiveOptions{Program, BatchSize})` re-derives up to `BatchSize` (64)
stale snapshots per call, oldest first per program, and reports
`Remaining`; the content service's maintenance sweep runs one batch per
tenant per pass (`services/content` `Sweep`, `RederiveTenant`,
`Options.RederiveBatch`). For each snapshot:

* the raw capture is rebuilt from the stored record and body (seals
  opened, body hash checked) and processed again with the learned
  volatility set it was derived with and its page URL (now in the capture
  record); its integrity verdict is kept (validity never moves);
* the normalized text, features record, kind, coverage, simhash and
  normalizer_version are rewritten in place, encoded as at ingest; every
  snapshot whose delta base it is gets re-encoded against the new blobs in
  the same transaction;
* the diff into it is recomputed against its (already re-derived)
  predecessor, keeping the diff's id and seq; a diff out of it into a
  still-stale successor keeps its content with its hunks inlined, and the
  successor's own re-derive recomputes it; changed diffs' index documents
  are replaced (the latest-snapshot document too);
* no snapshot, diff, finding, outbox event or capture count is added: a
  re-derive is not a change, so it raises no alert.

An ingest whose open version (or lineage predecessor) is stale re-derives
it first, so a new change is always computed between two snapshots of one
derivation. Concurrent ingests and jobs serialize on the predecessor row
(`Tx.LockSnapshot`) and retry on conflict. Progress lives in the stamps,
so an interrupted job resumes where it stopped and a repeated one does
nothing.

## Retention, GC, quotas

Per key: keep the newest `KeepLast` (50) ∪ fetched within `KeepFor`
(180 d) ∪ pinned ∪ the newest; diffs touching a deleted snapshot go
(cascade), their index documents are tombstoned. `ApplyRetention` then
runs chunk GC over the tenant's live and pinned manifests (chunkstore's
grace keeps blobs put since the previous GC began, so the next pass
reclaims them) and sets the stored-bytes usage to the store's physical
record bytes.

Quotas (`Limits`, per tenant, zero unlimited): captures per UTC day,
stored bytes, index bytes. Checked in the write transaction on a locked
usage row: a capture is admitted iff today's count is below the limit,
`stored + added <= limit` and the index is below its limit; otherwise
`QUOTA_EXCEEDED` (`Meta["quota"]`, `RetryAfter` until midnight for the
daily quota) and no rows. Chunks put by a refused capture are unreferenced
and reclaimed by GC.

## Errors

`*core.Error` with reasons `NOT_FOUND`, `VALIDATION_FAILED` (also query
errors, with `Meta["code"]` and `Meta["span"]` from textindex),
`QUOTA_EXCEEDED`, `CAPTURE_OUT_OF_ORDER` (failed precondition),
`VERSION_CONFLICT` (retried internally), `CONTENT_INTEGRITY` (stored
bytes failed verification), `INTERNAL`.

## Invariants (port these)

| Id | Test | Statement |
|---|---|---|
| CSTORE-INV-01 | `TestCStoreInv01_IngestIdempotent` | The same capture ingested again, sequentially or concurrently, is one snapshot: no new row, diff, finding, event, index document or quota count. |
| CSTORE-INV-02 | `TestCStoreInv02_TenantIsolation` | Identical content of two tenants is paid for by each (disjoint chunk namespaces, no dedupe benefit); no read, diff, pin, search hit or first-appeared hit crosses tenants; ids, keyed hashes and wrapped keys differ; one tenant's key cannot open the other's ciphertext. |
| CSTORE-INV-03 | `TestCStoreInv03_RetentionKeepsPinnedAndNewest` | Retention keeps exactly newest-KeepLast ∪ within-KeepFor ∪ pinned ∪ newest per key; diffs follow; survivors read back byte-exactly after keyframe rewrites; GC reclaims; stored-bytes usage equals the chunk bytes. |
| CSTORE-INV-04 | `TestCStoreInv04_DiffDeterminism` | Independent stores fed the same captures produce identical ids, manifests and change sets; `Diff` is byte-identical across calls; stored hunks and changes equal a recomputation. |
| CSTORE-INV-05 | `TestCStoreInv05_RebuildEqualsIncremental` | Inline, batched and rebuilt indexes hold the same documents under the same ids and answer a query set identically; a lost index is rebuilt from the watermark. |
| CSTORE-INV-06 | `TestCStoreInv06_QuotaExact` | Exactly N captures per day under a limit N (also concurrently), refusals store nothing, duplicates are free; stored-bytes limits admit iff usage + added ≤ limit; index-bytes limits refuse once reached. |
| CSTORE-INV-07 | `TestCStoreInv07_EncryptionRoundTripNoPlaintext` | For a secret of every named rule in a body and a header, findings decrypt to it, raw reads restore body and headers exactly, and no row, chunk object, decompressed record, decoded blob, diff record, index document or file, or outbox event holds it; tampering and other tenants' keys fail. |
| CSTORE-INV-08 | `TestCStoreInv08_SPAStorageUnderQuarter` | The SPA fixture's versions (raw snapshots: redacted bodies and capture records) take < 25 % of their raw size with the default codec; derived records are reported. |
| CSTORE-INV-09 | `TestCStoreInv09_ValidityIntervals` | Contiguous, disjoint `[valid_from, valid_to)` per key with one open version; unchanged re-fetches add no row; degraded captures are closed, empty and never open; as-of lookups return the containing version; unseen older captures are refused. |
| CSTORE-INV-10 | `TestCStoreInv10_SearchDefaults` | Hunk sides are separate documents; vendored bundles are excluded unless asked; decoded blobs are searchable in `decoded` only; first appeared returns the earliest adding snapshot, baselines included; query errors are VALIDATION_FAILED. |
| CSTORE-INV-11 | `TestCStoreInv11_SPAEndToEnd` | Renamed bundles link to their predecessors (lineage key, then SimHash) and an unrelated chunk does not; masked-only versions produce no documents; planted changes are findable by field query and regex; events carry a decodable observation and a valid patch; baselines are not alertable, the planted change is. |
| CSTORE-INV-12 | `TestCStoreInv12_VolatilityLearned` | A non-feature attribute changing on every capture of a stable page is frozen once (version 1), later captures use the learned normalizer and diff masked-only; feature paths are never counted. |
| CSTORE-INV-13 | `TestCStoreInv13_ObservationReplay` | The re-published observation of an unchanged re-fetch equals processing the capture afresh, byte for byte. |
| CSTORE-INV-14 | `TestCStoreInv14_RederiveIdempotent` (+`Postgres`) | After a normalizer upgrade, re-deriving in batches of one, in one batch, and with three concurrent jobs reach the same state, every snapshot carries the new stamp, and a second run re-derives nothing and changes nothing; a new stamp with an unchanged pipeline rewrites no record. |
| CSTORE-INV-15 | `TestCStoreInv15_RederiveNoChange` (+`Postgres`) | Re-derive inserts no snapshot, diff, finding or outbox event, allocates no diff seq, closes or touches no version and learns no volatility; ids, sides, validity, lineage and seqs are kept and the new normalizer_version recorded; an ingest after the upgrade re-derives its predecessor first, so its diff is between snapshots of one derivation. |
| CSTORE-INV-16 | `TestCStoreInv16_RederiveEqualsFreshIngest` (+`Postgres`) | After re-derive, every snapshot's normalized text, features record and derived columns, every diff's levels, stats, hunks and change set, and the search and first-appeared answers of a query set equal those of a store that ingested the same captures under the new pipeline. |

CSTORE-INV-14..16 live in `storetest` (`RederiveIdempotent`,
`RederiveNoChange`, `RederiveEqualsFresh`, with `UpgradedPipeline` as the
stand-in upgrade) so the Postgres adapter runs them too.

Contract suite (`storetest.Run`): `TestCStoreContract_Mem`,
`TestCStoreContract_Postgres` (as role `ff_content`, RLS on; skipped
without `FF_TEST_PG_DSN`), plus `TestCStoreRegress_RLSWithoutTenant`.
Fuzz targets: `FuzzCStoreSealRoundTrip` (redact → restore identity),
`FuzzCStoreHunkRoundTrip` (compact hunks expand to the presented lines),
`FuzzCStoreRecordDecode` (decoders are total).

## Benchmarks

`go test -run '^$' -bench CStore -benchtime 3x -benchmem ./contentstore`;
baseline `bench/results/contentstore/`. 4-vCPU Xeon, Go 1.26, 2026-11-13:

| Benchmark | Result |
|---|---|
| `CStoreAlgo/codec/delta/versions50` (50 × ~300 KB bundle, small edits) | stored 4.70 % of raw, 180 ms/version |
| `CStoreAlgo/codec/chunk/versions50` | stored 9.87 %, 159 ms/version |
| `CStoreAlgo/codec/delta/spa` (everything stored, SPA fixture) | 59.8 % of raw, 175 ms for 9 captures |
| `CStoreAlgo/codec/chunk/spa` | 95.1 %, 71 ms |
| `CStoreAlgo/index/inline/spa` vs `batched` | 231 ms vs 248 ms |
| `CStoreIngest` (one new ~300 KB bundle version, inline index) | 279 ms, 1.1 MB/s (dominated by `content.Process` and the diff) |
| `CStoreSearch` (term, field, phrase, regex over 39 captures' diffs) | 214 µs/query |

SPA raw snapshots (`CSTORE-INV-08`): delta 15,368 B of 71,825 B (21.4 %),
chunk 23,008 B (32.0 %).

## Running

```sh
go test ./contentstore/...
FF_TEST_PG_DSN=postgres://... go test ./contentstore/pg
go test ./contentstore -run '^$' -fuzz FuzzCStoreSealRoundTrip -fuzztime 30s
```

Known limitations: `OPEN_QUESTIONS.md`.
