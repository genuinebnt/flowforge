# vecindex: open questions and decisions

Each item names the spec text, the ambiguity, the choice made and the test
that pins it.

## Graph construction (22.4)

1. **robust_prune candidates.** The spec prunes `N_out(p) = robust_prune(p,
   V, alpha, R)` over the visited set only; the DiskANN paper uses
   `V ∪ N_out(p)`. We follow the spec. Recall targets are met
   (`TestVec005_Recall`).
2. **Medoid.** "Closest point to the centroid" is measured with squared L2
   for every metric (for inner product, in the augmented space of item 3).
3. **Inner product is not a metric.** With distance `-dot` the alpha rule
   (`alpha * d(p*, v) <= d(p, v)`) is meaningless on negative values and
   recall collapsed (about 0.5). IP graphs are therefore built with L2 over
   the standard MIPS-to-L2 reduction (append `sqrt(M^2 - |x|^2)`, M the
   largest norm); a query augmented with 0 ranks exactly as `-dot`, so
   searches and stored vectors are unchanged. Pinned by `TestSegmentMetrics`.
4. **Reachability.** Vamana does not guarantee every node is reachable from
   the medoid (small graphs with small R strand nodes), and vec_004 requires
   it. A post-pass links each stranded node from the nearest reachable node
   with spare degree (or swaps out a full node's farthest neighbour). Not in
   the spec; the degree bound holds. Pinned by `TestVec004_VamanaInvariants`.
5. **Parallel build.** Optional `Batch`/`Workers`: points of a batch search
   and prune against one snapshot, back edges are applied per target in
   batch order. The graph depends on `Batch` (Batch 1 is the spec's
   sequential algorithm) but never on `Workers`. Pinned by the
   "deterministic and independent of workers" subtest.

## Segment format (22.5)

6. **Records larger than a sector.** dim 960 with R 64 gives 4,100 B, so "a
   record never straddles a sector" cannot hold. Such records start on a
   sector boundary and occupy `ceil(record / 4096)` sectors
   (`TestSegmentAccessors`, dim 1100).
7. **Checksums.** The spec asks only that corruption fail open (vec_007).
   We use CRC32C per section and on the header, and a trailer with
   CRC32C(body || header). Open reads and checks the whole file, which costs
   O(file) I/O at open; very large segments would want lazy per-sector
   verification. Every single-byte flip gives `ErrChecksum`
   (`TestVec007_SegmentCorruption`).
8. **Tombstone epoch** in the header records the index generation (flush or
   merge counter) at build time. Live tombstones are an in-RAM bitset per
   segment, not persisted.
9. **PQ parameters.** The header stores m and k. When dim is not a multiple
   of m, the first `dim % m` sub-spaces get one extra coordinate. Codebooks
   are trained with L2 k-means for every metric; IP and cosine tables use
   `-dot` partials (plus 1 for cosine).

## Index behaviour (22.3, 22.7, 22.9)

10. **Seq ordering.** A write applies only if its seq is strictly newer than
    the last write (upsert or delete) applied for that id. Equal seq is a
    replay and a no-op. A delete of an unknown id is recorded, so a stale
    upsert delivered later is ignored. The per-id seq map is never compacted.
    `content_hash` no-op detection is the service's job (the index never sees
    text). Pinned by `conformance/vecindex/bruteforce/seq_ordering.json`.
11. **Port signatures.** Methods follow the spec's `VectorIndex` trait: no
    `context.Context` (segment reads are local and bounded) and `Upsert`
    returns only an error (the proto's `applied` flag is not surfaced). The
    info type is `IndexInfo`, after the proto message.
12. **Merge triggers.** "Tombstones exceed 20%" is strict (`> 0.2 * vectors`):
    a segment at exactly 20% is not merged. "Segment count exceeds 8" merges
    the smallest segments until at most 8 remain. Merges are rebuilds, not
    FreshDiskANN streaming merges (README). Pinned by
    `TestSegmentedAutoMerge`.
13. **Over-fetch and retry.** Segments are asked for `k * 2` (`k * 4` with a
    filter) with `L >= that`. When fewer than k survive tombstones and
    filters, the fetch and L double until k survive or every segment is
    exhausted. The memtable is exact and applies filters inline.
14. **Filters.** The proto's asset_types, tags and last_seen_after need
    entity metadata the index does not hold, so a filter is a caller
    predicate (`Filter.Keep`). The exact fallback ("fewer than 1% match")
    uses `Filter.MatchingIDs`, the caller's list of matching ids, and applies
    when that list is shorter than 1% of the live vectors.
15. **SearchParams.** `L = 0` means `max(100, k)`. `L < k` is rejected, as
    the proto says. `BeamWidth` (W of 22.5) is an addition to the proto
    message. `include_out_of_scope` and `Hit.in_scope` are service concerns
    and not represented.
16. **Isolation key.** The spec says one index per (tenant_id, program_id,
    model_id), but graph.vector_indexes is unique on model_version too, and
    22.10 builds a second index for a new model version. `Key.Model` should
    therefore carry the version (for example `e5-base@2`).
17. **Cosine zero vectors** cannot be normalised and are rejected with
    `ErrInvalidVector`, as are NaN and infinite components under any metric.

## Hybrid and near-duplicate detection (22.6, 22.8)

18. **RRF example rounding.** The spec rounds each term before summing
    (0.01639 + 0.01587 = 0.03226); the exact value is 0.032266. The
    conformance tolerance is 1e-5. Ties are broken by id, and a duplicate
    within one list counts once, at its best rank. In fused results,
    `Distance` is +Inf for hits that only the lexical list returned.
19. **SimHash details** are not fixed by the spec. We chose: FNV-1a 64 of the
    UTF-8 bytes; tokens are maximal Unicode letter/digit runs after
    lower-casing; 3-token shingles joined by one space; every occurrence
    votes +1/-1; a bit is set only when its sum is strictly positive. All are
    pinned by `conformance/vecindex/simhash`. With 3-token shingles a
    one-word edit in a short text moves the hash by about 5 bits, more than
    the threshold of 3; that is a property of the chosen shingling.
20. **MinHash details.** The permutation family is
    `(a x + b) mod (2^61 - 1)` over `x = FNV-1a64 mod p`, with coefficients
    from SplitMix64(seed), default seed `0x46464d494e484153`. The empty set's
    signature is all `0xffffffffffffffff`, and two empty sets have Jaccard
    similarity 1. LSH band keys are FNV-1a over the rows' little-endian
    bytes. All are pinned by `conformance/vecindex/minhash`.
