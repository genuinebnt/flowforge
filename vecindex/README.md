# vecindex

Similarity search for Flowforge (spec section 22): exact and approximate
nearest-neighbour search over entity embeddings, one index per
(tenant, program, model), plus embedding-free near-duplicate detection.
Layer 2; imports only the standard library and `core`.

## Spec coverage

| Spec | What | Where |
|------|------|-------|
| 22.2 | L2 / cosine / inner product; scalar reference and optimised kernels | `distance.go` |
| 22.3 | memtable, segments, tombstones, merge, isolation | `segmented.go`, `memtable.go`, `registry.go` |
| 22.4 | Vamana build: random R-regular init, medoid, two passes, greedy_search, robust_prune | `vamana.go` |
| 22.5 | "FFDA" segment file, beam search, product quantisation | `segment.go`, `pq.go`, `kmeans.go` |
| 22.6 | Hybrid search by reciprocal rank fusion | `rrf.go` |
| 22.7 | Post-filters with over-fetch and retry, exact fallback | `segmented.go` |
| 22.8 | SimHash + multi-index hashing, MinHash + LSH | `simhash.go`, `minhash.go` |
| 22.9, 24.4 | `Index` port (VectorIndex), `SearchParams`, `Hit`, `IndexInfo` | `index.go` |
| 25.3-25.8 | vec_001..008, properties, fuzz, b_vec_01..05 | `*_test.go` |

## API at a glance

```go
idx, _ := vecindex.NewSegmented(vecindex.Config{Dir: dir, Dim: 768, Metric: vecindex.Cosine, Seed: 1})
_ = idx.Upsert(entityID, vec, sourceSeq)          // ignored unless sourceSeq is newer
hits, _ := idx.Search(q, vecindex.SearchParams{K: 10}) // L defaults to 100, beam width to 4
_ = idx.Delete(entityID, sourceSeq)               // hidden immediately; dropped from disk at merge

reg := vecindex.NewRegistry()                     // one index per (tenant, program, model)
_ = reg.Register(vecindex.Key{Tenant: t, Program: p, Model: "e5-base@2"}, idx)
```

* `Index` is the port; `BruteForce` (exact, the in-memory fake and recall
  ground truth) and `Segmented` implement it.
* `BuildGraph`/`Graph` (in-memory Vamana), `WriteSegment`/`OpenSegment`/
  `Segment` (on-disk DiskANN segment), `TrainPQ`/`PQ`/`DistanceTable`.
* `RRF`, `HybridSearch`; `SimHash`, `SimHashText`, `SimHashIndex`,
  `MinHasher`, `LSH`, `CollisionProbability`.

### Segment file ("FFDA", version 1)

Sector 0 is the header (magic, version, dim, metric, R, V, medoid, PQ m and
k, record size, tombstone epoch, max source seq, section extents and
CRC32Cs, file size; CRC32C of the sector in its last 4 bytes). Then the
node section (records `vector f32[dim] | degree u32 | neighbours u32[R]`,
never straddling a sector; a record larger than a sector starts on its own
sector), the PQ codebooks, the PQ codes and the id table
(`uuid | source_seq`), each sector-aligned. An 8-byte trailer holds
`"FFDT"` and CRC32C(body || header). `OpenSegment` verifies every checksum
and validates every record, so a flipped byte fails with `ErrChecksum`.
RAM per segment: PQ codes, codebooks, id table; full vectors and adjacency
are read per search through `io.ReaderAt`.

## Tests and benchmarks

```sh
go test ./vecindex/...                    # full: vec_004/005 on 10,000 vectors
go test -race -short ./vecindex/...       # short: 2,000 vectors, fewer property cases
FF_SEED=123 FF_PROPTEST_CASES=100000 go test -run Prop ./vecindex/
go test ./vecindex -run '^$' -fuzz FuzzSegmentOpen -fuzztime 60s -fuzzminimizetime 5s
go test ./vecindex -run '^$' -bench Vec01
go test ./vecindex -run '^$' -bench 'Vec0[2-5]' -benchtime=1x
```

The fuzz target re-seals every input's checksums before a second open so
the fuzzer reaches the structural validation behind them. Inputs are
sector-sized (about 20 KiB), so keep `-fuzzminimizetime` short: Go's default
60 s minimisation of each new input otherwise looks like a stall.

