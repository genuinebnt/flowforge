# sketch

Similarity sketches for Flowforge, shared by `content` (bundle lineage,
V2_DESIGN §5: SimHash Hamming distance over normalized token shingles) and
`vecindex` (near-duplicate detection, spec 22.8). Layer 1: standard library
and `core` only. Pure and deterministic; every seed and formula is part of
the conformance contract.

## API

| Identifier | What it is |
|---|---|
| `FNV1a64`, `FNV1a64Bytes`, `TokenHash`, `TokensHash` | 64-bit FNV-1a over UTF-8 bytes, or over 4-byte little-endian token ids |
| `Tokens`, `Shingles`, `CharNGrams` | Text tokens (lower-cased letter/digit runs), space-joined w-shingles, rune n-grams |
| `TokenShingles`, `WShingling`, `JaccardSorted` | k-shingle hashes of a token-id sequence (multiset, in order), Broder's w-shingling (sorted set), exact Jaccard of sorted sets |
| `SimHash`, `SimHashHashes`, `SimHashText`, `SimHashTokens`, `Hamming` | 64-bit SimHash (+1/-1 votes, strictly positive bits) over strings, hashes, text or token shingles |
| `WeightedSimHash`, `WeightedSimHashTokens`, `CountTokens` | Weighted SimHash over (hash or token, int32 weight); term-frequency weights |
| `SimHashIndex[ID]`, `Match[ID]` | Exact Hamming ≤ 3 lookup by multi-index hashing (four 16-bit blocks) |
| `MinHasher`, `DefaultMinHasher`, `Signature`, `SignatureHashes`, `EstimateJaccard`, `Jaccard` | 128 permutations `(a·x + b) mod 2^61−1`, coefficients from `SplitMix64(0x46464d494e484153)` |
| `LSH[ID]`, `BandKey`, `CollisionProbability` | 32 bands × 4 rows; band key FNV-1a over the rows' LE bytes; `1−(1−s^r)^b` |
| `Method`, `EstimateSimilarity`, `SimilarityOptions` | Pluggable Jaccard estimate of two token sequences: `exact`, `minhash`, `simhash` |

Indexes are generic over the id type with a caller-supplied total order
(`vecindex` uses `core.UUID`, bytewise).

## Choosing a method

`EstimateSimilarity` selects one of three estimators of the same quantity,
the Jaccard similarity of the two w-shingle sets (config key
`SimilarityOptions.Method`, names `exact` / `minhash` / `simhash`; benchmark
`go test -run '^$' -bench SketchAlgo ./sketch`). Measured 2026-11-13 on a
4-vCPU Intel Xeon (Go 1.26), document pairs with 1 % of tokens replaced:

| Method | Pair from raw tokens, 10k tokens | Compare two stored sketches | Stored size per doc | abs. error (1k / 10k / 100k) |
|---|---|---|---|---|
| `exact` | 2.3 ms (8.6 M tokens/s) | 18 µs (10k), 130 µs (100k) | 8 B × shingles | 0 |
| `minhash` | 11.3 ms (1.8 M tokens/s) | 34–50 ns | 1 KiB | 0.036 / 0.001 / 0.014 |
| `simhash` | 4.8 ms (4.2 M tokens/s) | < 1 ns (popcount) | 8 B | 0.028 / 0.046 / 0.019 |

Index lookups among 100,000 stored documents: `SimHashIndex.Near` 1.3 µs,
`LSH.Candidates` 1.8 µs.

* **Default `exact`**: when both sequences are in memory the exact value is
  cheapest and has no error.
* **`minhash`** when one side is stored or indexed and the similarity value
  matters (unbiased, standard error √(J(1−J)/128)); LSH banding finds
  candidates above a Jaccard threshold.
* **`simhash`** when storage and lookup cost dominate: 8 bytes, Hamming
  lookups, but the coarsest estimate (±~3 bits of 64) and its Jaccard
  conversion assumes equally sized sets. This is what bundle lineage uses.

(Entry text for `docs/ALGORITHMS.md`; that file is outside this package's
write scope in the v2 build and is linked from here once it exists.)

## Invariants (port these)

| Id | Statement | Test |
|---|---|---|
| SKETCH-INV-01 | Outputs equal the pre-refactor `vecindex` outputs (`conformance/sketch/vecindex_compat.json`, `conformance/vecindex/{simhash,minhash}`) | `TestSketchInv01_VecindexCompat`, `TestSketchConformance*` |
| SKETCH-INV-02 | SimHash Hamming distance tracks shingle Jaccard: per bucket the mean distance is within 2.5 bits of Charikar's 64·θ/π and strictly increases as Jaccard falls | `TestSketchInv02_SimHashTracksJaccard` |
| SKETCH-INV-03 | MinHash estimate within 5 standard errors of exact Jaccard for every pair; mean error < 0.01 | `TestSketchInv03_MinHashEstimate` |
| SKETCH-INV-04 | LSH collision rate matches 1−(1−s^r)^b within 5 binomial standard errors (≥ 0.05) | `TestSketchInv04_LSHCollisionRate` |
| SKETCH-INV-05 | Weighted SimHash with unit weights = SimHash; term-frequency weights = multiset SimHash; positive scaling is invariant | `TestSketchInv05_WeightedConsistency` |
| SKETCH-INV-06 | WShingling is exactly the set of w-windows (size and Jaccard); TokenShingles has max(n−k+1, min(n,1)) elements | `TestSketchInv06_ShinglingExact` |
| SKETCH-INV-07 | `SimHashIndex.Near` = brute-force scan for distance ≤ 3 after any adds/removes | `TestSketchInv07_SimHashIndexExact` |
| SKETCH-INV-08 | `LSH.Candidates` = brute-force "agrees on a whole band" | `TestSketchInv08_LSHCandidatesExact` |
| SKETCH-INV-09 | SimHash ignores order; MinHash ignores order and duplicates; `SignatureHashes(FNV1a64(e))` = `Signature(e)` | `TestSketchInv09_OrderIndependence` |
| SKETCH-INV-10 | `MulAddMod61` = schoolbook 128-bit reference | `TestSketchInv10_MulAddMod61` |
| SKETCH-INV-11 | The three `Method`s estimate the same Jaccard: exact exact, MinHash within 5 SE, SimHash bits within 5 binomial SD of the prediction; identical inputs give 1 | `TestSketchInv11_SimilarityMethodsAgree` |

Statistical invariants use bounds of at least 5 standard deviations so a
random `FF_SEED` practically never fails them.

## Tests and benchmarks

```sh
go test ./sketch/...
FF_SEED=1 FF_PROPTEST_CASES=100000 go test ./sketch/
go test -run '^$' -bench . ./sketch/
```

Conformance data: `conformance/sketch/` (`tokens.json` generated by an
independent Python implementation of the token formulas;
`vecindex_compat.json` recorded from `vecindex` before the move) and
`conformance/vecindex/{simhash,minhash}`.

## Limitations

See `OPEN_QUESTIONS.md`. Notably `SimHashIndex` is exact only up to
distance 3, while content lineage uses distance ≤ 6.
