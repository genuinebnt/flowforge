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

### Content-defined chunking (`chunkstore`, v2)
* **Candidates** (`chunkstore.Config.Chunking`, `ChunkerConfig.Algo`,
  `ParseChunkAlgo`: `fastcdc`, `rabin`, `fixed`, `whole`; interface
  `chunkstore.Cutter`, `NewCutter`): FastCDC 2016 with normalized chunking
  (gear hash, 2/8/64 KiB); Rabin fingerprint CDC (LBFS: degree-53
  irreducible polynomial over a 48-byte rolling window, cut on all-one low
  bits, same 2/8/64 KiB bounds); fixed-size 8 KiB chunks; whole blob (one
  chunk per blob, split every 16 MiB). Version-2 manifests ("FFMF",
  `chunkstore/FORMAT.md` §1, §5) record the algorithm byte and sizes, so a
  store mixing chunkers serves every blob and re-cuts each exactly
  (CHUNK-INV-24/25). Benchmark:
  `go test -run '^$' -bench ChunkAlgo -benchtime 1x -benchmem ./chunkstore`
  (`BenchmarkChunkAlgo/<candidate>/<workload>`).
* **Measured** 2026-11-13, 4-vCPU Xeon (Go 1.26), `-benchtime 1x -count 3`,
  load average 6-7 from other builds (throughput is relative; the dedupe
  and storage ratios are deterministic). Workloads: the content
  conformance SPA fixture's 12 files; 20 versions of 1 MiB of text with 5
  in-place overwrites per version (`edit`), 5 insertions (`insert`), or
  1-64 bytes prepended (`shift`); 20 deploys of the generated 1 MiB
  webpack-like bundle (`jsbundle`). dedupe = logical bytes per new unique
  raw byte; stored = backend bytes (packs, indexes, ref) per logical byte;
  kept = share of chunks of 1 MiB of random data surviving a 1-64 byte
  insertion in its first 4 KiB.
  | Candidate | cut throughput | spa | edit | insert | shift | jsbundle | kept | mean chunk |
  |---|---|---|---|---|---|---|---|---|
  | fastcdc | 1.94-2.02 GB/s | 1.0× / 32.1 % | 10.3× / 3.75 % | 10.3× / 3.77 % | 16.8× / 2.45 % | 13.2× / 2.78 % | 99.1 % | 9.8 KB |
  | rabin | 0.52-0.53 GB/s | 1.0× / 30.4 % | 7.4× / 4.92 % | 7.7× / 4.74 % | 18.9× / 2.20 % | 12.8× / 2.82 % | 99.0 % | 10.3 KB |
  | fixed | (no hashing) | 1.0× / 33.0 % | 11.6× / 3.49 % | 1.18× / 30.7 % | 1.0× / 36.0 % | 1.56× / 21.7 % | 0 % | 8.2 KB |
  | whole | (no hashing) | 1.0× / 27.9 % | 1.0× / 31.3 % | 1.0× / 31.4 % | 1.0× / 31.2 % | 1.0× / 23.9 % | 0 % | 1 MB |

  Storing a 20-version history takes 33-45 ms with FastCDC, 62-106 ms with
  Rabin, and 110-250 ms with fixed-size or whole-blob chunks, whose time
  goes to compressing the bytes they fail to dedupe.
* **Default:** FastCDC. It is the content-defined chunker that keeps
  chunks across insertions and shifts (99 % kept, like Rabin) at about 4×
  Rabin's cutting speed, and its normalized chunking tightens the
  chunk-size spread, which is why it dedupes in-place edits and
  insertions better than Rabin (10.3× against 7.4-7.7×: Rabin's
  geometric sizes put more bytes in the long chunks an edit invalidates).
  Fixed-size chunks lose everything after the first insertion or shift
  (1.0-1.6×), which is the reason for content-defined chunking. The SPA
  fixture cannot separate the candidates: its files are 0.5-22 KB and
  share no content across versions, so nothing dedupes and the whole
  blob compresses best.
* **When to pick others:** `rabin` where a port or another system must
  share chunk boundaries with an LBFS/restic-style chunker, or on
  prepend-heavy histories (it led on `shift` here, 18.9× against 16.8×);
  `fixed` for content edited only in place (disk images, fixed-record
  files: 11.6× on `edit`, the best of the four) and as the baseline;
  `whole` for small, independent blobs (the SPA case: 27.9 %), where
  per-chunk overhead and per-chunk zstd only cost.

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

### Crawl revisit policy (`services/toolrunner`)
* **Candidates** (`toolrunner.RevisitPolicy`; `ContentConfig.Revisit`,
  `ParseRevisitPolicy`: `adaptive`, `fixed`; `FF_CRAWL_REVISIT` for the
  tool runner): the V2_DESIGN §11 item 8 policy (the interval halves when
  a visit finds a change, doubles when it finds none, starts at 24 h and
  is clamped to [1 h, 7 d]), and a fixed 24 h interval. A "change" is a
  structural or level-3 change; masked-only and reordered versions are
  not. Benchmark: `go test -run '^$' -bench ToolAlgo ./services/toolrunner`
  (`BenchmarkToolAlgo/revisit/<candidate>/change_every_<h>h`): a page with
  exponentially distributed change gaps of the given mean over 90 days,
  50 seeded histories; metrics are fetches per day and mean hours from a
  change to the visit that sees it.
