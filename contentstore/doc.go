// Package contentstore stores captured content as versioned snapshots and
// serves their diffs and change search (docs/V2_DESIGN.md §6.2 snapshot
// records, §6.3 compression points, §6.4 secrets, §7 search, §11 items 13,
// 14 and 18, and the §12 amendments that win over them: no cross-tenant
// dedupe, pinning, validity intervals and unchanged re-fetches (§12.12),
// redaction before storage and indexing (§12.11), cause, baseline,
// classes and newness (§12.2, §12.3, §12.13), search defaults (§12.14),
// re-derive jobs (§12.2)).
//
// Layer 4: it composes the pure content pipeline (package content) with
// the chunk store (per-tenant namespaces), the text index (one index per
// tenant and program) and the Repo port (MemRepo; contentstore/pg). It
// does no network I/O of its own, reads no clock (core.Clock) and draws
// randomness only from core.Rng (data keys, GCM nonces).
//
// Ingest: a capture is either an exact re-send (one snapshot,
// CSTORE-INV-01), an unchanged re-fetch (last_seen moves on the open
// version), or a new version: processed with the key's learned volatility
// and the tenant's keyed hasher, linked to its predecessor (parent, or a
// lineage parent found by content.FindPredecessor for a renamed script),
// diffed and classified, stored (redacted body, capture record,
// normalized text, features; compact hunks and semantic records), its
// secrets and redaction seals encrypted with the tenant's data key, quota
// usage counted exactly, and one content.changes outbox event written in
// the same transaction.
//
// Blobs go through a blob codec (BlobCodec, a pluggable decision): the
// default delta codec stores each blob as a zstd frame against its delta
// base's blob (chunkstore delta frames), keyframes bounding the chains;
// the chunk codec leaves dedupe and compression to chunkstore. Retention
// rewrites the dependents of a deleted base as keyframes first, and chunk
// GC reclaims what no row references.
//
// Re-derive (V2 §12.2, Rederive): snapshots derived by an older pipeline
// stamp are re-derived from their stored raw bodies in place, their diffs
// recomputed between snapshots of one derivation and re-indexed, without
// adding a version, diff, event or alert.
//
// PORT: no map iteration order reaches an output (keys are sorted before
// they are); record encodings are canonical JSON (core.CanonicalJSON);
// derived ids are SHA-256 over length-prefixed parts (derivedUUID).
package contentstore
