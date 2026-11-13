# Algorithm decisions

Every place where several algorithms or strategies were measured and one
was chosen. All candidates stay in the code, selectable by configuration
(`docs/CONVENTIONS.md`, "Pluggable algorithms"). The comparative
benchmark of each entry prints a side-by-side table:

```bash
go test -run='^$' -bench <Pkg>Algo -benchmem ./<pkg>
```

Measurements below come from the 4-vCPU Xeon development machine on
2026-11-12/13, often with other builds running, so treat them as relative.
Re-measure before relying on absolute numbers. Committed baselines are in
`bench/results/<module>/`.

Entries marked **retrofit** predate this rule. Their alternatives exist
but are not yet all selectable in non-test code or behind an
`<Pkg>Algo` benchmark; closing that is tracked in `docs/GAPS.md`
(G-ALG-*).

## v1 decisions

### Rate-limiting algorithm (`limiter`)
* **Candidates:** GCRA (mutex), GCRA (lock-free atomic CAS), token bucket,
  sliding-window counter, sliding-window log, leaky bucket, AIMD (adaptive
  rate), WFQ (fairness across tenants).
* **Measured:** single key, uncontended, ns/op: GCRA 59, atomic GCRA 46,
  token bucket 58, leaky bucket 62, sliding counter 56, sliding log 57,
  all with 0 allocations. With 1,000 keys under contention: atomic GCRA
  31 against a global-mutex token bucket at 154. On one hot key the
  mutex is competitive and the result is noisy.
* **Default:** GCRA. Its state is one timestamp per key, which fits
  atomics and a single Redis value, and it is exact. The sliding counter
  is approximate (it can admit about 2L in an arbitrary window), and the
  sliding log is exact but uses memory per request.
* **When to pick others:** the leaky bucket for smooth pacing toward a
  target; AIMD when the upstream signals overload; WFQ for fairness
  across tenants; the sliding log as an accuracy baseline.

### Work placement (`placement`)
* **Candidates:** rendezvous (HRW) hashing, consistent hashing with
  virtual nodes.
* **Measured** (owner lookup, through the port):
  | Workers | Rendezvous | Consistent hashing | Bare ring lookup |
  |---|---|---|---|
  | 10 | 139 ns | 302 ns | 161 ns |
  | 1,000 | 7.35 µs | 2.18 µs | 613 ns |

  Removing one of 5 workers moves 223 of 1,000 keys with rendezvous and
  201 with consistent hashing.
* **Default:** rendezvous. It needs no ring state, moves only the failed
  worker's keys, and is the spec's choice in 23.3. Consistent hashing
  wins past about 100 workers or when weights are needed.

### Scope rule matching (`scope`), **retrofit**
* **Candidates:** an indexed matcher (reversed-label suffix trie for
  domains, prefix trie for CIDRs, a URL-prefix index), and a naive linear
  scan.
* **Measured,** µs per decision:
  | Rules | Indexed | Naive |
  |---|---|---|
  | 10 | 1.24 | 0.96 |
  | 1,000 | 1.34 | 75.4 |
  | 10,000 | 1.59 | 148 |
* **Default:** indexed. Its cost stays flat as rules grow, which is what
  spec b_scope_01 asks for. The naive scan is faster below about 50
  rules and is the differential-test reference.

### Graph representation (`csr`), **retrofit**
* **Candidates:** a CSR snapshot, and a hash-map adjacency list (currently
  benchmark code only).
* **Measured** at V = 100k, E = 1M:
  | Algorithm | CSR | Adjacency list | Ratio |
  |---|---|---|---|
  | BFS | 9.6 ms | 38 ms | ~4× |
  | PageRank, parallel | 12.7 ms | 962 ms | ~75× |
  | PageRank, 1 worker | 21 ms | 962 ms | ~45× |
* **Default:** CSR for analytics. Contiguous arrays and the pull-based
  transpose need no atomics. An adjacency list suits a graph that is
  mutated constantly, which is why the delta overlay exists.

### PageRank execution (`csr`)
* **Candidates:** parallel over node ranges, or sequential.
* **Default:** parallel. Its output is deterministic because each node's
  sum is computed by one worker in a fixed order.

### Vector distance kernels (`vecindex`)
* **Candidates:** a scalar reference, and an unrolled kernel with several
  accumulators.
* **Measured:** the unrolled kernel is 1.4–2.2× the scalar one depending
  on dimension.
* **Default:** unrolled. True AVX2/AVX-512 kernels are v3 C2 and join as a
  third candidate.

### Nearest-neighbour index (`vecindex`)
* **Candidates:** exact brute force, a Vamana graph (one segment), and a
  segmented index (memtable + Vamana segments + tombstones).
* **Measured:** recall@10 against L on 20k 128-d vectors with beam width 4:
  | L | 10 | 50 | 100 | 200 |
  |---|---|---|---|---|
  | recall@10 | 0.504 | 0.898 | 0.975 | 1.000 |

  Build takes 10.5 s sequential or 3.6 s with 8 workers.
* **Default:** brute force below the memtable threshold, Vamana segments
  above it. Sentinel measured HNSW and IVF-PQ for comparison; adding HNSW
  as a candidate is tracked in GAPS (G-ALG-05).

### Interpreter (`vm` against `compiler/treewalk`)
* **Candidates:** the bytecode VM, and the tree-walking evaluator.
* **Measured** (tree-walker time ÷ VM time): fib(30) 2.3, arithmetic loop
  1.1, map/filter over 1M elements 0.5 (the VM loses), string building 1.1.
* **Default:** the VM, because it is required for fuel metering,
  snapshots and replay. The tree-walker is the differential reference.
  The list-building loss is GAPS G-LANG-02 (v3 C1).

### Event store backend (`eventstore`)
* **Candidates:** MemStore, Postgres (`pgstore`), and later Raft (v3).
* **Measured** (append): MemStore about 6 µs per append and up to about
  930k events/s; Postgres with fsync about 0.45 ms per append, 125k
  events/s at 16 runs and batch size 100.
* **Default:** Postgres in services, MemStore in tests and `flowforge dev`.

### Content-defined chunking (`chunkstore`, v2), **retrofit**
* **Candidates:** FastCDC with normalized chunking (implemented). Planned:
  fixed-size chunking, Rabin fingerprint CDC, and whole-blob storage.
* **Measured:** FastCDC chunks at 1.4–1.9 GB/s. 50 versions of a 2.1 MB
  bundle are stored in 1.55 % of their raw size (dedupe 27.6×,
  compression 2.68×).
* **Default:** FastCDC. Its gear hash is cheap and normalized chunking
  tightens the chunk-size spread. The alternatives are tracked in
  G-ALG-06 to show *why* content-defined chunking beats fixed-size chunks
  on shifted content.

### Compression level (`chunkstore`, v2)
* **Candidates:** zstd level 3 on write; level 19 on GC recompression; raw
  when zstd does not save at least 1/8.
* **Default:** level 3 on the write path. Put throughput is limited by
  zstd at about 190 MB/s.

## v2 decisions (being measured)

Filled in as the v2 packages land:

* **diff:** Myers, linear-space Myers, patience, histogram, and the DP
  reference.
* **textindex:** posting codecs, intersection algorithms, BM25 against
  TF-IDF, and trigram-prefiltered regex against a full scan.
* **htmlparse** against `x/net/html`, and the strategy for looking up
  named character references.
* **jsparse:** keyword recognition, character classification, and the
  expression-parsing strategy.
* **sketch:** SimHash and MinHash against exact Jaccard similarity.
