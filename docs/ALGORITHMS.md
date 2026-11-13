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

## v2 decisions

### Text diff algorithm (`diff`)
* **Candidates** (`Options.Algorithm`): `MyersLinearSpace` (default),
  `Myers` (greedy, O(ND) memory), `Patience`, `Histogram` (git's default),
  and `Reference` (dynamic-programming LCS, quadratic, for tiny inputs and
  as a test oracle). Benchmark: `BenchmarkDiffAlgo`.
* **Measured,** 1M tokens with 1 % edits:
  | Algorithm | Time | Memory |
  |---|---|---|
  | MyersLinearSpace | 2.3 s | 37 MB |
  | Patience | 144 ms | 163 MB |
  | Histogram | 163 ms | 46 MB |

  MyersLinearSpace allocates about 19 B per token. The default edit cap
  (10,000) turns a 200k-token diff with 10 % edits from 6.0 s into 162 ms by
  falling back.
* **Default:** MyersLinearSpace. It is the only candidate that guarantees a
  *minimal* edit script, which matters for invariants and for small,
  exact diffs, and its memory is linear.
* **Recommendation for the content pipeline:** use **Histogram** for large
  normalized snapshots. It is about 14× faster here with modest memory,
  matches git's output byte for byte, and tends to produce more readable
  hunks around moved blocks. Myers stays the default of the library
  itself. Patience is fastest but uses 4× the memory.
* **git compatibility:** with `GitCompact`, Histogram reproduces git 2.43
  everywhere; Patience does too, except where it falls back to Myers;
  Myers matches only where git picks the same alignment (git's tie-breaking
  is not reproduced). Recorded in `conformance/diff/unified_git.json`
  (320 cases).

### Near-duplicate sketches (`sketch`)
* **Candidates:** exact Jaccard, MinHash with LSH banding, SimHash
  (64-bit, weighted variant). Benchmark: `BenchmarkSketchAlgo`.
* **Measured:** comparing stored sketches takes about 35 ns for MinHash and
  under 1 ns for SimHash. Exact Jaccard costs time proportional to the set
  size.
* **Default:** SimHash for chunk lineage (cheap and fixed-size); MinHash
  when a Jaccard estimate with a known error bound is needed; exact for
  small sets and as the reference.
* **Known limit:** the SimHash multi-index lookup is exact only up to
  Hamming distance 3, and bundle lineage needs 6 (`sketch/OPEN_QUESTIONS.md`).

### Text search (`textindex`)
* **Engines:** the segmented `Index` (default) and `ScanIndex`
  (brute force, identical semantics; for tiny corpora and as the oracle).
  Benchmark: `BenchmarkTIdxAlgo/<decision>/<candidate>`.
* **Measured** (100k synthetic diff hunks, on a loaded machine):
  | Decision | Candidates and results | Default |
  |---|---|---|
  | Posting codec | plain 4.8, delta+varint 1.35, group-varint 1.65 bytes/posting | delta+varint (smallest; decode speed close to group-varint here) |
  | List intersection | skip pointers about 14× faster than galloping at a 1:1,000 size ratio | skip pointers |
  | Ranking | BM25 and TF-IDF tie at MRR 0.947 on the labelled set | BM25 (length normalization; a larger labelled set may separate them) |
  | Regex | trigram prefilter about 4× faster than a full scan | trigram prefilter |
* **Throughput:** 11–13k docs/s indexing, 770 B per doc, and an index 2.2×
  the raw text (trigrams dominate). p50 query times: term 1.4–1.7 ms,
  phrase 6–11 ms, filtered 2.4–3 ms, regex 110–150 ms.

### Content pipeline decisions (`content`)

Four decision points, all selectable in non-test code. Benchmark:
`go test -run '^$' -bench ContentAlgo -benchmem ./content`
(`BenchmarkContentAlgo/<decision>/<candidate>`). Measured 2026-11-13 on a
4-vCPU Xeon (Go 1.26), `-benchtime=3x`, with other work running, so the
numbers are relative.

| Decision (config) | Candidate | Measured | Default and why |
|---|---|---|---|
| lineage matcher (`ProcessOptions.Lineage`, `LineageOptions.Algo`; `simhash`/`minhash`/`jaccard`) | `simhash` | 13.2–13.3 ms to fingerprint a 200 KB bundle and scan 20 candidates; 8 B stored per version | **default** (V2 §5): fixed 8-byte sketch per stored version, Hamming ≤ 6 via a linear scan (sketch's multi-index is exact only to 3) |
| | `minhash` | 21–22 ms; 1 KiB stored | when the Jaccard value itself matters (estimate with a known error) |
| | `jaccard` | 5.7–6.0 ms; 8 B × shingles stored | exact reference; cheapest when both token streams are at hand, but every old version must keep its shingle set |
| module splitter (`SplitOptions.Strategy`; `auto`/`webpack`/`esbuild`/`toplevel`) | `auto` | 38–41 MB/s on a 2 MB webpack bundle | **default**: webpack tables + esbuild wrappers + rollup/vite top-level statements, so every bundler shape gets module identity |
| | `webpack` | 40–47 MB/s | webpack-only sites (the rest is one runtime unit) |
| | `esbuild` | 33 MB/s | esbuild-only sites |
| | `toplevel` | 36–40 MB/s | scope-hoisted (rollup/vite) output; no table detection |
| volatility detector (`VolatilityPolicy.Algo`; `ratio`/`streak`) | `ratio` | 1.6–1.7 ms per 10,000 counters | **default**: robust to an occasional stable observation (≥ 8 observations, ≥ 90 % changed) |
| | `streak` | 1.5 ms | learns faster (5 consecutive changes) but one stable observation resets it |
| diff policy (`DiffOptions.Policy`; `size`/`myers`/`histogram`/`patience`) | `size` | 34–44 MB/s on a 512 KB normalized bundle, 1 % edited | **default**: MyersLinearSpace tokens up to 16 KiB (minimal scripts on small artifacts), Histogram lines above (docs/ALGORITHMS.md: Histogram for large normalized snapshots); a reordered 20 KB bundle took ≈ 0.2 s per diff at token level against the 10,000-edit cap |
| | `myers` | 5–7 MB/s | minimal scripts up to the §5 4 MiB cap |
| | `histogram` | 26–42 MB/s | lines always; git-compatible hunks |
| | `patience` | 30 MB/s | lines always; more memory |

All policies go through `diff.BoundedDiff`, so the §5 bounds (edit cap,
4 MiB token / 64 MiB line caps, hash-only fallback, recorded level and
reason) hold for every candidate.

Conformance and invariant tests (CONTENT-INV-01..16) run against every
candidate. See `content/README.md` for throughput per content kind.

### Content store blob codec (`contentstore`)
* **Candidates** (`contentstore.Config.Codec`, `ParseBlobCodec`:
  `delta`, `chunk`; service `FF_CONTENT_CODEC`): delta frames (each blob
  one zstd frame against its delta base's blob as raw dictionary,
  chunkstore `EncodeDelta`, keyframes every 16 versions) or plain blobs
  (chunkstore FastCDC dedupe and per-chunk zstd level 3). Benchmark:
  `BenchmarkCStoreAlgo/codec/<candidate>/<versions50|spa>`.
* **Measured** (2026-11-13, 3 runs): 50 versions of a ~300 KB bundle with
  small edits: delta stores 4.70 % of raw at 180 ms/version, chunk 9.87 %
  at 159 ms/version. SPA fixture, everything stored: delta 59.8 %, chunk
  95.1 %; raw snapshots only (CSTORE-INV-08): delta 21.4 %, chunk 32.0 %.
  zstd level: SpeedBetterCompression within 2 % of SpeedBestCompression
  at about a quarter of the time and memory, so it is pinned.
* **Default:** `delta`: the only candidate that meets V2 §10.7's 25 % on
  small sites, and half the bytes on long histories.
* **When to pick `chunk`:** CPU-bound ingest of large bundles where
  storage is cheap, or when versions share long unchanged runs and
  chains are undesirable (no keyframe rewrites on retention).

### Content store indexing (`contentstore`)
* **Candidates** (`contentstore.Indexes.Mode`, `ParseIndexMode`:
  `inline`, `batched`; `FF_CONTENT_INDEX_MODE`): index each ingest's diff
  before Ingest returns, or let a loop index pending diffs above the
  watermark. Benchmark: `BenchmarkCStoreAlgo/index/<mode>/spa`.
* **Measured:** 231 ms (inline) vs 248 ms (batched, one pass at the end)
  for the SPA fixture: equal within noise; textindex's size-tiered merge
  absorbs the per-ingest segments.
* **Default:** `inline`: changes are searchable when Ingest returns, at
  no measured cost. `batched` fits bursty bulk imports (fewer, larger
  segments) at the price of search lag.

## v2 decisions (being measured)

* **htmlparse** against `x/net/html`, and the lookup strategy for named
  character references.
* **jsparse:** keyword recognition, character classification, and the
  expression-parsing strategy.
* **Event bus adapters** (`feat_v1`): in-memory, Postgres and Pub/Sub
  (pstest), compared on latency and throughput.
* **chunkstore** alternatives (fixed-size chunking, Rabin, whole-blob),
  tracked as G-ALG-06.

### Base64 decoding (`codec`)
* **Candidates** (`codec.Base64Options.Algo`, `codec.ParseBase64Algo`:
  `statemachine`, `swar`, `stdlib`): the RFC 4648 §4 state machine, byte by
  byte (the reference); a table-driven word-at-a-time decoder (four
  256-entry tables give each character's sextet pre-shifted, with an
  "invalid" bit above bit 23, so eight characters decode with eight
  lookups, one test and one 8-byte store); and `encoding/base64` on inputs
  it can take unchanged. The fast candidates hand every quantum that is not
  four plain alphabet characters (white space, padding, the end, an error)
  to the state machine, so all three return the same bytes and the same
  error code and offset (CODEC-INV-06). Benchmark:
  `go test -run '^$' -bench CodecAlgo -benchmem ./codec`
  (`BenchmarkCodecAlgo/<candidate>/<size>[-mime]`).
* **Measured** 2026-11-13, 4-vCPU Xeon (Go 1.26), two runs, decoded MB/s;
  strict canonical input, and lenient MIME input wrapped at 76 characters:
  | Candidate | 64 B | 4 KiB | 1 MiB | 1 MiB MIME |
  |---|---|---|---|---|
  | swar | 1,303-1,306 | 1,804-1,958 | 1,853-2,035 | 963-1,066 |
  | statemachine | 595-598 | 753-760 | 579-753 | 480-493 |
  | stdlib | 562-639 | 817-864 | 749-836 | 491-494 |
* **Default:** `swar`. It is 2.2-2.5× the stdlib on canonical input and 2×
  on MIME input (the stdlib candidate falls back to the state machine for
  white space and for the lenient mode's mixed alphabets), and it is our
  own portable code with the state machine's semantics, so the Rust port
  reproduces it directly.
* **When to pick others:** `statemachine` as the reference and when
  reading the code; `stdlib` to compare against Go's decoder (it gives no
  benefit otherwise).