* **Measured** 2026-11-13, 4-vCPU Xeon (Go 1.26):
  | Candidate | every 6 h | every 72 h | every 720 h |
  |---|---|---|---|
  | adaptive | 4.43 fetches/day, 4.5 h delay | 0.44/day, 40.5 h | 0.19/day, 80.0 h |
  | fixed | 1.00/day, 12.1 h | 1.00/day, 11.8 h | 1.00/day, 11.7 h |
* **Default:** `adaptive`. It spends fetches where changes are: on a fast
  page it detects changes almost three times sooner, and on stable pages
  it fetches two to five times less, which matters because every fetch is
  charged against the program's per-target rate (V2 §9). Its delay on slow
  pages is bounded by the 7 d cap.
* **When to pick others:** `fixed` when detection latency must not depend
  on a page's history (a program that requires daily evidence of every
  page), or to compare against.

### Named character references and the parser (`htmlparse`)
* **Candidates** (`htmlparse.TokenizeOptions.CharRefLookup` /
  `Options.CharRefLookup`, `CharRefStrategy`; `CharRefStrategies()`):
  `CharRefTrie` (default: a generated compact prefix trie, a binary search
  over each node's sorted edges), `CharRefSortedSearch` (two binary
  searches over a shrinking range of the 2231 sorted names per byte),
  `CharRefMap` (read the alphanumeric run, then probe a hash map of the
  names from the longest candidate prefix down). All three return the
  longest name that prefixes the input; HTML-INV-11 checks each against
  brute force and the html5lib tokenizer and tree suites run under each.
  The parser itself is compared with `golang.org/x/net/html` (scripting
  off), which is a comparison and test oracle only (`TestHTMLDiffOracle`;
  imported from tests), never a selectable implementation. Benchmark:
  `go test -run '^$' -bench HTMLAlgo -benchmem ./htmlparse`
  (`BenchmarkHTMLAlgo/charref/<candidate>/<input>`,
  `BenchmarkHTMLAlgo/{parse,tokenize}/{ours,xnethtml}/<size>`).
* **Measured** 2026-11-13, 4-vCPU Xeon (Go 1.26), tokenizer MB/s,
  `-benchtime 20x -count 3`, under load. `entities`: every name of the
  table plus near misses; `urls`: 1 MB of links whose query strings put
  long parameter names after `&`; `page-1MB`: the generated page.
  | Strategy | entities | urls | page-1MB |
  |---|---|---|---|
  | trie | 90-93 | 147-159 | 89-100 |
  | sorted | 54-62 | 113-130 | 90-95 |
  | map | 130-132 | 112-116 | 90-105 |

  Against x/net/html (`-benchtime 2x`): parse 38/33 MB/s (10 KB),
  44/40 (1 MB), 44/44 (10 MB), with 3.7-3.8× fewer allocations and 1.6-1.7×
  the bytes (wide arena nodes); tokenize 86/54, 104/63, 107/66 MB/s.
* **Default:** the trie. Its cost per `&` is bounded by the bytes that
  match a name prefix, so the `&` of a query string (the commonest `&` on
  real pages) is rejected after a byte or two: 1.3× the map on `urls`,
  where the map hashes every prefix length of each parameter name.
  Real pages tie.
* **When to pick others:** `map` for entity-dense text (generated
  documentation, escaped code listings: 1.4× the trie), at the cost of a
  2231-entry map built at start-up; `sorted` where table size matters (no
  table beyond the names; a port with tight memory) or as the reference.