Conformance data: `conformance/vecindex/{bruteforce,pq,rrf,simhash,minhash}`.
The vec_004/vec_005 corpus is a seeded "SIFT-like" Gaussian mixture
(64 clusters in [0, 100)^128, a 16-d latent component and isotropic noise;
`helpers_test.go`, generator `gaussian_mixture_v1`).

### Baseline (4 shared vCPUs, Intel Xeon, Go 1.26, one run)

b_vec_03, one segment of 20,000 x 128-d vectors (R 64, L_build 100,
alpha 1.2, PQ m 16), 200 queries, k = 10:

| L | W | recall@10 | node reads/query | p50 | p99 | QPS (1 core) |
|--:|--:|----------:|-----------------:|----:|----:|----:|
| 10 | 4 | 0.504 | 20 | 46 us | 107 us | 20,500 |
| 50 | 4 | 0.898 | 60 | 88 us | 185 us | 10,600 |
| 100 | 4 | 0.975 | 112 | 172 us | 361 us | 5,900 |
| 200 | 4 | 1.000 | 214 | 188 us | 356 us | 5,000 |
| 100 | 1 | 0.980 | 112 | 168 us | 377 us | 6,100 |
| 100 | 8 | 0.990 | 115 | 135 us | 311 us | 7,100 |

The segment is read from memory here (`io.ReaderAt` over a byte slice), so
latency is CPU only; on SSD it adds one read round trip per beam iteration.
vec_005 (10,000 vectors, sequential build) reaches recall@10 = 1.00 at L = 100.

Other baselines from the same run:

* b_vec_01 kernels (ns/distance, scalar -> unrolled): dim 128 L2 56 -> 40,
  IP 54 -> 40; dim 768 IP 520 -> 246; dim 960 L2 701 -> 321. About 1.4-2.2x,
  not the "several times" an AVX2 kernel would give (see limitations).
* b_vec_02 build of 20,000 vectors: sequential 10.5 s; batched (256) 7.0 s on
  1 worker, 4.1 s on 4, 3.6 s on 8 (the three batched graphs are identical).
* b_vec_04 bytes per vector (5,000 x 128-d): RAM 40 / 56 / 88 B for
  m = 16 / 32 / 64 (PQ code + 24 B id entry; codebooks add 128 KiB per
  segment), disk 887 / 903 / 935 B (full vector 512 B + adjacency 260 B +
  code + id).
* b_vec_05 streaming, 3,000 x (insert + delete + query) on 10,000 vectors:
  5 merges, recall@10 1.00 static and while streaming (drift 0 points),
  slowest write (a synchronous flush or merge) 341 ms.

## Known limitations and future work

* **No SIMD assembly.** Go has no portable SIMD intrinsics; the optimised
  kernels are 8-way unrolled pure Go with eight accumulators and no bounds
  checks. AVX2/NEON kernels need Go assembly (`distance_amd64.s`) behind
  the same functions, tested against the scalar reference by vec_001.
* **pgvector adapter** (`vecindex/pgvector`, spec 22.5 "first version": HNSW
  with `vector_cosine_ops` behind the same `Index` port) is future work; the
  extension is not installed locally.
* **Merges rebuild** (new Vamana graph and new PQ codebooks from the live
  vectors) instead of FreshDiskANN's streaming merge (Vamana insert of new
  points, patching neighbours of deleted ones). Flushes and merges run
  synchronously under the write lock; a service wanting a background builder
  sets `DisableAutoMerge` and a large `FlushThreshold` and calls `Flush` /
  `Merge` itself.
* **No recovery from disk.** Tombstones and the seq/version map live in RAM;
  `NewSegmented` does not load existing files. The index is derived from
  `graph.embeddings`, so recovery is a replay; GCS replication is a service
  concern.
* Beam search reads the W sectors of an iteration sequentially (no
  concurrent reads or io_uring) and has no hot-node cache.
* The memtable is always brute force (it flushes at 50,000 by default, the
  spec's brute-force limit); there is no age-based flush (no clock here).
* See `OPEN_QUESTIONS.md` for spec ambiguities and the choices made.
