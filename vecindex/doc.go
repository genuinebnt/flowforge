// Package vecindex implements Flowforge similarity search (spec section 22):
// "find assets like this one" over the embeddings of one (tenant, program,
// model) triple. Similarity is only a hint; callers pass every hit through
// the scope service before returning or acting on it.
//
// The package covers:
//
//   - Metrics and distance kernels (22.2): L2 (squared), Cosine (vectors
//     normalised at insert, distance 1 - dot) and InnerProduct (-dot), each
//     with a scalar reference kernel and an unrolled, bounds-check-free
//     pure-Go kernel (distance.go).
//   - The Index port (VectorIndex of 24.4, contract of 22.9): Upsert,
//     Delete, Search and Info, writes ordered by source seq, hits sorted by
//     distance with ties broken by id. BruteForce is the exact
//     implementation and ground truth (bruteforce.go).
//   - Product quantisation (22.5): deterministic k-means++ / Lloyd per
//     sub-space with 256 centroids, encode, decode and per-query m x 256
//     asymmetric distance tables (pq.go, kmeans.go).
//   - Vamana graph construction (22.4): random R-regular initialisation,
//     medoid entry point, two passes (alpha 1.0 then alpha), greedy_search
//     and robust_prune, with an optional deterministic batch-parallel build
//     (vamana.go).
//   - The "FFDA" segment file and beam search (22.5): sector-aligned node
//     records, PQ codes and codebooks and an id table, per-section and
//     whole-file CRC32C; beam search navigates by PQ distance from RAM and
//     re-ranks with the full vectors read from the file (segment.go).
//   - The segmented index (22.3, 22.7): brute-force memtable, immutable
//     segments, tombstones with over-fetch, merges that physically drop
//     deleted vectors, post-filters with retry and an exact fallback for
//     highly selective filters (segmented.go, memtable.go).
//   - Per-(tenant, program, model) isolation (22.3): Registry (registry.go).
//   - Hybrid search by reciprocal rank fusion (22.6, rrf.go).
//   - Near-duplicate detection without embeddings (22.8): SimHash with
//     multi-index hashing and MinHash with banded LSH. The algorithms live
//     in package sketch (shared with content); simhash.go and minhash.go
//     are thin wrappers keeping this package's API and errors.
//
// Everything is deterministic: every random choice comes from a seeded
// core.Rng, and no code reads the clock or the environment. The package
// imports only the standard library, core and sketch.
package vecindex
