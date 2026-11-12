# csr

Immutable Compressed Sparse Row (CSR) graph snapshots and graph analytics
for Flowforge (module `ff-csr`, layer 1, depends only on `core`).

Spec sections: **21** (layout 21.1, counting-sort build 21.2, test vector
21.3, freshness 21.4, algorithms 21.5) and the traversal operations of
**5.9**. Required tests `csr_001`..`csr_009` (25.3), the `ff-csr`
properties (25.4), `fuzz_csr_open` (25.5) and `b_csr_01`..`b_csr_03` (25.8).

## What is here

| File | Contents |
|------|----------|
| `graph.go`, `labels.go` | `Graph`, `Meta`, `Edge`, accessors, `LabelSet`, `Direction` |
| `build.go` | `Build` (dense ids in entity-id order, O(V+E) counting sort, transpose) and `Builder` |
| `format.go`, `decode.go`, `open.go`, `view.go`, `mmap_*.go` | "FFCS" encoder, validating decoder, mmap `Open`; `view.go` is the only file using `unsafe` |
| `bfs.go`, `dijkstra.go`, `pagerank.go`, `wcc.go`, `scc.go`, `kcore.go`, `cuts.go`, `triangles.go`, `blast.go` | Algorithms of 21.5 / 5.9, pure functions of a `*Graph` |
| `undirected.go`, `partition.go` | Simple undirected view, bitset, `Partition` result type |
| `overlay.go`, `holder.go`, `compaction.go` | Delta overlay, atomic snapshot swap, compaction trigger (21.4) |

Conformance data lives in `conformance/csr/`: `spec_21_3.json` is written
by hand from the spec; the other files (path, star, cycle, grid, two
cliques joined by a bridge) are generated from naive reference
implementations in `reference_test.go`, never from the code under test.

## Running

```sh
go test ./csr/...                                   # unit, conformance, property tests
go test -race ./csr/...
FF_PROPTEST_CASES=100000 go test ./csr/ -run Prop   # nightly property depth
go test ./csr/ -run '^$' -fuzz FuzzCSROpen -fuzztime 60s
go test ./csr/ -run '^$' -bench . -benchtime=3x
go test ./csr/ -run TestCSRGoldenRegenerate -csr.regen   # rewrite generated goldens
```

Indicative numbers (4 vCPU Xeon, V = 100k, E = 1M, power-law degrees):
build 73 ms (~67 ns per V+E); BFS 9.6 ms vs 38 ms on a
`map[uint32][]uint32` adjacency list (4x); PageRank to 1e-6 in 11
iterations 13 ms (21 ms on one worker) vs 960 ms on the hash map (75x, 45x
single-threaded); SCC 24 ms; WCC 7 ms; cold open of the 20 MB file with
full verification 30-45 ms.

## Known limitations

* Opening verifies the sha256, every crc32c and every structural invariant,
  so open cost is linear in file size (see OPEN_QUESTIONS.md).
* A Graph from `Open` aliases a read-only mapping; the caller must `Close`
  it once readers are done, and must not touch it afterwards. `Holder`
  never closes snapshots.
* Offsets are always stored as u64; edge confidences as f32 unless
  `FlagConfU8` is set.
* The overlay models node additions and edge additions/removals; entity
  staleness and deletions take effect at the next compaction.
* The `since` filter of `neighbors` (5.9) is not supported: snapshots do
  not carry `last_seen`.