### Lexer and expression parsing (`jsparse`)
* **Candidates** (`jsparse.Options.Algorithms`, `jsparse.Algorithms`):
  keyword recognition `Keywords`: `KeywordSwitch` (default, a
  length-guarded string switch), `KeywordMap` (hash map),
  `KeywordPerfectHash` (first two and last two bytes and the length packed
  into a uint64, multiplied, top 7 bits: one probe of a 128-slot table,
  one string comparison); ASCII identifier classification `ASCIIClass`:
  `ASCIITable` (default, 128-entry class table), `ASCIIBranches` (range
  comparisons); non-ASCII classification `IdentClass`:
  `IdentUnicodeTables` (default, Go's range tables), `IdentBMPBitmap`
  (2 × 8 KiB BMP bitmaps); binary expressions `Binary`: `BinaryClimbing`
  (default, precedence climbing over a table), `BinaryCascade` (one
  function per precedence level). Pratt parsing is not a separate
  candidate: for binary operators it is precedence climbing (the same
  loop over binding powers); the cascade is the real alternative.
  `TestJSAlgoEquivalent` checks identical trees and diagnostics, and the
  test262 parser suite (pass, explicit, fail, early) runs under every
  candidate with the same allowed deviations. Benchmark:
  `go test -run '^$' -bench JSAlgo -benchtime 10x -count 5 ./jsparse`
  (`BenchmarkJSAlgo/<decision>/<candidate>/<input>`; `keyword-lex` and
  `asciiclass-lex` replay the parser's token stream through the lexer
  alone).
* **Measured** 2026-11-13, 4-vCPU Xeon (Go 1.26), MB/s, median (range) of
  five runs for the lexer-only rows and three for the parse rows, load
  average 6-7 from other builds:
  | Decision | Candidate | Input | MB/s |
  |---|---|---|---|
  | keyword (lexer only) | switch | bundle 1 MB | 69.2 (66.9-71.3) |
  | | perfect hash | | 67.5 (66.2-69.2) |
  | | map | | 65.5 (63.5-66.9) |
  | ASCII class (lexer only) | table | bundle 1 MB | 65.2 (62.0-67.8) |
  | | branches | | 68.6 (63.9-69.1) |
  | non-ASCII class (parse) | unicode tables | non-ASCII 1 MB | 39.9 (36.4-40.4) |
  | | BMP bitmap | | 46.9 (37.9-48.0) |
  | binary expressions (parse) | climbing | bundle 1 MB | 21.9 (21.6-25.0) |
  | | cascade | | 20.0 (19.3-21.6) |

  Full parses of the 1 MB bundle drift between 19 and 26 MB/s for the same
  candidate across runs on this machine, so only the lexer-only rows
  resolve differences of a few percent.
* **Default and why:** the keyword switch: the three are within their
  spread (a lookup is a few percent of lexing), and the switch needs no
  table or initialization and maps 1:1 to a Rust `match` on `&str`. The
  ASCII table: branches measured up to 5 % faster in the lexer-only runs,
  inside the spread of the runs; the table is one load per byte with no
  data-dependent branch, and stays until a quiet machine shows a
  difference. Unicode tables: the bitmap wins only on input made almost
  entirely of non-ASCII identifiers, and the tables cover the whole code
  space without a 16 KiB table built on first use (the Rust
  `unicode-id-start` approach). Precedence climbing: faster than the
  cascade (twelve calls per operand) and what V2_DESIGN §4.2 specifies.
* **When to pick others:** `KeywordPerfectHash` where a switch compiles
  to a linear scan (a port without string-switch optimization), the map
  as the obvious reference; `ASCIIBranches` where a table load is costly;
  `IdentBMPBitmap` for scripts in non-Latin alphabets; `BinaryCascade` as
  the readable reference that mirrors the grammar's productions.

### Event bus adapters (`services/internal/bus`)
* **Candidates** (`bus.Bus`; `busconfig.Open`, `FF_BUS`: `mem`,
  `postgres`, `pubsub`): `bus.Mem` (in process), `bus.PgBus` (tables plus
  LISTEN/NOTIFY, migration 0016), `pubsubbus.Bus` (Google Pub/Sub; the
  `pstest` fake in the benchmark). `bustest.Run` checks every adapter
  against the same contract (at least once, redelivery, dead-lettering,
  ordering per key, inbox idempotence). Benchmark:
  `go test -run '^$' -bench BusAlgo -benchtime 2s ./services/internal/bus/`
  (`BenchmarkBusAlgo/<adapter>/<latency|throughput>`; `postgres` needs
  `FF_TEST_PG_DSN`). Latency is one publish then waiting for its delivery;
  throughput publishes batches of 100 on 10 ordering keys.
* **Measured** 2026-11-13, 4-vCPU Xeon (Go 1.26), Postgres 16 on the same
  host:
  | Adapter | latency (round trip) | throughput |
  |---|---|---|
  | mem | 2.3-2.5 µs | 92,000-94,000 msgs/s (bounded by the benchmark's 50 µs poll) |
  | postgres, fsync on | 1.2 ms | ~1,500 msgs/s |
  | postgres, fsync off | 0.53 ms | ~3,200 msgs/s |
  | pubsub-pstest | 99 ms | ~90 msgs/s |
* **Default:** by profile, not by speed: `mem` for one process
  (`flowforge dev`, tests: no I/O, but nothing survives a restart and no
  other process sees the messages); `postgres` when a database is
  configured and no Pub/Sub project is (durable, ordered, no extra
  service; one transaction per publish and per ack dominates); `pubsub`
  on GCP. `pstest` runs its delivery loop on a ~100 ms cycle, so its row
  measures the client library, not the Pub/Sub service.
* **When to pick others:** Pub/Sub beyond a few thousand messages per
  second or across regions; Postgres for local multi-process runs and
  small single-VM deployments. Details: `services/internal/bus/README.md`.

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
