# chunkstore

Content-addressed, deduplicating, compressed blob store for Flowforge v2
(V2_DESIGN §6.1, §6.3). Layer 1: imports the standard library, `core`
and `github.com/klauspost/compress/zstd` only. `chunkstore/fsstore` is
the directory adapter, `chunkstore/gcsstore` the Cloud Storage adapter
(its own README is its package comment). Byte formats: [FORMAT.md](FORMAT.md) (normative,
the Rust port's contract). Open issues: [OPEN_QUESTIONS.md](OPEN_QUESTIONS.md).

## Design in one paragraph

A chunker (`Config.Chunking`, a pluggable decision: FastCDC by default,
Rabin fingerprint CDC, fixed-size or whole-blob; see
[docs/ALGORITHMS.md, "Content-defined chunking"](../docs/ALGORITHMS.md))
splits a blob into chunks named by the SHA-256 of their bytes; FastCDC
uses normalized chunking, 2 KiB / 8 KiB / 64 KiB, gear table from
`core.SplitMix64(0x46464344)`. New chunks are zstd-compressed (level 3; raw when
zstd saves less than 1/8) and appended to an open pack, sealed at
`TargetPackSize` (16 MiB) or at the end of each Put; sealing writes the
pack ("FFPK"), its fan-out index ("FFPI") and then the ref ("FFRF") that
names the live packs — the single commit point. The blob's manifest
("FFMF": total length + ordered ids) is stored as a chunk, so its id is
a content id; it records the chunker that cut it (version 2), so one
store holds blobs of any mix of chunkers and each blob can be re-cut
exactly. A Store is scoped to one tenant namespace
(`t/<ns>/...`): dedupe, presence, stats, GC and fsck never cross it.

## API

```go
s, _ := chunkstore.Open(ctx, backend, "tenant-42", chunkstore.Config{}) // namespace is mandatory
id, st, _ := s.Put(ctx, r)       // Stats{Raw, Stored, NewChunks, DedupChunks, NewRaw, NewManifest}
rc, _ := s.Get(ctx, id)          // every chunk verified before its bytes are returned
info, _ := s.Stat(ctx, id)       // size, chunk counts, stored bytes
rep, _ := s.GC(ctx, live, pinned, chunkstore.GCOptions{LiveThresholdPercent: 50, RecompressLevel: 19})
fr, _ := s.Fsck(ctx, ids)        // or chunkstore.Fsck(ctx, backend, "tenant-42", ids) offline
m := s.Metrics()                 // m.DedupeRatio(), m.CompressionRatio(), m.StoredRatio()
_ = s.Flush(ctx); _ = s.Close(ctx)
```

* **Port:** `Backend` (`Put`, `Get`, `GetRange`, `List`, `Delete` of
  named objects; atomic Put, idempotent Delete). Implementations:
  `MemBackend`, `fsstore.Dir` (temp file + fsync + rename + directory
  fsync; `NoSync` for tests), `gcsstore.Backend` (one object per name,
  generation-conditional writes and deletes so a second writer fails with
  `ErrConflict`; tests against the in-process JSON API fake
  `gcsstore/gcsfake`). `chunkstore/backendtest` is the contract suite every
  implementation runs (`Contract`, and `Store` for a Store's whole life
  over reopened instances).
* **Chunkers:** `Cutter` (`Cut`, `Config`), `NewCutter(ChunkerConfig{Algo:
  ChunkFastCDC | ChunkRabin | ChunkFixed | ChunkWhole, ...})`,
  `ParseChunkAlgo`, `Boundaries(c, data)`, streaming `NewChunker(c, r)`;
  `FastCDC` keeps its own `Cut`/`Boundaries`/`NewChunker`.
  `ManifestInfo.Chunking` and `Manifest.Chunking` name a blob's chunker.
* **Concrete pieces:** `PackWriter`/`DecodePack`, `NewIndex`/`Index.Encode`/
  `DecodeIndex`/`Index.Lookup`, `Manifest.Encode`/`DecodeManifest`,
  `Ref.Encode`/`DecodeRef`.
* **Delta frames:** `EncodeDelta(raw, dict)`, `DecodeDelta`, `IsDelta`
  ("FFZB", `delta.go`): one zstd frame against a raw dictionary (the
  previous version), stored by callers as ordinary blobs; the content
  store's delta codec. Test: `TestChunkDeltaFrames`.
* **Errors:** `*Error{Code, Object, Detail}` with a closed list of codes
  (`errors.go`); `Code.Kind()` maps to `core.Kind`. Fsck reports
  `Problem{Code, Severity, Object, Detail}` sorted deterministically.
* **Concurrency:** one `sync.RWMutex` per Store. Chunking, hashing,
  compression and decompression run outside it; Put takes it briefly per
  new chunk and while sealing; Get holds the read lock per record fetch;
  GC and Fsck hold the write lock throughout. GC grace: chunks of
  running Puts are never collected, and a blob survives the first GC
  that starts after its Put returned; later GCs must name it (`live` or
  `pinned`). One Store process per namespace (single writer).
* **GC:** mark live ∪ pinned ∪ grace; rewrite packs with live bytes
  below the threshold (copying live records, recompressed at level 19 by
  the 1/8 rule), drop fully dead packs; write new packs and indexes,
  swap the ref, then delete replaced packs and orphans.

## Invariants (port these)

| Id | Test | Statement |
|----|------|-----------|
| CHUNK-INV-01 | `TestChunkInv01_RoundTrip` | Get(Put(x)) == x for sizes 0, 1, every min/avg/max boundary ±1 and random sizes, in several configurations and under every chunker; Stat reports the size and the recorded chunker. |
| CHUNK-INV-02 | `TestChunkInv02_BoundariesArePure` | Cut points are a pure function of the bytes since the previous cut: re-chunking from any cut reproduces the remaining cuts (every chunker). |
| CHUNK-INV-03 | `TestChunkInv03_ShiftResistance` | For the content-defined chunkers (FastCDC, Rabin): inserting 1–64 bytes in the first 4 KiB of 1 MiB of random data preserves ≥ 90 % of the chunks (and changes at most a constant number). |
| CHUNK-INV-04 | `TestChunkInv04_ChunkLengthBounds` | Every chunk is in [min, max] except the last, which is in [1, max] (whole blob: every chunk but the last is exactly max); cuts increase strictly and end at the input length (every chunker). |
| CHUNK-INV-05 | `TestChunkInv05_StreamingEqualsSlice` | The streaming chunker yields the slice API's boundaries for every read size (1 B … 1 MiB, one-byte, half, data+EOF, random) and every chunker, including a 16 MiB whole-blob chunker past its initial buffer. |
| CHUNK-INV-06 | `TestChunkInv06_Dedupe` | Storing the same content twice adds zero chunks and zero bytes and writes nothing; repeats inside one input are stored once. |
| CHUNK-INV-07 | `TestChunkInv07_StoreCorruptionDetected` | Flipping any byte of any pack, index or ref is reported by fsck as that object's `*_BAD_MAGIC` (bytes 0–3) or `*_CHECKSUM` code; Open fails with it for indexes and refs; Get never returns wrong bytes. |
| CHUNK-INV-08 | `TestChunkInv08_DecoderCorruptionCodes` | Each decoder maps any single flipped byte to `*_BAD_MAGIC`/`*_CHECKSUM` and any truncation to a coded error; a flipped record byte gives `RECORD_MALFORMED` (lengths) or `RECORD_CHECKSUM`. |
| CHUNK-INV-09 | `TestChunkInv09_GCSafetyAndReclaim` | Over random put/drop/pin/GC/reopen sequences, GC never removes a chunk reachable from a live, pinned or grace manifest; fsck is clean; no pack stays below the live threshold; threshold 100 leaves nothing unreachable. |
| CHUNK-INV-10 | `TestChunkInv10_GCCrashSafety` | A crash injected at every mutating backend call of GC (call fails, or succeeds with the ack lost) leaves a consistent, complete store; the next GC finishes the job with a problem-free fsck. |
| CHUNK-INV-11 | `TestChunkInv11_Determinism` | Same inputs, order and clock → byte-identical packs, indexes, manifests and ref; manifest ids do not depend on packing; index bytes do not depend on entry order. |
| CHUNK-INV-12 | `TestChunkInv12_ZstdDeterminismAndFormatIndependence` | The pinned encoder is byte-deterministic (instances, repeats, concurrency 1 vs 8); records written by other encoders/levels read back with the same ids and a clean fsck. |
| CHUNK-INV-13 | `TestChunkInv13_ConformanceBoundaries` | The gear table and the cut points/chunk hashes of the seeded inputs equal `gear.json` and `boundaries.json`; the default masks are the paper's; the Rabin tables and the Rabin, fixed-size and whole-blob cut points equal `chunkers.json`. |
| CHUNK-INV-14 | `TestChunkInv14_GoldenFormats` | The golden pack, index, manifests (version 1 and 2) and ref decode to their documented fields, and encoding those fields reproduces the bytes. |
| CHUNK-INV-15 | `TestChunkInv15_PutCrashSafety` | A crash at any mutating call of a Put leaves an fsck-clean store with every earlier blob intact; the interrupted blob is complete or absent. |
| CHUNK-INV-16 | `TestChunkInv16_Concurrency` | Concurrent Put/Get/GC are race-free and lose no blob whose Put completed. |
| CHUNK-INV-17 | `TestChunkInv17_TenantIsolation` | Two namespaces with identical content each pay for their own chunks; presence, Get, Stat, Stats, Metrics, GC and Fsck of one never reflect the other; namespaces are mandatory and validated. |
| CHUNK-INV-18 | `TestChunkInv18_Pinning` | Pinned manifests survive every GC regardless of the live set; an unknown pinned id aborts GC with nothing changed. |
| CHUNK-INV-19 | `TestChunkInv19_IndexLookupMatchesScan` | Fan-out + binary-search lookup equals a linear scan for present and absent ids. |
| CHUNK-INV-20 | `TestChunkInv20_ReopenPersistence` | A reopened store serves the same blobs and dedupes against them. |
| CHUNK-INV-21 | `TestChunkInv21_GCGrace` | A blob put before a GC survives that GC unnamed and is collected by the next GC that does not name it. |
| CHUNK-INV-22 | `TestChunkInv22_RecompressionRule` | GC recompression keeps ids and content, stores zstd only when `8*stored_len <= 7*raw_len` and never grows a record. |
| CHUNK-INV-23 | `TestChunkInv23_MetricsConsistency` | Metrics equal the sums of Put stats and the backend's object sizes; ratios follow from the counters. |
| CHUNK-INV-24 | `TestChunkInv24_MixedChunkers` | One namespace written by every chunker in turn serves every blob; each manifest records its chunker, and re-cutting the blob with it reproduces the chunk list; the same content dedupes under one chunker and is stored apart under another; fsck is clean and flags lengths the recorded chunker cannot produce (`MANIFEST_CHUNKING_MISMATCH`); GC keeps every named blob. |
| CHUNK-INV-25 | `TestChunkInv25_ManifestChunkerDescriptor` | Every canonical chunker encodes as manifest version 2 and decodes to itself; an unset chunker is version 1 (the bytes written before version 2); unknown algorithms are `MANIFEST_UNSUPPORTED`, bad reserved bytes or descriptors `MANIFEST_MALFORMED`; the descriptor is part of the manifest id. |

Also: `TestChunkRabinPolynomialIrreducible` (Ben-Or's test on the Rabin
polynomial; rolling update equals direct fingerprinting),
`TestChunkerAlgoConfigValidation`, `TestChunkAcceptance_FiftyVersions` (V2_DESIGN acceptance),
`fsstore` and `gcsstore` contract/lifecycle tests (`backendtest`), and fuzz targets `FuzzChunker`
(INV-02/04/05 as assertions, every chunker), `FuzzPackDecode`, `FuzzIndexDecode`,
`FuzzManifestDecode`, `FuzzRefDecode` (coded errors only; accepted input
re-encodes to itself; each target also decodes a CRC-repaired variant so
the structural checks behind the checksum are fuzzed).

## Conformance data (`conformance/chunkstore/`)

`gear.json` (256 u64), `boundaries.json` (19 cases: SplitMix64 bytes,
SplitMix64 ASCII and zeros, default and three other parameter sets, with
cut offsets and per-chunk SHA-256; generators documented in the file),
`chunkers.json` (Rabin polynomial and tables; Rabin, fixed-size and
whole-blob cut points on seeded inputs, with a hash over the chunk ids),
`golden_pack.json`, `golden_index.json`, `golden_manifest.json` (version 1),
`golden_manifest_v2.json`, `golden_ref.json` (hex plus decoded fields), `zstd.json` (pinned
settings; informative Go frame hash). Regenerate only for a deliberate
format change: `go test ./chunkstore -run Conformance|Golden -update`.

## Tests and benchmarks

```sh
go test -race ./chunkstore/...                      # ~45 s with -race
go test -short ./chunkstore/...                     # skips the 100 MB acceptance run
go test ./chunkstore -run '^$' -fuzz FuzzPackDecode -fuzztime 20s
go test ./chunkstore -run '^$' -bench . -benchtime 1x
```

Reference numbers (4-vCPU Xeon, Go 1.26, one run):

| Benchmark | Result |
|---|---|
| `BenchmarkChunk` (64 MiB random) | 1.4–1.9 GB/s |
| `BenchmarkPut/1MB`, `/100MB` (fresh text, no dedupe) | ~100 MB/s (zstd-bound: the single pinned encoder does ~190 MB/s on this input) |
| `BenchmarkGet/1MB`, `/100MB` (full verification) | 260–310 MB/s |
| `BenchmarkAcceptance50Versions` (50 × 2.1 MB JS-like bundle, small edits, a rename every 10th version) | stored/raw **1.55 %** (target < 15 %), dedupe 27.6×, compression 2.68× |
| `BenchmarkChunkAlgo/<fastcdc\|rabin\|fixed\|whole>/<workload>` (chunker comparison: cut throughput, dedupe and storage on the SPA fixture and edit/insert/shift/bundle histories, shift resistance) | FastCDC 1.9–2.0 GB/s, Rabin 0.5 GB/s; insert histories dedupe 10.3× (FastCDC), 7.7× (Rabin), 1.2× (fixed), 1.0× (whole); full table in [docs/ALGORITHMS.md](../docs/ALGORITHMS.md) |

Known limitations are in OPEN_QUESTIONS.md.
