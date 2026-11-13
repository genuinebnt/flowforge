# sketch: open questions and decisions

1. **Same algorithms as vecindex 22.8.** Everything moved from `vecindex`
   keeps its formulas, seeds and outputs (vecindex OPEN_QUESTIONS 19–20);
   pinned by `conformance/sketch/vecindex_compat.json` and
   `TestSketchConformance_Vecindex`.
2. **Generic ids.** `SimHashIndex` and `LSH` take any comparable id and a
   comparator for result order, instead of `core.UUID`. Results are sorted,
   so map iteration never reaches the output.
3. **Token-id hashing.** A token id is hashed as its 4 little-endian bytes
   (FNV-1a 64), a k-shingle as the concatenation. Ids are only comparable
   across documents from one shared interner or another stable numbering;
   sketches of independently interned documents are meaningless. Pinned by
   `conformance/sketch/tokens.json`.
4. **Fewer tokens than the shingle width** give one shingle of all of them
   (as `Shingles` did); an empty sequence gives none.
5. **Weighted SimHash** uses int32 weights and int64 accumulators, so no
   overflow below 2^32 features. Negative weights are allowed and vote
   against their bits. `CountTokens` saturates at MaxInt32.
6. **SimHash to Jaccard.** `MethodSimHash` estimates cos θ = cos(π·h/64)
   and converts with J = c/(2−c), exact only for equally sized sets; it is
   a coarse estimate (`TestSketchInv11_SimilarityMethodsAgree` states the
   bound in bits).
7. **`EstimateSimilarity` of two empty sequences is 1** for every method,
   matching `Jaccard(nil, nil)`.
8. **Lineage threshold (open).** V2_DESIGN §5 matches bundles at SimHash
   distance ≤ 6, but `SimHashIndex` is exact only to `NearDuplicateDistance`
   = 3 (four 16-bit blocks). Distance 6 needs seven blocks (about 9 bits
   each) or a scan; `content` must choose. Until then a caller with a
   handful of candidate scripts can scan with `Hamming`.
